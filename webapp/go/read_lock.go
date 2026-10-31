package main

import (
	"net/http"
	"strings"
)

// Reads of one resource need only its user/post locks. Collection responses
// capture coherent state together, excluding executing mutations as a cohort.
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
		uses := a.transactions.reference(keys, false)
		for i := range uses {
			uses[i].write = false
		}
		a.acquireResources(uses)
		a.globalGate.RLock()
		unlock := func() {
			a.globalGate.RUnlock()
			a.releaseResources(uses)
			a.mu.RUnlock()
		}
		// A newly linked image or changed session can require more resources than
		// the optimistic lookup observed. Resolve again before reading its state.
		current, needAll := a.readKeys(r)
		retry := needAll
		for _, key := range current {
			if !hasResource(uses, key) {
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
	uses := a.transactions.reference([]string{"user:" + uid}, false)
	uses[0].write = false
	a.acquireResources(uses)
	a.globalGate.RLock()
	return func() { a.globalGate.RUnlock(); a.releaseResources(uses); a.mu.RUnlock() }
}
