package main

import (
	"net/http"
	"sort"
	"strings"
)

// Reads of one resource need only its user/post stripes. Collection responses
// retain the full coherent snapshot until their records have been captured.
func (a *App) readKeys(r *http.Request) ([]string, bool) {
	s, path := a.s, r.URL.Path
	if strings.HasPrefix(path, "/api/images/") {
		im := s.Images.get(strings.TrimPrefix(path, "/api/images/"))
		if im == nil {
			return nil, true
		}
		keys := []string{"user:" + im.OwnerID}
		switch im.Kind {
		case 'p':
			keys = append(keys, "post:"+im.Ref)
		case 'r':
			reaction := s.Reactions.get(im.Ref)
			if reaction == nil {
				return nil, true
			}
			post := s.Posts.get(reaction.PostID)
			if post == nil {
				return nil, true
			}
			keys = append(keys, "post:"+post.ID, "user:"+post.AuthorID)
		}
		return keys, false
	}
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
	if strings.HasPrefix(r.URL.Path, "/api/feeds/") {
		// Feed capture needs a coherent view of all users and posts. Drain
		// executing operations only, without joining every stripe's queue.
		a.mu.RLock()
		a.globalGate.Lock()
		return func() {
			a.globalGate.Unlock()
			a.mu.RUnlock()
		}
	}
	for {
		a.mu.RLock()
		keys, all := a.readKeys(r)
		indices := allStateStripes[:]
		var seen [stateShardCount]bool
		if !all {
			indices = nil
			for _, key := range keys {
				index := int(stateHash(key) % stateShardCount)
				if !seen[index] {
					seen[index] = true
					indices = append(indices, index)
				}
			}
			sort.Ints(indices)
		}
		a.acquireStripes(indices, false)
		a.globalGate.RLock()
		unlock := func() {
			a.globalGate.RUnlock()
			for i := len(indices) - 1; i >= 0; i-- {
				a.transactions[indices[i]].RUnlock()
			}
			a.mu.RUnlock()
		}
		if !all {
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
