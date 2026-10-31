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
	Epoch    string   `json:"epoch"`
	ID       string   `json:"id"`
	Changes  []change `json:"changes"`
	Source   string   `json:"source,omitempty"`
	Sequence uint64   `json:"sequence,omitempty"`
}

type masterPull struct {
	Epoch         string
	Through, Head uint64
	Transactions  []masterTransaction
}

// A regional master owns durable account/image state for its fixed users.
// The coordinator's State is only an aggregated read view of these records.
type regionalMaster struct {
	mu                 sync.RWMutex
	region             string
	durable, imageRoot string
	files              *regionalUploads
	s                  *State
	initialState       *State
	homes              *masterHomes
	journal            *os.File
	seen               map[string]bool
	epoch              string
	signer             *App
	stager             *App
	stats              *regionalImageMetrics
	sequence           uint64
	events             []masterTransaction
	nameCache          *shardedMap[string, string]
	localMetrics       [3]requestMetrics
}

func loadMasterInitial(initial, region, backingDir string) (*State, *masterHomes, error) {
	s := emptyState()
	homes := &masterHomes{users: map[string]string{}, names: map[string]string{}, images: map[[16]byte]uint8{}, posts: map[[16]byte]uint8{}}
	complete := false
	defer func() {
		if !complete {
			s.closeHistory()
		}
	}()
	for _, setBacking := range []func(string) error{s.Posts.useFileBacking, s.Reactions.useFileBacking, s.Images.useFileBacking} {
		if err := setBacking(backingDir); err != nil {
			return nil, nil, err
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
		homes.names[v.Username] = v.HomeRegion
		if v.HomeRegion == region {
			return s.apply("account", b)
		}
		return nil
	}); err != nil {
		return nil, nil, err
	}
	if err := loadLines(filepath.Join(initial, "posting-windows.jsonl.gz"), func(b []byte) error { return s.apply("window", b) }); err != nil {
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
	s.finishPostPeriods()
	complete = true
	return s, homes, nil
}

func newRegionalMaster(initial, durable, imageRoot, region string, files *regionalUploads) (*regionalMaster, error) {
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
	s, homes, err := loadMasterInitial(initial, region, durable)
	if err != nil {
		return nil, err
	}
	m := &regionalMaster{region: region, durable: durable, imageRoot: imageRoot, files: files, s: s.fork(), initialState: s, homes: homes, seen: map[string]bool{}, signer: &App{appCore: &appCore{failures: new(shardedMap[string, []time.Time]), signatures: map[string]string{}}}, nameCache: new(shardedMap[string, string])}
	m.stager = &App{appCore: &appCore{durable: filepath.Join(files.root, "pending")}}
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
		if txn.Source != "" {
			if txn.Source != region || txn.Sequence != m.sequence+1 {
				m.close()
				return nil, fmt.Errorf("invalid local transaction sequence")
			}
			m.sequence = txn.Sequence
			m.events = append(m.events, txn)
		}
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
	if m.initialState != nil {
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
		if txn.Source != "" && (txn.Source != m.region || txn.Sequence != m.sequence+1) {
			return fmt.Errorf("invalid local transaction sequence")
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
		if txn.Source != "" {
			m.sequence = txn.Sequence
			m.events = append(m.events, txn)
		}
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
	m.sequence, m.events = 0, nil
	m.nameCache = new(shardedMap[string, string])
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

func (m *regionalMaster) pull(epoch string, after uint64) (masterPull, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if epoch != m.epoch || after > m.sequence {
		return masterPull{}, fmt.Errorf("invalid mirror cursor")
	}
	through := min(after+128, m.sequence)
	return masterPull{Epoch: m.epoch, Through: through, Head: m.sequence, Transactions: append([]masterTransaction(nil), m.events[after:through]...)}, nil
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
