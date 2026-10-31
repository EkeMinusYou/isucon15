package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	"image/jpeg"
	"mime/multipart"
	"net/http/httptest"
	"runtime"
	"sync"
	"testing"
	"time"
)

type concurrentRequest struct {
	method, path, token, contentType string
	body                             []byte
}

func runTogether(a *App, requests []concurrentRequest) []*httptest.ResponseRecorder {
	results := make([]*httptest.ResponseRecorder, len(requests))
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i, q := range requests {
		wg.Add(1)
		go func(i int, q concurrentRequest) {
			defer wg.Done()
			<-start
			r := httptest.NewRequest(q.method, q.path, bytes.NewReader(q.body))
			r.Header.Set("Authorization", "Bearer "+q.token)
			r.Header.Set("Content-Type", q.contentType)
			w := httptest.NewRecorder()
			a.ServeHTTP(w, r)
			results[i] = w
		}(i, q)
	}
	close(start)
	wg.Wait()
	return results
}
func imageRequest(path, token string, fields map[string]string) concurrentRequest {
	var b bytes.Buffer
	mw := multipart.NewWriter(&b)
	f, _ := mw.CreateFormFile("image", "image.jpg")
	jpeg.Encode(f, image.NewRGBA(image.Rect(0, 0, 2, 2)), nil)
	for k, v := range fields {
		mw.WriteField(k, v)
	}
	mw.Close()
	return concurrentRequest{"POST", path, token, mw.FormDataContentType(), b.Bytes()}
}
func openWindow(t *testing.T, a *App, date string) string {
	t.Helper()
	admin := requireCall(t, a, "POST", "/api/admin/auth/login", "", object{"username": "admin", "password": "password"}, 200)["token"].(string)
	requireCall(t, a, "POST", "/api/admin/posting-window", admin, object{"region": "asia", "localDate": date}, 201)
	return admin
}
func TestConcurrentPostSlotAndReplay(t *testing.T) {
	a := fixtureApp(t)
	uid, token := register(t, a, "post_race")
	openWindow(t, a, "2026-11-01")
	q := imageRequest("/api/posts", token, map[string]string{"visibility": "public", "location_kind": "none"})
	qs := make([]concurrentRequest, 8)
	for i := range qs {
		qs[i] = q
	}
	accepted := 0
	for _, w := range runTogether(a, qs) {
		if w.Code == 201 {
			accepted++
		} else if w.Code != 409 {
			t.Fatalf("unexpected status %d: %s", w.Code, w.Body.String())
		}
	}
	if accepted != 1 || len(a.s.UserPosts.Get(uid)) != 1 {
		t.Fatalf("accepted=%d", accepted)
	}
	restored, err := newApp(a.initial, a.durable)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.journal.Close()
	defer restored.s.closeHistory()
	if len(restored.s.UserPosts.Get(uid)) != 1 {
		t.Fatal("post lost after replay")
	}
}

func TestConcurrentIndependentUpdatesAndReplay(t *testing.T) {
	a := fixtureApp(t)
	openWindow(t, a, "2026-11-01")
	uids := []string{}
	requests := []concurrentRequest{}
	for i := 0; i < 16; i++ {
		uid, token := register(t, a, fmt.Sprintf("independent_%d", i))
		uids = append(uids, uid)
		requests = append(requests, imageRequest("/api/posts", token, map[string]string{"visibility": "public", "location_kind": "none"}))
	}
	for _, w := range runTogether(a, requests) {
		if w.Code != 201 {
			t.Fatalf("independent write: %d %s", w.Code, w.Body.String())
		}
	}
	restored, err := newApp(a.initial, a.durable)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.journal.Close()
	defer restored.s.closeHistory()
	for _, uid := range uids {
		if len(restored.s.UserPosts.Get(uid)) != 1 {
			t.Fatal("independent transaction lost during replay")
		}
	}
}

func TestConcurrentUsernameRegistration(t *testing.T) {
	a := fixtureApp(t)
	b, _ := json.Marshal(object{"username": "same_username", "displayName": "same", "region": "asia"})
	requests := make([]concurrentRequest, 8)
	for i := range requests {
		requests[i] = concurrentRequest{"POST", "/api/auth/register", "", "application/json", b}
	}
	created := 0
	for _, w := range runTogether(a, requests) {
		if w.Code == 201 {
			created++
		} else if w.Code != 409 {
			t.Fatalf("registration: %d", w.Code)
		}
	}
	if created != 1 {
		t.Fatalf("created %d accounts with one name", created)
	}
}
func TestConcurrentReactionReplacement(t *testing.T) {
	a := fixtureApp(t)
	_, owner := register(t, a, "reaction_owner")
	actor, token := register(t, a, "reaction_actor")
	openWindow(t, a, "2026-11-01")
	post := upload(t, a, "/api/posts", owner, map[string]string{"visibility": "public", "location_kind": "none"}, 201)
	upload(t, a, "/api/posts", token, map[string]string{"visibility": "public", "location_kind": "none"}, 201)
	pid := post["id"].(string)
	q := imageRequest("/api/posts/"+pid+"/realmojis", token, map[string]string{"reactionType": "like"})
	qs := make([]concurrentRequest, 8)
	for i := range qs {
		qs[i] = q
	}
	for _, w := range runTogether(a, qs) {
		if w.Code != 201 {
			t.Fatalf("reaction: %d %s", w.Code, w.Body.String())
		}
	}
	current := 0
	for _, id := range a.s.PostReactions.Get(pid) {
		v := a.s.Reactions.get(id)
		if v.ReactorID == actor && v.ReplacedAt == nil && v.DeletedAt == nil {
			current++
		}
	}
	if current != 1 {
		t.Fatalf("current reactions=%d", current)
	}
}
func TestAcceptAndBlockAreAtomic(t *testing.T) {
	a := fixtureApp(t)
	aid, at := register(t, a, "accept_actor")
	bid, bt := register(t, a, "block_actor")
	v := requireCall(t, a, "POST", "/api/friend-requests", at, object{"recipientId": bid}, 201)
	results := runTogether(a, []concurrentRequest{{"POST", "/api/friend-requests/" + v["relationId"].(string) + "/accept", bt, "application/json", nil}, {"PUT", "/api/blocks/" + bid, at, "application/json", nil}})
	if results[0].Code != 200 && results[0].Code != 403 {
		t.Fatalf("accept=%d", results[0].Code)
	}
	if results[1].Code != 204 || a.s.friend(aid, bid) || !a.s.blocked(aid, bid) {
		t.Fatal("block exposed an inconsistent friendship")
	}
}
func TestPostingWindowSwitchAndPostAreAtomic(t *testing.T) {
	a := fixtureApp(t)
	uid, token := register(t, a, "window_race")
	admin := openWindow(t, a, "2026-11-01")
	q := imageRequest("/api/posts", token, map[string]string{"visibility": "public", "location_kind": "none"})
	b, _ := json.Marshal(object{"region": "asia", "localDate": "2026-11-02"})
	for _, w := range runTogether(a, []concurrentRequest{q, {"POST", "/api/admin/posting-window", admin, "application/json", b}}) {
		if w.Code != 201 {
			t.Fatalf("window/post=%d %s", w.Code, w.Body.String())
		}
	}
	ids := a.s.UserPosts.Get(uid)
	if len(ids) != 1 {
		t.Fatal("lost post")
	}
	p := a.s.Posts.get(ids[0])
	current := a.s.window(a.s.Accounts.Get(uid))
	if a.s.canPost(a.s.Accounts.Get(uid)) != (p.WindowID != current.ID) {
		t.Fatal("posting slot does not match the current window")
	}
}

// A queued per-user update must not delay opening the next posting window.
func TestPostingWindowDoesNotDrainQueuedUpdates(t *testing.T) {
	a := fixtureApp(t)
	uid, token := register(t, a, "queued_update")
	admin := openWindow(t, a, "2026-11-01")
	stripe := &a.transactions[stateHash("user:"+uid)%stateShardCount]
	stripe.Lock()
	held := true
	defer func() {
		if held {
			stripe.Unlock()
		}
	}()
	updated := make(chan int, 1)
	go func() {
		status, _, _ := call(t, a, "PATCH", "/api/me", token, object{"bio": "queued"})
		updated <- status
	}()
	deadline := time.Now().Add(time.Second)
	for a.mu.TryLock() {
		a.mu.Unlock()
		if time.Now().After(deadline) {
			t.Fatal("update did not enter the queue")
		}
		runtime.Gosched()
	}
	opened := make(chan int, 1)
	go func() {
		status, _, _ := call(t, a, "POST", "/api/admin/posting-window", admin, object{"region": "asia", "localDate": "2026-11-02"})
		opened <- status
	}()
	select {
	case status := <-opened:
		if status != 201 {
			t.Fatalf("window returned %d", status)
		}
	case <-time.After(time.Second):
		stripe.Unlock()
		held = false
		<-updated
		<-opened
		t.Fatal("posting window waited for a queued update")
	}
	stripe.Unlock()
	held = false
	if status := <-updated; status != 200 {
		t.Fatalf("update returned %d", status)
	}
}

func TestWaitingForOneStripeReleasesOtherStripes(t *testing.T) {
	for _, exclusive := range []bool{false, true} {
		t.Run(fmt.Sprintf("exclusive=%v", exclusive), func(t *testing.T) {
			a := &App{appCore: &appCore{}}
			a.transactions[1].Lock()
			acquired, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
			go func() {
				a.acquireStripes([]int{0, 1}, exclusive)
				close(acquired)
				<-release
				if exclusive {
					a.transactions[1].Unlock()
					a.transactions[0].Unlock()
				} else {
					a.transactions[1].RUnlock()
					a.transactions[0].RUnlock()
				}
				close(finished)
			}()
			deadline := time.Now().Add(time.Second)
			for a.stripeRetries.Load() == 0 && time.Now().Before(deadline) {
				runtime.Gosched()
			}
			if a.stripeRetries.Load() == 0 {
				t.Error("request did not wait for the busy stripe")
			}
			if a.transactions[0].TryLock() {
				a.transactions[0].Unlock()
			} else {
				t.Error("waiting request retained an unrelated stripe")
			}
			a.transactions[1].Unlock()
			<-acquired
			close(release)
			<-finished
		})
	}
}
