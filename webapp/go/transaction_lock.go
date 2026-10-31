package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strings"
)

// Never wait for one resource while retaining locks on unrelated resources.
// After waking, retain that resource and try the rest without blocking.
func (a *App) acquireStripeModes(indices []int, writes *[stateShardCount]bool) {
	held := -1
	for {
		var acquired [stateShardCount]int
		n := 0
		if held >= 0 {
			acquired[n] = held
			n++
		}
		blocked := -1
		for _, i := range indices {
			if i == held {
				continue
			}
			ok := false
			if writes != nil && writes[i] {
				ok = a.transactions[i].TryLock()
			} else {
				ok = a.transactions[i].TryRLock()
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
			if writes != nil && writes[acquired[n]] {
				a.transactions[acquired[n]].Unlock()
			} else {
				a.transactions[acquired[n]].RUnlock()
			}
		}
		a.stripeRetries.Add(1)
		if writes != nil && writes[blocked] {
			a.transactions[blocked].Lock()
		} else {
			a.transactions[blocked].RLock()
		}
		held = blocked
	}
}

// All transactions acquire logical resource stripes before using state.
// The lifecycle lock excludes initialize. The global gate is acquired after
// stripes, so global updates wait only for executing operations, not queues.
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
			a.transactionShards.Add(stateShardCount)
			a.transactionGlobal.Add(1)
			return func() {
				a.globalGate.Unlock()
				a.mu.RUnlock()
			}
		}
		indices := []int{}
		seen := [stateShardCount]bool{}
		writes := [stateShardCount]bool{}
		sharedUsers := interactionReadsUsers(r)
		for _, key := range keys {
			i := int(stateHash(key) % stateShardCount)
			if !sharedUsers || !strings.HasPrefix(key, "user:") {
				writes[i] = true
			}
			if !seen[i] {
				seen[i] = true
				indices = append(indices, i)
			}
		}
		sort.Ints(indices)
		a.acquireStripeModes(indices, &writes)
		a.globalGate.RLock()
		unlock := func() {
			a.globalGate.RUnlock()
			for i := len(indices) - 1; i >= 0; i-- {
				if writes[indices[i]] {
					a.transactions[indices[i]].Unlock()
				} else {
					a.transactions[indices[i]].RUnlock()
				}
			}
			a.mu.RUnlock()
		}
		if r.URL.Path == "/api/auth/login" {
			// A registration may have published the name while locks were being
			// acquired. Retry if its actual user requires an additional stripe.
			a.viewMu.RLock()
			a.s = a.appCore.s
			current, _ := a.mutationKeys(r)
			a.viewMu.RUnlock()
			retry := false
			for _, key := range current {
				if !seen[stateHash(key)%stateShardCount] {
					retry = true
				}
			}
			if retry {
				unlock()
				continue
			}
		}
		a.transactionRequests.Add(1)
		a.transactionShards.Add(uint64(len(indices)))
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

func (a *App) mutationKeys(r *http.Request) ([]string, bool) {
	s, path := a.s, r.URL.Path
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	actor := s.Sessions.Get(token)
	keys := []string{"user:" + actor}
	var input struct{ Username, RecipientID, Region, IP string }
	signatureReport := path == masterPrefix+"failure" && isInternal(r)
	if signatureReport || strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") || strings.HasPrefix(path, "/api/auth/") || strings.HasPrefix(path, "/api/admin/auth/") || path == "/api/friend-requests" || path == "/api/me" {
		if r.Body != nil {
			b, _ := io.ReadAll(r.Body)
			r.Body = io.NopCloser(bytes.NewReader(b))
			json.Unmarshal(b, &input)
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
		return []string{"name:" + input.Username, "user:" + uid, "ip:" + ipOf(r)}, false
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
		return append(keys, "user:"+input.RecipientID), false
	}
	if strings.HasPrefix(path, "/api/friend-requests/") {
		parts := strings.Split(strings.TrimPrefix(path, "/api/friend-requests/"), "/")
		if len(parts) > 0 {
			if relation := s.Requests.Get(parts[0]); relation != nil {
				return append(keys, "user:"+relation.RequesterID, "user:"+relation.RecipientID), false
			}
		}
		return nil, true
	}
	for _, prefix := range []string{"/api/friendships/", "/api/follows/", "/api/blocks/"} {
		if strings.HasPrefix(path, prefix) {
			return append(keys, "user:"+strings.TrimPrefix(path, prefix)), false
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
