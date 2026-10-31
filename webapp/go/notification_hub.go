package main

import (
	"sync"
	"sync/atomic"
)

type notificationSignal struct {
	changed   chan struct{}
	listeners int
}

type notificationHub struct {
	mu       sync.Mutex
	users    map[string]*notificationSignal
	regions  map[string]*notificationSignal
	wakeups  atomic.Uint64
	targeted atomic.Uint64
}

type notificationSubscription struct {
	hub          *notificationHub
	user, region string
}

// Subscription and channel snapshots are taken while holding state locks, so
// a committed state change cannot be missed between the check and the wait.
func (h *notificationHub) subscribe(user, region string) notificationSubscription {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.users == nil {
		h.users = make(map[string]*notificationSignal)
	}
	if h.regions == nil {
		h.regions = make(map[string]*notificationSignal)
	}
	for _, group := range []struct {
		signals map[string]*notificationSignal
		key     string
	}{{h.users, user}, {h.regions, region}} {
		s := group.signals[group.key]
		if s == nil {
			s = &notificationSignal{changed: make(chan struct{})}
			group.signals[group.key] = s
		}
		s.listeners++
	}
	return notificationSubscription{h, user, region}
}
func (s notificationSubscription) channels() (<-chan struct{}, <-chan struct{}) {
	s.hub.mu.Lock()
	defer s.hub.mu.Unlock()
	return s.hub.users[s.user].changed, s.hub.regions[s.region].changed
}
func (s notificationSubscription) close() {
	s.hub.mu.Lock()
	defer s.hub.mu.Unlock()
	for _, group := range []struct {
		signals map[string]*notificationSignal
		key     string
	}{{s.hub.users, s.user}, {s.hub.regions, s.region}} {
		v := group.signals[group.key]
		v.listeners--
		if v.listeners == 0 {
			delete(group.signals, group.key)
		}
	}
}
func (h *notificationHub) signal(s *notificationSignal) {
	if s == nil {
		return
	}
	h.targeted.Add(uint64(s.listeners))
	close(s.changed)
	s.changed = make(chan struct{})
}
func (h *notificationHub) userChanged(user string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.signal(h.users[user])
}
func (h *notificationHub) regionChanged(region string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.signal(h.regions[region])
}
func (h *notificationHub) reset() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, s := range h.users {
		h.signal(s)
	}
	for _, s := range h.regions {
		h.signal(s)
	}
}
