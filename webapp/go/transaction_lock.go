package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strings"
)

func (a *App) lockSnapshot() {
	a.mu.RLock()
	for i := range a.transactions {
		a.transactions[i].RLock()
	}
	a.globalGate.RLock()
}
func (a *App) unlockSnapshot() {
	a.globalGate.RUnlock()
	for i := len(a.transactions) - 1; i >= 0; i-- {
		a.transactions[i].RUnlock()
	}
	a.mu.RUnlock()
}

// All transactions acquire logical resource stripes in the same order.
// The lifecycle lock excludes initialize. The global gate is acquired after
// stripes, so global updates wait only for executing operations, not queues.
func (a *App) lockRequest(r *http.Request) func() {
	if r.URL.Path == "/api/initialize" {
		a.mu.Lock()
		return a.mu.Unlock
	}
	if r.Method == "GET" {
		a.lockSnapshot()
		return a.unlockSnapshot
	}
	for {
		a.mu.RLock()
		keys, all := a.mutationKeys(r)
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
		for _, key := range keys {
			i := int(stateHash(key) % stateShardCount)
			if !seen[i] {
				seen[i] = true
				indices = append(indices, i)
			}
		}
		sort.Ints(indices)
		for _, i := range indices {
			a.transactions[i].Lock()
		}
		a.globalGate.RLock()
		unlock := func() {
			a.globalGate.RUnlock()
			for i := len(indices) - 1; i >= 0; i-- {
				a.transactions[indices[i]].Unlock()
			}
			a.mu.RUnlock()
		}
		if r.URL.Path == "/api/auth/login" {
			// A registration may have published the name while locks were being
			// acquired. Retry if its actual user requires an additional stripe.
			current, _ := a.mutationKeys(r)
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

func (a *App) mutationKeys(r *http.Request) ([]string, bool) {
	s, path := a.s, r.URL.Path
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	actor := s.Sessions.Get(token)
	keys := []string{"user:" + actor}
	var input struct{ Username, RecipientID, Region string }
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") || strings.HasPrefix(path, "/api/auth/") || strings.HasPrefix(path, "/api/admin/auth/") || path == "/api/friend-requests" || path == "/api/me" {
		if r.Body != nil {
			b, _ := io.ReadAll(r.Body)
			r.Body = io.NopCloser(bytes.NewReader(b))
			json.Unmarshal(b, &input)
		}
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
