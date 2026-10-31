package main

import (
	"net/http"
	"sort"
	"strings"
)

// Reads of one resource need only its user/post stripes. Collection responses
// capture coherent state together, excluding executing mutations as a cohort.
func (a *App) readKeys(r *http.Request) ([]string, bool) {
	s, path := a.s, r.URL.Path
	actor := s.Sessions.Get(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	if actor == "" {
		return nil, true
	}
	keys := []string{"user:" + actor}
	switch path {
	case "/api/me", "/api/me/export", "/api/posting-window/current", "/api/posts/me/current", "/api/notifications":
		return keys, false
	}
	if strings.HasPrefix(path, "/api/users/") {
		return append(keys, "user:"+strings.TrimPrefix(path, "/api/users/")), false
	}
	if strings.HasPrefix(path, "/api/posts/") {
		id := strings.TrimPrefix(path, "/api/posts/")
		if !strings.Contains(id, "/") {
			if post := s.Posts.get(id); post != nil {
				return append(keys, "user:"+post.AuthorID, "post:"+post.ID), false
			}
		}
	}
	return nil, true
}

func (a *App) lockReadRequest(r *http.Request) func() {
	if strings.HasPrefix(r.URL.Path, "/api/images/") {
		// Image validity uses immutable records in concurrent stores, and is
		// checked again after signing. Only reset can retire their backing data.
		a.mu.RLock()
		return a.mu.RUnlock
	}
	for {
		a.mu.RLock()
		keys, all := a.readKeys(r)
		if all {
			a.snapshots.lock(&a.globalGate)
			return func() {
				a.snapshots.unlock(&a.globalGate)
				a.mu.RUnlock()
			}
		}
		var indices []int
		var seen [stateShardCount]bool
		for _, key := range keys {
			index := int(stateHash(key) % stateShardCount)
			if !seen[index] {
				seen[index] = true
				indices = append(indices, index)
			}
		}
		sort.Ints(indices)
		a.acquireStripeModes(indices, nil)
		a.globalGate.RLock()
		unlock := func() {
			a.globalGate.RUnlock()
			for i := len(indices) - 1; i >= 0; i-- {
				a.transactions[indices[i]].RUnlock()
			}
			a.mu.RUnlock()
		}
		// A newly linked image or changed session can require more resources than
		// the optimistic lookup observed. Resolve again before reading its state.
		current, needAll := a.readKeys(r)
		retry := needAll
		for _, key := range current {
			if !seen[stateHash(key)%stateShardCount] {
				retry = true
			}
		}
		if retry {
			unlock()
			continue
		}
		return unlock
	}
}

func (a *App) lockUserRead(uid string) func() {
	a.mu.RLock()
	stripe := &a.transactions[stateHash("user:"+uid)%stateShardCount]
	stripe.RLock()
	a.globalGate.RLock()
	return func() { a.globalGate.RUnlock(); stripe.RUnlock(); a.mu.RUnlock() }
}
