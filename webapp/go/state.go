package main

import (
	"bufio"
	"compress/gzip"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
)

type Account struct {
	searchUsername string
	searchDisplay  string
	ID             string  `json:"id"`
	Username       string  `json:"username"`
	Secret         string  `json:"secretKeySha256"`
	DisplayName    string  `json:"displayName"`
	Bio            string  `json:"bio"`
	ProfileImageID *string `json:"profileImageId"`
	Region         string  `json:"region"`
	HomeRegion     string  `json:"homeRegion,omitempty"`
	AccountType    string  `json:"accountType"`
	Status         string  `json:"status"`
}
type Administrator struct {
	ID       string `json:"id"`
	Username string `json:"username"`
	Password string `json:"passwordSha256"`
}
type Window struct {
	ID        string     `json:"id"`
	Region    string     `json:"region"`
	LocalDate string     `json:"localDate"`
	StartsAt  time.Time  `json:"startsAt"`
	Deadline  time.Time  `json:"onTimeDeadline"`
	EndedAt   *time.Time `json:"endedAt,omitempty"`
}
type Location struct {
	Kind      string   `json:"kind"`
	Latitude  *float64 `json:"latitude,omitempty"`
	Longitude *float64 `json:"longitude,omitempty"`
	Locality  *string  `json:"locality,omitempty"`
}
type Post struct {
	ID         string     `json:"id"`
	AuthorID   string     `json:"authorId"`
	WindowID   string     `json:"postingWindowId"`
	ImageID    string     `json:"imageId"`
	Caption    string     `json:"caption"`
	Visibility string     `json:"visibility"`
	Location   Location   `json:"location"`
	PostedAt   time.Time  `json:"postedAt"`
	DeletedAt  *time.Time `json:"deletedAt,omitempty"`
}
type Reaction struct {
	ID         string     `json:"id"`
	PostID     string     `json:"postId"`
	ReactorID  string     `json:"reactorId"`
	ImageID    string     `json:"imageId"`
	Type       string     `json:"reactionType"`
	CreatedAt  time.Time  `json:"createdAt"`
	ReplacedAt *time.Time `json:"replacedAt,omitempty"`
	DeletedAt  *time.Time `json:"deletedAt,omitempty"`
}
type Comment struct {
	ID        string     `json:"id"`
	PostID    string     `json:"postId"`
	AuthorID  string     `json:"authorId"`
	Body      string     `json:"body"`
	CreatedAt time.Time  `json:"createdAt"`
	DeletedAt *time.Time `json:"deletedAt,omitempty"`
}
type Relation struct {
	ID          string    `json:"id"`
	RequesterID string    `json:"requesterId,omitempty"`
	RecipientID string    `json:"recipientId,omitempty"`
	User1ID     string    `json:"user1Id,omitempty"`
	User2ID     string    `json:"user2Id,omitempty"`
	FollowerID  string    `json:"followerId,omitempty"`
	FolloweeID  string    `json:"followeeId,omitempty"`
	BlockerID   string    `json:"blockerId,omitempty"`
	BlockedID   string    `json:"blockedId,omitempty"`
	Status      string    `json:"status,omitempty"`
	CreatedAt   time.Time `json:"createdAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
}
type Image struct {
	ID            string `json:"id"`
	OwnerID       string `json:"ownerId"`
	MediaType     string `json:"mediaType"`
	Part          string `json:"part"`
	Offset        int64  `json:"offset"`
	Size          int64  `json:"size"`
	Ref           string `json:"-"`
	Kind          byte   `json:"-"`
	StorageRegion string `json:"storageRegion,omitempty"`
}
type Notification struct {
	ID        string    `json:"id"`
	UserID    string    `json:"userId"`
	WindowID  string    `json:"postingWindowId"`
	CreatedAt time.Time `json:"createdAt"`
}
type change struct {
	Kind string          `json:"kind"`
	Data json.RawMessage `json:"data"`
}
type tokenChange struct{ Token, User string }
type banChange struct {
	IP    string
	Until time.Time
}
type State struct {
	dayPostCache  sync.Map
	imageLinks    *shardedMap[[16]byte, imageLink]
	LivePosts     *shardedMap[string, []string]
	Accounts      *shardedMap[string, *Account]
	Names         *shardedMap[string, string]
	Admins        *shardedMap[string, *Administrator]
	Windows       *shardedMap[string, *Window]
	Current       *shardedMap[string, string]
	Posts         *recordStore[Post]
	UserPosts     *shardedMap[string, []string]
	PostPeriods   *shardedMap[string, []datedPostRef]
	periodsReady  bool
	Reactions     *recordStore[Reaction]
	PostReactions *shardedMap[string, []string]
	Comments      *recordStore[Comment]
	PostComments  *shardedMap[string, []string]
	Images        *recordStore[Image]
	Requests      *shardedMap[string, *Relation]
	RequestOut    *shardedMap[string, map[string]*Relation]
	RequestIn     *shardedMap[string, map[string]*Relation]
	Friends       *shardedMap[string, *Relation]
	Adj           *shardedMap[string, map[string]bool]
	Follows       *shardedMap[string, *Relation]
	Blocks        *shardedMap[string, *Relation]
	Sessions      *shardedMap[string, string]
	UserTokens    *shardedMap[string, map[string]bool]
	AdminSessions *shardedMap[string, string]
	Bans          *shardedMap[string, time.Time]
	Notifications *shardedMap[string, []Notification]
}
type App struct {
	*appCore
	s             *State
	pending       []change // Owned by one request, never shared between handlers.
	prepared      bool
	signals       []stateSignal
	deferFailures bool
	failureEvents []string
}
type appCore struct {
	regionalImages       regionalImageMetrics
	mu                   sync.RWMutex
	globalGate           sync.RWMutex
	viewMu               sync.RWMutex
	master               *regionalMaster
	mastersEnabled       bool
	initialState         *State
	masterEpoch          string
	mirrored             [3]uint64
	mirror               mirrorQueue
	mirrorWaitNS         atomic.Uint64
	mirrorPulls          atomic.Uint64
	mirrorRecords        atomic.Uint64
	journalMu            sync.Mutex
	notifier             notificationHub
	transactions         [stateShardCount]sync.RWMutex
	transactionRequests  atomic.Uint64
	transactionShards    atomic.Uint64
	transactionGlobal    atomic.Uint64
	stripeRetries        atomic.Uint64
	journalWaitNS        atomic.Uint64
	journalWriteNS       atomic.Uint64
	s                    *State
	initial, durable     string
	journal              *os.File
	failures             *shardedMap[string, []time.Time]
	signatures           map[string]string
	signatureMu          sync.Mutex
	signatureFlights     map[string]*signatureFlight
	signatureOrder       []string
	signatureNext        int
	signatureCoalesced   atomic.Uint64
	signatureEvictions   atomic.Uint64
	signatureRPCFailures atomic.Uint64
	signatureHits        atomic.Uint64
	signatureMisses      atomic.Uint64
	signatureResets      atomic.Uint64
	signatureNS          atomic.Uint64
	remoteSignatures     atomic.Uint64
	historyResidentBytes atomic.Uint64
	uploadStageNS        atomic.Uint64
	uploadStageBytes     atomic.Uint64
	routeMetrics         [7]requestMetrics
}

func emptyState() *State {
	s := &State{
		imageLinks:    new(shardedMap[[16]byte, imageLink]),
		LivePosts:     new(shardedMap[string, []string]),
		Accounts:      new(shardedMap[string, *Account]),
		Names:         new(shardedMap[string, string]),
		Admins:        new(shardedMap[string, *Administrator]),
		Windows:       new(shardedMap[string, *Window]),
		Current:       new(shardedMap[string, string]),
		UserPosts:     new(shardedMap[string, []string]),
		PostPeriods:   new(shardedMap[string, []datedPostRef]),
		PostReactions: new(shardedMap[string, []string]),
		PostComments:  new(shardedMap[string, []string]),
		Requests:      new(shardedMap[string, *Relation]),
		RequestOut:    new(shardedMap[string, map[string]*Relation]),
		RequestIn:     new(shardedMap[string, map[string]*Relation]),
		Friends:       new(shardedMap[string, *Relation]),
		Adj:           new(shardedMap[string, map[string]bool]),
		Follows:       new(shardedMap[string, *Relation]),
		Blocks:        new(shardedMap[string, *Relation]),
		Sessions:      new(shardedMap[string, string]),
		UserTokens:    new(shardedMap[string, map[string]bool]),
		AdminSessions: new(shardedMap[string, string]),
		Bans:          new(shardedMap[string, time.Time]),
		Notifications: new(shardedMap[string, []Notification]),
		Posts:         newRecordStore[Post](),
		Reactions:     newRecordStore[Reaction](),
		Comments:      newRecordStore[Comment](),
		Images:        newRecordStore[Image](),
	}
	s.Images.decorate = func(id string, im *Image) {
		key, _ := uuidKey(id)
		if link, ok := s.imageLinks.Load(key); ok {
			im.Ref = uuidString(link.Ref)
			im.Kind = link.Kind
		}
	}
	return s
}
func id() string {
	var b [16]byte
	if _, e := rand.Read(b[:]); e != nil {
		panic(e)
	}
	b[6] = (b[6] & 15) | 64
	b[8] = (b[8] & 63) | 128
	x := hex.EncodeToString(b[:])
	return x[:8] + "-" + x[8:12] + "-" + x[12:16] + "-" + x[16:20] + "-" + x[20:]
}
func pair(a, b string) string {
	if a > b {
		a, b = b, a
	}
	return a + "/" + b
}
func directed(a, b string) string        { return a + "/" + b }
func (s *State) friend(a, b string) bool { return s.Adj.Get(a)[b] }
func (s *State) blocked(a, b string) bool {
	return s.Blocks.Get(directed(a, b)) != nil || s.Blocks.Get(directed(b, a)) != nil
}
func (s *State) active(a string) bool {
	return s.Accounts.Get(a) != nil && s.Accounts.Get(a).Status == "active"
}
func (s *State) apply(kind string, data []byte) error {
	decode := func(v any) {
		if e := json.Unmarshal(data, v); e != nil {
			panic(e)
		}
	}
	switch kind {
	case "account":
		var v Account
		decode(&v)
		if v.HomeRegion == "" {
			v.HomeRegion = v.Region
			if old := s.Accounts.Get(v.ID); old != nil && old.HomeRegion != "" {
				v.HomeRegion = old.HomeRegion
			}
		}
		v.searchUsername = foldSearch(v.Username)
		v.searchDisplay = foldSearch(v.DisplayName)
		s.Accounts.Set(v.ID, &v)
		s.Names.Set(v.Username, v.ID)
	case "admin":
		var v Administrator
		decode(&v)
		s.Admins.Set(v.Username, &v)
	case "window":
		var v Window
		decode(&v)
		s.Windows.Set(v.ID, &v)
		if v.EndedAt != nil {
			for uid, ids := range s.LivePosts.Snapshot() {
				keep := ids[:0]
				for _, pid := range ids {
					if s.Posts.get(pid).WindowID != v.ID {
						keep = append(keep, pid)
					}
				}
				if len(keep) == 0 {
					s.LivePosts.Delete(uid)
				} else {
					s.LivePosts.Set(uid, keep)
				}
			}
		}
		if v.EndedAt == nil {
			s.Current.Set(v.Region, v.ID)
		} else if s.Current.Get(v.Region) == v.ID {
			s.Current.Delete(v.Region)
		}
	case "post":
		var v Post
		decode(&v)
		s.dayPostCache.Delete(v.AuthorID)
		if s.Posts.get(v.ID) == nil {
			s.UserPosts.Set(v.AuthorID, append(s.UserPosts.Get(v.AuthorID), v.ID))
			s.indexPostPeriod(&v)
			if win := s.Windows.Get(v.WindowID); win != nil && win.EndedAt == nil {
				s.LivePosts.Set(v.AuthorID, append(s.LivePosts.Get(v.AuthorID), v.ID))
			}
		}
		s.Posts.set(v.ID, &v)
		s.linkImage(v.ImageID, v.ID, 'p')
	case "reaction":
		var v Reaction
		decode(&v)
		if s.Reactions.get(v.ID) == nil {
			s.PostReactions.Set(v.PostID, append(s.PostReactions.Get(v.PostID), v.ID))
		}
		s.Reactions.set(v.ID, &v)
		s.linkImage(v.ImageID, v.ID, 'r')
	case "comment":
		var v Comment
		decode(&v)
		if s.Comments.get(v.ID) == nil {
			s.PostComments.Set(v.PostID, append(s.PostComments.Get(v.PostID), v.ID))
		}
		s.Comments.set(v.ID, &v)
	case "image":
		var v Image
		decode(&v)
		s.Images.set(v.ID, &v)
	case "request":
		var v Relation
		decode(&v)
		if old := s.Requests.Get(v.ID); old != nil {
			delete(s.RequestOut.Get(old.RequesterID), old.ID)
			delete(s.RequestIn.Get(old.RecipientID), old.ID)
		}
		s.Requests.Set(v.ID, &v)
		if v.Status == "pending" {
			if s.RequestOut.Get(v.RequesterID) == nil {
				s.RequestOut.Set(v.RequesterID, map[string]*Relation{})
			}
			if s.RequestIn.Get(v.RecipientID) == nil {
				s.RequestIn.Set(v.RecipientID, map[string]*Relation{})
			}
			s.RequestOut.Get(v.RequesterID)[v.ID] = &v
			s.RequestIn.Get(v.RecipientID)[v.ID] = &v
		}
	case "friend":
		var v Relation
		decode(&v)
		s.Friends.Set(pair(v.User1ID, v.User2ID), &v)
		for _, x := range [][2]string{{v.User1ID, v.User2ID}, {v.User2ID, v.User1ID}} {
			if s.Adj.Get(x[0]) == nil {
				s.Adj.Set(x[0], map[string]bool{})
			}
			s.Adj.Get(x[0])[x[1]] = true
		}
	case "unfriend":
		var v [2]string
		decode(&v)
		s.Friends.Delete(pair(v[0], v[1]))
		delete(s.Adj.Get(v[0]), v[1])
		delete(s.Adj.Get(v[1]), v[0])
	case "follow":
		var v Relation
		decode(&v)
		s.Follows.Set(directed(v.FollowerID, v.FolloweeID), &v)
	case "unfollow":
		var v [2]string
		decode(&v)
		s.Follows.Delete(directed(v[0], v[1]))
	case "block":
		var v Relation
		decode(&v)
		s.Blocks.Set(directed(v.BlockerID, v.BlockedID), &v)
	case "unblock":
		var v [2]string
		decode(&v)
		s.Blocks.Delete(directed(v[0], v[1]))
	case "session", "adminsession":
		var v tokenChange
		decode(&v)
		m := s.Sessions
		if kind == "adminsession" {
			m = s.AdminSessions
		}
		if kind == "session" {
			if old := m.Get(v.Token); old != "" {
				delete(s.UserTokens.Get(old), v.Token)
			}
			if v.User != "" {
				tokens := s.UserTokens.Get(v.User)
				if tokens == nil {
					tokens = make(map[string]bool)
					s.UserTokens.Set(v.User, tokens)
				}
				tokens[v.Token] = true
			}
		}
		if v.User == "" {
			m.Delete(v.Token)
		} else {
			m.Set(v.Token, v.User)
		}
	case "ban":
		var v banChange
		decode(&v)
		s.Bans.Set(v.IP, v.Until)
	case "notification":
		var v Notification
		decode(&v)
		s.Notifications.Set(v.UserID, append(s.Notifications.Get(v.UserID), v))
	default:
		return fmt.Errorf("unknown journal kind %q", kind)
	}
	return nil
}
func (a *App) put(kind string, v any) {
	b, e := json.Marshal(v)
	if e != nil {
		panic(e)
	}
	if e = a.s.apply(kind, b); e != nil {
		panic(e)
	}
	a.pending = append(a.pending, change{kind, b})
}
func (a *App) commit() {
	if len(a.pending) == 0 {
		if a.prepared {
			a.s = a.appCore.s
			a.prepared = false
		}
		return
	}
	txn := masterTransaction{Epoch: a.masterEpoch, ID: id(), Changes: a.pending}
	var b []byte
	var e error
	if a.mastersEnabled {
		b, e = json.Marshal(txn)
	} else {
		b, e = json.Marshal(a.pending)
	}
	if e != nil {
		panic(e)
	}
	b = append(b, '\n')
	waiting := time.Now()
	a.journalMu.Lock()
	a.journalWaitNS.Add(uint64(time.Since(waiting)))
	writing := time.Now()
	if _, e = a.journal.Write(b); e != nil {
		log.Fatalf("journal write: %v", e)
	}
	a.journalWriteNS.Add(uint64(time.Since(writing)))
	a.journalMu.Unlock()
	if a.mastersEnabled {
		a.persistMasterTransaction(txn)
	}
	if a.prepared {
		a.viewMu.Lock()
		for _, c := range a.pending {
			if err := a.appCore.s.apply(c.Kind, c.Data); err != nil {
				panic(err)
			}
		}
		a.s = a.appCore.s
		a.viewMu.Unlock()
		a.prepared = false
	}
	a.pending = nil
	a.publishSignals()
}
func loadLines(path string, fn func([]byte) error) error {
	f, e := os.Open(path)
	if e != nil {
		return e
	}
	defer f.Close()
	g, e := gzip.NewReader(f)
	if e != nil {
		return e
	}
	defer g.Close()
	sc := bufio.NewScanner(g)
	sc.Buffer(make([]byte, 65536), 4<<20)
	for sc.Scan() {
		if e = fn(sc.Bytes()); e != nil {
			return e
		}
	}
	return sc.Err()
}
func (a *App) loadInitial() error {
	if a.initialState != nil {
		a.s = a.initialState.fork()
		a.appCore.s = a.s
		return nil
	}
	s := emptyState()
	complete := false
	defer func() {
		if !complete {
			s.closeHistory()
		}
	}()
	pool := map[string]string{}
	intern := func(x string) string {
		if y, ok := pool[x]; ok {
			return y
		}
		pool[x] = x
		return x
	}
	// Intern identifiers shared across history and image metadata.
	files := []struct{ pattern, kind string }{{"accounts.jsonl.gz", "account"}, {"administrators.jsonl.gz", "admin"}, {"posting-windows.jsonl.gz", "window"}, {"friend-requests.jsonl.gz", "request"}, {"friendships.jsonl.gz", "friend"}, {"follows.jsonl.gz", "follow"}, {"blocks.jsonl.gz", "block"}, {"images/index.part-*.jsonl.gz", "image"}, {"posts.jsonl.gz", "post"}, {"realmojis.part-*.jsonl.gz", "reaction"}, {"comments.jsonl.gz", "comment"}}
	for _, spec := range files {
		paths, e := filepath.Glob(filepath.Join(a.initial, spec.pattern))
		if e != nil || len(paths) == 0 {
			return fmt.Errorf("initial input missing: %s", spec.pattern)
		}
		for _, p := range paths {
			if e = loadLines(p, func(b []byte) error {
				switch spec.kind {
				case "post":
					var v Post
					if e := json.Unmarshal(b, &v); e != nil {
						return e
					}
					v.ID = intern(v.ID)
					v.AuthorID = intern(v.AuthorID)
					v.WindowID = intern(v.WindowID)
					v.ImageID = intern(v.ImageID)
					v.Visibility = intern(v.Visibility)
					v.Location.Kind = intern(v.Location.Kind)
					if err := s.Posts.add(v.ID, b, &v); err != nil {
						return err
					}
					s.UserPosts.Set(v.AuthorID, append(s.UserPosts.Get(v.AuthorID), v.ID))
					s.indexPostPeriod(&v)
					if win := s.Windows.Get(v.WindowID); win != nil && win.EndedAt == nil {
						s.LivePosts.Set(v.AuthorID, append(s.LivePosts.Get(v.AuthorID), v.ID))
					}
					s.linkImage(v.ImageID, v.ID, 'p')
				case "reaction":
					var v Reaction
					if e := json.Unmarshal(b, &v); e != nil {
						return e
					}
					v.ID = intern(v.ID)
					v.PostID = intern(v.PostID)
					v.ReactorID = intern(v.ReactorID)
					v.ImageID = intern(v.ImageID)
					v.Type = intern(v.Type)
					if err := s.Reactions.add(v.ID, b, &v); err != nil {
						return err
					}
					s.PostReactions.Set(v.PostID, append(s.PostReactions.Get(v.PostID), v.ID))
					s.linkImage(v.ImageID, v.ID, 'r')
				case "comment":
					var v Comment
					if e := json.Unmarshal(b, &v); e != nil {
						return e
					}
					v.ID = intern(v.ID)
					v.PostID = intern(v.PostID)
					v.AuthorID = intern(v.AuthorID)
					if err := s.Comments.add(v.ID, b, &v); err != nil {
						return err
					}
					s.PostComments.Set(v.PostID, append(s.PostComments.Get(v.PostID), v.ID))
				case "image":
					var v Image
					if e := json.Unmarshal(b, &v); e != nil {
						return e
					}
					v.ID = intern(v.ID)
					v.OwnerID = intern(v.OwnerID)
					v.Part = intern(v.Part)
					v.MediaType = intern(v.MediaType)
					if err := s.Images.add(v.ID, b, &v); err != nil {
						return err
					}
				default:
					return s.apply(spec.kind, b)
				}
				return nil
			}); e != nil {
				return fmt.Errorf("load %s: %w", p, e)
			}
			log.Printf("loaded %s", filepath.Base(p))
		}
	}
	// Historical notifications precede all future SSE connection times. They are
	// immutable input, and therefore need no live index or replay allocation.
	s.finishPostPeriods()
	complete = true
	a.s = s
	if a.mastersEnabled {
		a.initialState = s
		a.s = s.fork()
	}
	a.appCore.s = a.s
	a.historyResidentBytes.Store(uint64(len(s.Posts.data)+len(s.Reactions.data)+len(s.Comments.data)+len(s.Images.data)) * recordChunkBytes)
	return nil
}

func (a *App) closeHistory() {
	if a.initialState != nil {
		a.initialState.closeHistory()
	} else if a.appCore.s != nil {
		a.appCore.s.closeHistory()
	}
}
func newAppCore(initial, durable string) (*App, error) {
	a := &App{appCore: &appCore{initial: initial, durable: durable, failures: new(shardedMap[string, []time.Time]), signatures: map[string]string{}}}
	if e := os.MkdirAll(filepath.Join(durable, "images"), 0755); e != nil {
		return nil, e
	}
	return a, nil
}

func newApp(initial, durable string) (*App, error) {
	a, err := newAppCore(initial, durable)
	if err != nil {
		return nil, err
	}
	if e := a.loadInitial(); e != nil {
		return nil, e
	}
	p := filepath.Join(durable, "journal.jsonl")
	f, e := os.OpenFile(p, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0600)
	if e != nil {
		return nil, e
	}
	a.journal = f
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 65536), 64<<20)
	for sc.Scan() {
		var cs []change
		if e = json.Unmarshal(sc.Bytes(), &cs); e != nil {
			f.Close()
			return nil, e
		}
		for _, c := range cs {
			if e = a.s.apply(c.Kind, c.Data); e != nil {
				f.Close()
				return nil, e
			}
		}
	}
	if e = sc.Err(); e != nil {
		f.Close()
		return nil, e
	}
	log.Printf("state ready: accounts=%d posts=%d images=%d", a.s.Accounts.Len(), a.s.Posts.len(), a.s.Images.len())
	return a, nil
}
func (a *App) reset() error {
	if a.mastersEnabled {
		return a.resetRegionalMasters()
	}
	if a.s != nil {
		a.s.closeHistory()
	}
	a.s = nil
	a.appCore.s = nil
	runtime.GC()
	if e := a.loadInitial(); e != nil {
		return e
	}
	if e := a.journal.Truncate(0); e != nil {
		return e
	}
	if _, e := a.journal.Seek(0, io.SeekEnd); e != nil {
		return e
	}
	// Only initialize removes uploaded bytes. Ordinary restarts replay them.
	if e := os.RemoveAll(filepath.Join(a.durable, "images")); e != nil {
		return e
	}
	if e := os.MkdirAll(filepath.Join(a.durable, "images"), 0755); e != nil {
		return e
	}
	a.failures = new(shardedMap[string, []time.Time])
	a.pending = nil
	a.signatureMu.Lock()
	a.signatures = map[string]string{}
	a.signatureOrder = nil
	a.signatureNext = 0
	a.signatureResets.Add(1)
	a.signatureMu.Unlock()
	a.notifier.reset()
	return resetRegionalImages()
}
func (a *App) imageReader(im *Image) (*os.File, int64, error) {
	p := filepath.Join(a.initial, "images", im.Part)
	if im.Part == "" {
		p = filepath.Join(a.durable, "images", im.ID)
	}
	f, e := os.Open(p)
	if os.IsNotExist(e) && im.Part == "" && im.StorageRegion != "" {
		if e = a.fetchRegionalImage(im); e == nil {
			f, e = os.Open(p)
		}
	}
	return f, im.Offset, e
}
func (s *State) window(u *Account) *Window { return s.Windows.Get(s.Current.Get(u.Region)) }

type dayPostState struct {
	date    string
	post    *Post
	deleted int
}

func (s *State) dayPosts(u *Account) (*Post, int) {
	w := s.window(u)
	if w == nil {
		return nil, 0
	}
	if value, ok := s.dayPostCache.Load(u.ID); ok {
		cached := value.(dayPostState)
		if cached.date == w.LocalDate {
			return cached.post, cached.deleted
		}
	}
	var p *Post
	deleted := 0
	day := dateNumber(w.LocalDate)
	for _, ref := range s.postsInPeriod(u.ID, day, day+1) {
		x := s.Posts.get(ref.id)
		if x.DeletedAt != nil {
			deleted++
		} else {
			p = x
		}
	}
	s.dayPostCache.Store(u.ID, dayPostState{date: w.LocalDate, post: p, deleted: deleted})
	return p, deleted
}
func (s *State) canPost(u *Account) bool {
	w := s.window(u)
	p, d := s.dayPosts(u)
	return w != nil && w.EndedAt == nil && !time.Now().Before(w.StartsAt) && p == nil && d < 2
}
func (s *State) common(a, b string) int {
	n := 0
	for x := range s.Adj.Get(a) {
		if s.Adj.Get(b)[x] {
			n++
		}
	}
	return n
}
func (s *State) view(u *Account, p *Post) bool {
	if p == nil || p.DeletedAt != nil || !s.active(p.AuthorID) {
		return false
	}
	if p.AuthorID == u.ID {
		return true
	}
	w := s.Windows.Get(p.WindowID)
	if w == nil || w.EndedAt != nil || s.blocked(u.ID, p.AuthorID) {
		return false
	}
	own, _ := s.dayPosts(u)
	if own == nil {
		return false
	}
	return p.Visibility == "public" || s.friend(u.ID, p.AuthorID) || (p.Visibility == "friends_of_friends" && s.common(u.ID, p.AuthorID) > 0)
}
func (s *State) interact(u *Account, p *Post) bool {
	if !s.view(u, p) || s.Windows.Get(p.WindowID).EndedAt != nil {
		return false
	}
	own, _ := s.dayPosts(u)
	return own != nil && (p.AuthorID == u.ID || p.Visibility == "public" || s.friend(u.ID, p.AuthorID))
}
