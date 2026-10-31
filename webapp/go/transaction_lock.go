package main

import (
	"bytes"
	"crypto/hmac"
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
)

type transactionEntry struct {
	sync.RWMutex
	refs int
}

type transactionLocks struct {
	mu      sync.Mutex
	entries map[string]*transactionEntry
}

type resourceUse struct {
	key   string
	entry *transactionEntry
	write bool
}

// References include waiters, so an entry cannot disappear while any request
// still uses it. Idle entries are removed rather than retaining every user ID.
func (r *transactionLocks) reference(keys []string, sharedUsers bool) []resourceUse {
	sort.Strings(keys)
	uses := make([]resourceUse, 0, len(keys))
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.entries == nil {
		r.entries = map[string]*transactionEntry{}
	}
	for _, key := range keys {
		if len(uses) > 0 && uses[len(uses)-1].key == key {
			continue
		}
		entry := r.entries[key]
		if entry == nil {
			entry = &transactionEntry{}
			r.entries[key] = entry
		}
		entry.refs++
		uses = append(uses, resourceUse{key, entry, !sharedUsers || !strings.HasPrefix(key, "user:")})
	}
	return uses
}

func (r *transactionLocks) unreference(uses []resourceUse) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, use := range uses {
		use.entry.refs--
		if use.entry.refs == 0 {
			delete(r.entries, use.key)
		}
	}
}

func (use resourceUse) unlock() {
	if use.write {
		use.entry.Unlock()
	} else {
		use.entry.RUnlock()
	}
}

// Never retain an unrelated resource while waiting for a busy one. The
// resource key, rather than a fixed hash bucket, determines mutual exclusion.
func (a *App) acquireResources(uses []resourceUse) {
	held := -1
	acquired := make([]int, len(uses))
	for {
		n := 0
		if held >= 0 {
			acquired[n] = held
			n++
		}
		blocked := -1
		for i, use := range uses {
			if i == held {
				continue
			}
			ok := false
			if use.write {
				ok = use.entry.TryLock()
			} else {
				ok = use.entry.TryRLock()
			}
			if !ok {
				blocked = i
				break
			}
			acquired[n] = i
			n++
		}
		if blocked < 0 {
			return
		}
		for n > 0 {
			n--
			uses[acquired[n]].unlock()
		}
		a.lockRetries.Add(1)
		if uses[blocked].write {
			uses[blocked].entry.Lock()
		} else {
			uses[blocked].entry.RLock()
		}
		held = blocked
	}
}

func (a *App) releaseResources(uses []resourceUse) {
	for i := len(uses) - 1; i >= 0; i-- {
		uses[i].unlock()
	}
	a.transactions.unreference(uses)
}

// The lifecycle lock excludes initialize. The global gate is acquired after
// resources, so global updates wait for executing operations, not their queues.
func (a *App) lockRequest(r *http.Request) func() {
	if r.URL.Path == "/api/initialize" {
		a.mu.Lock()
		a.s = a.appCore.s
		return a.mu.Unlock
	}
	if r.Method == "GET" {
		return a.lockReadView()
	}
	for {
		a.mu.RLock()
		a.viewMu.RLock()
		a.s = a.appCore.s
		keys, all := a.mutationKeys(r)
		a.viewMu.RUnlock()
		if all {
			a.globalGate.Lock()
			a.transactionRequests.Add(1)
			a.transactionGlobal.Add(1)
			return func() { a.globalGate.Unlock(); a.mu.RUnlock() }
		}
		uses := a.transactions.reference(keys, interactionReadsUsers(r) || relationshipMutation(r))
		a.acquireResources(uses)
		a.globalGate.RLock()
		unlock := func() { a.globalGate.RUnlock(); a.releaseResources(uses); a.mu.RUnlock() }
		if r.URL.Path == "/api/auth/login" {
			// A registration may publish the name before its resource is acquired.
			a.viewMu.RLock()
			a.s = a.appCore.s
			current, _ := a.mutationKeys(r)
			a.viewMu.RUnlock()
			retry := false
			for _, key := range current {
				found := false
				for _, use := range uses {
					if use.key == key {
						found = true
						break
					}
				}
				if !found {
					retry = true
					break
				}
			}
			if retry {
				unlock()
				continue
			}
		}
		a.transactionRequests.Add(1)
		a.transactionKeys.Add(uint64(len(uses)))
		return unlock
	}
}

// Interaction mutations change the post and its image records, while user
// records and relationships are only consulted for authorization.
func interactionReadsUsers(r *http.Request) bool {
	p := r.URL.Path
	if r.Method == "POST" && strings.HasPrefix(p, "/api/posts/") {
		return strings.HasSuffix(p, "/comments") || strings.HasSuffix(p, "/realmojis")
	}
	return r.Method == "DELETE" && (strings.HasPrefix(p, "/api/comments/") || strings.HasPrefix(p, "/api/realmojis/"))
}

func relationshipMutation(r *http.Request) bool {
	p := r.URL.Path
	return p == "/api/friend-requests" || strings.HasPrefix(p, "/api/friend-requests/") || strings.HasPrefix(p, "/api/friendships/") || strings.HasPrefix(p, "/api/follows/") || strings.HasPrefix(p, "/api/blocks/")
}

func (a *App) mutationKeys(r *http.Request) ([]string, bool) {
	s, path := a.s, r.URL.Path
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	actor := s.Sessions.Get(token)
	keys := []string{"user:" + actor}
	var input struct{ Username, SecretKey, RecipientID, Region, IP string }
	signatureReport := path == masterPrefix+"failure" && isInternal(r)
	if signatureReport || strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") || strings.HasPrefix(path, "/api/auth/") || strings.HasPrefix(path, "/api/admin/auth/") || path == "/api/friend-requests" || path == "/api/me" {
		if r.Body != nil {
			b, _ := io.ReadAll(r.Body)
			r.Body = io.NopCloser(bytes.NewReader(b))
			json.NewDecoder(bytes.NewReader(b)).Decode(&input)
		}
	}
	if signatureReport && input.IP != "" {
		return []string{"ip:" + input.IP}, false
	}
	if path == "/api/auth/register" {
		return []string{"name:" + input.Username}, false
	}
	if path == "/api/auth/login" {
		uid := s.Names.Get(input.Username)
		keys := []string{"name:" + input.Username, "user:" + uid}
		user := s.Accounts.Get(uid)
		if !usernameRE.MatchString(input.Username) || user == nil || user.Status != "active" || !hmac.Equal([]byte(user.Secret), []byte(secretHash(input.SecretKey))) {
			keys = append(keys, "ip:"+ipOf(r))
		}
		return keys, false
	}
	if strings.HasPrefix(path, "/api/admin/") {
		return nil, true
	}
	if actor == "" {
		return nil, true
	}
	if path == "/api/me/profile-image" {
		return append(keys, "ip:"+ipOf(r)), false
	}
	if path == "/api/me" || path == "/api/me/account-deletion" || path == "/api/auth/logout" {
		return keys, false
	}
	if path == "/api/posts" {
		return keys, false
	}
	if path == "/api/friend-requests" {
		return append(keys, "user:"+input.RecipientID, "relation:"+pair(actor, input.RecipientID)), false
	}
	if strings.HasPrefix(path, "/api/friend-requests/") {
		parts := strings.Split(strings.TrimPrefix(path, "/api/friend-requests/"), "/")
		if len(parts) > 0 {
			if relation := s.Requests.Get(parts[0]); relation != nil {
				return append(keys, "user:"+relation.RequesterID, "user:"+relation.RecipientID, "relation:"+pair(relation.RequesterID, relation.RecipientID)), false
			}
		}
		return nil, true
	}
	for _, prefix := range []string{"/api/friendships/", "/api/follows/", "/api/blocks/"} {
		if strings.HasPrefix(path, prefix) {
			other := strings.TrimPrefix(path, prefix)
			return append(keys, "user:"+other, "relation:"+pair(actor, other)), false
		}
	}
	pid := ""
	if strings.HasPrefix(path, "/api/posts/") {
		pid = strings.Split(strings.TrimPrefix(path, "/api/posts/"), "/")[0]
	}
	if strings.HasPrefix(path, "/api/comments/") {
		if c := s.Comments.get(strings.TrimPrefix(path, "/api/comments/")); c != nil {
			pid = c.PostID
			keys = append(keys, "user:"+c.AuthorID)
		}
	}
	if strings.HasPrefix(path, "/api/realmojis/") {
		if v := s.Reactions.get(strings.TrimPrefix(path, "/api/realmojis/")); v != nil {
			pid = v.PostID
			keys = append(keys, "user:"+v.ReactorID)
		}
	}
	if pid != "" {
		if p := s.Posts.get(pid); p != nil {
			return append(keys, "post:"+pid, "user:"+p.AuthorID), false
		}
	}
	return nil, true
}
