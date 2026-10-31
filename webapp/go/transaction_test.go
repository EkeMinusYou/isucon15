package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	"image/jpeg"
	"mime/multipart"
	"net/http"
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

func testResource(t *testing.T, a *App, key string) *transactionEntry {
	t.Helper()
	uses := a.transactions.reference([]string{key}, false)
	t.Cleanup(func() { a.transactions.unreference(uses) })
	return uses[0].entry
}

func TestUnrelatedUsersDoNotShareTransactionLock(t *testing.T) {
	a := fixtureApp(t)
	first := id()
	second := id()
	for second == first || stateHash("user:"+second)%stateShardCount != stateHash("user:"+first)%stateShardCount {
		second = id()
	}
	request := func(user string) *http.Request {
		a.s.Sessions.Set(user, user)
		r := httptest.NewRequest("PATCH", "/api/me", nil)
		r.Header.Set("Authorization", "Bearer "+user)
		return r
	}
	firstRequest, secondRequest := request(first), request(second)
	unlock := a.lockRequest(firstRequest)
	held := true
	defer func() {
		if held {
			unlock()
		}
	}()
	done := make(chan struct{})
	go func() {
		release := a.lockRequest(secondRequest)
		release()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		unlock()
		held = false
		<-done
		t.Fatal("unrelated users shared a transaction lock")
	}
	unlock()
	held = false
	a.transactions.mu.Lock()
	defer a.transactions.mu.Unlock()
	if len(a.transactions.entries) != 0 {
		t.Fatal("finished requests retained resource locks")
	}
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

func TestLoginLocksFollowRequestDecoder(t *testing.T) {
	a := fixtureApp(t)
	registered := requireCall(t, a, "POST", "/api/auth/register", "", object{"username": "login_decoder", "displayName": "Decoder", "region": "asia"}, 201)
	uid := registered["user"].(map[string]any)["id"].(string)
	b, _ := json.Marshal(object{"username": "login_decoder", "secretKey": registered["secretKey"]})
	b = append(b, []byte(" {}")...)
	r := httptest.NewRequest("POST", "/api/auth/login", bytes.NewReader(b))
	keys, _ := a.mutationKeys(r)
	found := false
	for _, key := range keys {
		found = found || key == "user:"+uid
	}
	if !found {
		t.Fatal("login did not lock the user accepted by the request decoder")
	}
	var input struct{ Username string }
	if !decode(r, &input) || input.Username != "login_decoder" {
		t.Fatal("lock selection changed the request body")
	}
}

func TestIndependentRelationshipsShareTarget(t *testing.T) {
	a := fixtureApp(t)
	target := idInRegion("asia")
	actors := []string{idInRegion("asia"), idInRegion("asia")}
	request := func(actor string) *http.Request {
		a.s.Sessions.Set(actor, actor)
		r := httptest.NewRequest("PUT", "/api/follows/"+target, nil)
		r.Header.Set("Authorization", "Bearer "+actor)
		return r
	}
	first, second := request(actors[0]), request(actors[1])
	unlock := a.lockRequest(first)
	defer unlock()
	done := make(chan struct{})
	go func() {
		release := a.lockRequest(second)
		release()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("independent relationship waited for the shared target")
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
	stripe := testResource(t, a, "user:"+uid)
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

func TestWaitingForOneResourceReleasesOtherResources(t *testing.T) {
	for _, exclusive := range []bool{false, true} {
		t.Run(fmt.Sprintf("exclusive=%v", exclusive), func(t *testing.T) {
			a := &App{appCore: &appCore{}}
			uses := a.transactions.reference([]string{"test:0", "test:1"}, false)
			defer a.transactions.unreference(uses)
			for i := range uses {
				uses[i].write = exclusive
			}
			uses[1].entry.Lock()
			acquired, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
			go func() {
				a.acquireResources(uses)
				close(acquired)
				<-release
				for _, use := range uses {
					use.unlock()
				}
				close(finished)
			}()
			deadline := time.Now().Add(time.Second)
			for a.lockRetries.Load() == 0 && time.Now().Before(deadline) {
				runtime.Gosched()
			}
			if a.lockRetries.Load() == 0 {
				t.Error("request did not wait for the busy resource")
			}
			if uses[0].entry.TryLock() {
				uses[0].entry.Unlock()
			} else {
				t.Error("waiting request retained an unrelated resource")
			}
			uses[1].entry.Unlock()
			<-acquired
			close(release)
			<-finished
		})
	}
}

func TestInteractionAllowsConcurrentAuthorReads(t *testing.T) {
	a := fixtureApp(t)
	_, ownerToken := register(t, a, "readable_author")
	_, actorToken := register(t, a, "commenting_actor")
	openWindow(t, a, "2026-11-01")
	created := upload(t, a, "/api/posts", ownerToken, map[string]string{"visibility": "public", "location_kind": "none"}, 201)
	post := *a.s.Posts.get(created["id"].(string))
	req := httptest.NewRequest("POST", "/api/posts/"+post.ID+"/comments", bytes.NewBufferString(`{"body":"hello"}`))
	req.Header.Set("Authorization", "Bearer "+actorToken)
	unlock := a.lockRequest(req)
	done := make(chan int, 1)
	go func() { status, _, _ := call(t, a, "GET", "/api/me", ownerToken, nil); done <- status }()
	select {
	case status := <-done:
		unlock()
		if status != 200 {
			t.Fatalf("author read returned %d", status)
		}
	case <-time.After(time.Second):
		unlock()
		<-done
		t.Fatal("interaction exclusively locked an unchanged author")
	}
}

func TestSignatureReportDoesNotDrainUnrelatedUpdates(t *testing.T) {
	a := fixtureApp(t)
	oldSecret := transportSecret
	transportSecret = "signature-lock-test"
	defer func() { transportSecret = oldSecret }()
	a.globalGate.RLock()
	held := true
	defer func() {
		if held {
			a.globalGate.RUnlock()
		}
	}()
	done := make(chan int, 1)
	go func() {
		r := httptest.NewRequest("POST", masterPrefix+"failure", bytes.NewBufferString(`{"ip":"192.0.2.19","region":"europe","kind":"signature"}`))
		r.Header.Set("X-ISU-Transport", transportSecret)
		w := httptest.NewRecorder()
		a.ServeHTTP(w, r)
		done <- w.Code
	}()
	select {
	case status := <-done:
		if status != 204 {
			t.Fatalf("signature report returned %d", status)
		}
	case <-time.After(time.Second):
		a.globalGate.RUnlock()
		held = false
		<-done
		t.Fatal("signature report waited for an unrelated update")
	}
}
