package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

type masterTransaction struct {
	Epoch   string   `json:"epoch"`
	ID      string   `json:"id"`
	Changes []change `json:"changes"`
}

// A regional master owns durable account/image state for its fixed users.
// The coordinator's State is only an aggregated read view of these records.
type regionalMaster struct {
	mu                          sync.RWMutex
	region                      string
	initial, durable, imageRoot string
	files                       *regionalUploads
	s                           *State
	homes                       *masterHomes
	journal                     *os.File
	seen                        map[string]bool
	epoch                       string
	signer                      *App
	stats                       *regionalImageMetrics
}

func loadMasterInitial(initial, region string) (*State, *masterHomes, error) {
	s := emptyState()
	homes := &masterHomes{users: map[string]string{}, images: map[[16]byte]uint8{}, posts: map[[16]byte]uint8{}}
	complete := false
	defer func() {
		if !complete {
			s.closeHistory()
		}
	}()
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
		if v.HomeRegion == region {
			return s.apply("account", b)
		}
		return nil
	}); err != nil {
		return nil, nil, err
	}
	for _, spec := range []struct{ pattern, kind string }{{"images/index.part-*.jsonl.gz", "image"}, {"posts.jsonl.gz", "post"}, {"realmojis.part-*.jsonl.gz", "reaction"}} {
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
					if home == region {
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
					if home == region {
						if err := s.Posts.add(v.ID, b, &v); err != nil {
							return err
						}
						s.linkImage(v.ImageID, v.ID, 'p')
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
	complete = true
	return s, homes, nil
}

func newRegionalMaster(initial, durable, imageRoot, region string, files *regionalUploads) (*regionalMaster, error) {
	if err := os.MkdirAll(durable, 0700); err != nil {
		return nil, err
	}
	s, homes, err := loadMasterInitial(initial, region)
	if err != nil {
		return nil, err
	}
	m := &regionalMaster{region: region, initial: initial, durable: durable, imageRoot: imageRoot, files: files, s: s, homes: homes, seen: map[string]bool{}, signer: &App{appCore: &appCore{}}}
	if b, readErr := os.ReadFile(filepath.Join(durable, "epoch")); readErr == nil {
		m.epoch = strings.TrimSpace(string(b))
	} else if !os.IsNotExist(readErr) {
		s.closeHistory()
		return nil, readErr
	}
	m.journal, err = os.OpenFile(filepath.Join(durable, "master.jsonl"), os.O_CREATE|os.O_RDWR|os.O_APPEND, 0600)
	if err != nil {
		s.closeHistory()
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
	if m.s != nil {
		m.s.closeHistory()
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
	case "session":
		var v tokenChange
		if err := json.Unmarshal(c.Data, &v); err != nil {
			return err
		}
		if v.User == "" {
			if homeOfNewID(v.Token) != m.region {
				return fmt.Errorf("session belongs to another master")
			}
			return nil
		}
		uid = v.User
	case "ban":
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
	// Initialize may briefly block reads. Release the previous mapped history
	// before rebuilding it so both generations never consume memory together.
	m.s.closeHistory()
	m.s = emptyState()
	runtime.GC()
	s, _, err := loadMasterInitial(m.initial, m.region)
	if err != nil {
		return err
	}
	if err := m.journal.Truncate(0); err != nil {
		s.closeHistory()
		return err
	}
	m.s, m.seen = s, map[string]bool{}
	// Truncate metadata before removing originals, so a restart never replays
	// an old reference after its original has been removed by initialization.
	m.files.mu.Lock()
	defer m.files.mu.Unlock()
	if err := os.RemoveAll(m.files.acceptedDir()); err != nil {
		return err
	}
	if err := os.MkdirAll(m.files.acceptedDir(), 0700); err != nil {
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
