package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type masterTransaction struct {
	Epoch   string   `json:"epoch"`
	ID      string   `json:"id"`
	Changes []change `json:"changes"`
}

// Regional image servers retain the records needed to validate their fixed
// users' images. The coordinator publishes mutations after this copy is durable.
type regionalMaster struct {
	mu                 sync.RWMutex
	region             string
	durable, imageRoot string
	files              *regionalUploads
	s                  *State
	initialState       *State
	sharedInitial      bool
	homes              *masterHomes
	journal            *os.File
	seen               map[string]bool
	epoch              string
	signer             *App
	stats              *regionalImageMetrics
}

func loadMasterInitial(initial, region, backingDir string, base *State) (*State, *masterHomes, error) {
	s := base
	if s == nil {
		s = emptyState()
	}
	homes := &masterHomes{users: map[string]string{}, images: map[[16]byte]uint8{}, posts: map[[16]byte]uint8{}}
	complete := false
	defer func() {
		if !complete && base == nil {
			s.closeHistory()
		}
	}()
	if base == nil {
		for _, setBacking := range []func(string) error{s.Posts.useFileBacking, s.Reactions.useFileBacking, s.Images.useFileBacking} {
			if err := setBacking(backingDir); err != nil {
				return nil, nil, err
			}
		}
	}
	if err := loadLines(filepath.Join(initial, "accounts.jsonl.gz"), func(b []byte) error {
		var v Account
		if err := json.Unmarshal(b, &v); err != nil {
			return err
		}
		if v.HomeRegion == "" {
			v.HomeRegion = v.Region
		}
		if !validRegion(v.HomeRegion) {
			return fmt.Errorf("invalid account home")
		}
		homes.users[v.ID] = v.HomeRegion
		if base == nil && v.HomeRegion == region {
			return s.apply("account", b)
		}
		return nil
	}); err != nil {
		return nil, nil, err
	}
	if base == nil {
		if err := loadLines(filepath.Join(initial, "posting-windows.jsonl.gz"), func(b []byte) error { return s.apply("window", b) }); err != nil {
			return nil, nil, err
		}
	}
	for _, spec := range []struct{ pattern, kind string }{{"images/index.part-*.jsonl.gz", "image"}, {"posts.jsonl.gz", "post"}, {"realmojis.part-*.jsonl.gz", "reaction"}} {
		if base != nil && spec.kind == "reaction" {
			continue
		}
		paths, err := filepath.Glob(filepath.Join(initial, spec.pattern))
		if err != nil || len(paths) == 0 {
			return nil, nil, fmt.Errorf("master initial input missing: %s", spec.pattern)
		}
		for _, path := range paths {
			if err := loadLines(path, func(b []byte) error {
				switch spec.kind {
				case "image":
					var v Image
					if err := json.Unmarshal(b, &v); err != nil {
						return err
					}
					home := homes.user(v.OwnerID)
					if key, ok := uuidKey(v.ID); ok {
						homes.images[key] = masterRegionCode(home)
					}
					if base == nil && home == region {
						return s.Images.add(v.ID, b, &v)
					}
				case "post":
					var v Post
					if err := json.Unmarshal(b, &v); err != nil {
						return err
					}
					home := homes.user(v.AuthorID)
					if key, ok := uuidKey(v.ID); ok {
						homes.posts[key] = masterRegionCode(home)
					}
					if base == nil && home == region {
						if err := s.Posts.add(v.ID, b, &v); err != nil {
							return err
						}
						s.linkImage(v.ImageID, v.ID, 'p')
						s.UserPosts.Set(v.AuthorID, append(s.UserPosts.Get(v.AuthorID), v.ID))
						s.indexPostPeriod(&v)
						if win := s.Windows.Get(v.WindowID); win != nil && win.EndedAt == nil {
							s.LivePosts.Set(v.AuthorID, append(s.LivePosts.Get(v.AuthorID), v.ID))
						}
					}
				case "reaction":
					var v Reaction
					if err := json.Unmarshal(b, &v); err != nil {
						return err
					}
					if homes.user(v.ReactorID) == region {
						if err := s.Reactions.add(v.ID, b, &v); err != nil {
							return err
						}
						s.linkImage(v.ImageID, v.ID, 'r')
					}
				}
				return nil
			}); err != nil {
				return nil, nil, err
			}
		}
	}
	if base == nil {
		s.finishPostPeriods()
	}
	complete = true
	return s, homes, nil
}

func newRegionalMaster(initial, durable, imageRoot, region string, files *regionalUploads) (*regionalMaster, error) {
	return openRegionalMaster(initial, durable, imageRoot, region, files, nil)
}

// A colocated coordinator and master can share immutable input and keep their
// live overlays independent. Only the coordinator owns that base's lifetime.
func openRegionalMaster(initial, durable, imageRoot, region string, files *regionalUploads, base *State) (*regionalMaster, error) {
	files.region = region
	if files.active == nil {
		files.active = map[string]bool{}
	}
	for _, dir := range []string{files.acceptedDir(), files.pendingDir()} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			return nil, err
		}
	}
	if err := os.MkdirAll(durable, 0700); err != nil {
		return nil, err
	}
	s, homes, err := loadMasterInitial(initial, region, durable, base)
	if err != nil {
		return nil, err
	}
	m := &regionalMaster{region: region, durable: durable, imageRoot: imageRoot, files: files, s: s.fork(), initialState: s, sharedInitial: base != nil, homes: homes, seen: map[string]bool{}, signer: &App{appCore: &appCore{failures: new(shardedMap[string, []time.Time]), signatures: map[string]string{}}}}
	if b, readErr := os.ReadFile(filepath.Join(durable, "epoch")); readErr == nil {
		m.epoch = strings.TrimSpace(string(b))
	} else if !os.IsNotExist(readErr) {
		if base == nil {
			s.closeHistory()
		}
		return nil, readErr
	}
	m.journal, err = os.OpenFile(filepath.Join(durable, "master.jsonl"), os.O_CREATE|os.O_RDWR|os.O_APPEND, 0600)
	if err != nil {
		if base == nil {
			s.closeHistory()
		}
		return nil, err
	}
	scan := bufio.NewScanner(m.journal)
	scan.Buffer(make([]byte, 65536), 32<<20)
	for scan.Scan() {
		var txn masterTransaction
		if err := json.Unmarshal(scan.Bytes(), &txn); err != nil {
			m.close()
			return nil, err
		}
		if txn.Epoch != m.epoch {
			m.close()
			return nil, fmt.Errorf("master journal epoch mismatch")
		}
		for _, c := range txn.Changes {
			if err := m.s.apply(c.Kind, c.Data); err != nil {
				m.close()
				return nil, err
			}
		}
		m.seen[txn.ID] = true
	}
	if err := scan.Err(); err != nil {
		m.close()
		return nil, err
	}
	return m, nil
}

func (m *regionalMaster) close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.journal != nil {
		m.journal.Close()
	}
	if m.initialState != nil && !m.sharedInitial {
		m.initialState.closeHistory()
	}
}

func (m *regionalMaster) validate(c change) error {
	var uid string
	switch c.Kind {
	case "account":
		var v Account
		if err := json.Unmarshal(c.Data, &v); err != nil {
			return err
		}
		uid = v.ID
		if v.HomeRegion != m.region {
			return fmt.Errorf("account assigned to another master")
		}
		if old := m.s.Accounts.Get(uid); old != nil && old.HomeRegion != v.HomeRegion {
			return fmt.Errorf("account home changed")
		}
	case "post":
		var v Post
		if err := json.Unmarshal(c.Data, &v); err != nil {
			return err
		}
		uid = v.AuthorID
		if indexedHome(m.homes.posts, v.ID) != m.region {
			return fmt.Errorf("post identifier belongs to another master")
		}
	case "reaction":
		var v Reaction
		if err := json.Unmarshal(c.Data, &v); err != nil {
			return err
		}
		uid = v.ReactorID
	case "image":
		var v Image
		if err := json.Unmarshal(c.Data, &v); err != nil {
			return err
		}
		uid = v.OwnerID
		if indexedHome(m.homes.images, v.ID) != m.region {
			return fmt.Errorf("image identifier belongs to another master")
		}
		if v.Part == "" {
			f, err := m.files.open(v.ID)
			if err != nil {
				return fmt.Errorf("original missing at its master: %w", err)
			}
			info, err := f.Stat()
			f.Close()
			if err != nil || info.Size() != v.Size {
				return fmt.Errorf("original size differs at its master")
			}
		}
	case "ban", "window":
		return nil
	default:
		return fmt.Errorf("record kind %q is not master-owned", c.Kind)
	}
	if m.homes.user(uid) != m.region {
		return fmt.Errorf("record belongs to another user master")
	}
	return nil
}

func (m *regionalMaster) commit(txns []masterTransaction) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.commitLocked(txns)
}

func (m *regionalMaster) commitLocked(txns []masterTransaction) error {
	for _, txn := range txns {
		if txn.Epoch != m.epoch {
			return fmt.Errorf("master epoch mismatch")
		}
		if m.seen[txn.ID] {
			continue
		}
		if txn.ID == "" {
			return fmt.Errorf("missing transaction identifier")
		}
		for _, c := range txn.Changes {
			if err := m.validate(c); err != nil {
				return err
			}
		}
		b, err := json.Marshal(txn)
		if err != nil {
			return err
		}
		if _, err := m.journal.Write(append(b, '\n')); err != nil {
			return err
		}
		for _, c := range txn.Changes {
			if err := m.s.apply(c.Kind, c.Data); err != nil {
				return err
			}
		}
		m.seen[txn.ID] = true
	}
	return nil
}

func (m *regionalMaster) reset(epoch string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if epoch != "" && m.epoch == epoch {
		return nil
	}
	if err := m.journal.Truncate(0); err != nil {
		return err
	}
	m.s, m.seen = m.initialState.fork(), map[string]bool{}
	// Truncate metadata before removing originals, so a restart never replays
	// an old reference after its original has been removed by initialization.
	if err := m.files.reset(); err != nil {
		return err
	}
	if err := writeTextAtomic(filepath.Join(m.durable, "epoch"), epoch); err != nil {
		return err
	}
	m.epoch = epoch
	return nil
}

func writeTextAtomic(path, value string) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".epoch-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, err = f.WriteString(value)
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(f.Name(), path)
}
