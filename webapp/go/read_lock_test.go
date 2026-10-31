package main

import (
	"fmt"
	"testing"
	"time"
)

func TestResourceReadsDoNotWaitForUnrelatedStripe(t *testing.T) {
	a := fixtureApp(t)
	uid, token := register(t, a, "independent_reader")
	openWindow(t, a, "2026-11-01")
	post := upload(t, a, "/api/posts", token, map[string]string{"visibility": "public", "location_kind": "none"}, 201)
	ownerStripe := int(stateHash("user:"+uid) % stateShardCount)
	postStripe := int(stateHash("post:"+post["id"].(string)) % stateShardCount)
	blocked := 0
	for blocked == ownerStripe || blocked == postStripe {
		blocked++
	}
	stripe := &a.transactions[blocked]
	stripe.Lock()
	held := true
	defer func() {
		if held {
			stripe.Unlock()
		}
	}()
	for _, path := range []string{"/api/me", "/api/posting-window/current", "/api/posts/me/current", "/api/me/export", "/api/feeds/public", "/api/memories?month=2026-11", "/api/users/" + uid, "/api/posts/" + post["id"].(string), post["imageUrl"].(string)} {
		done := make(chan int, 1)
		go func(path string) { status, _, _ := call(t, a, "GET", path, token, nil); done <- status }(path)
		select {
		case status := <-done:
			if status != 200 {
				t.Fatalf("%s: status=%d", path, status)
			}
		case <-time.After(time.Second):
			stripe.Unlock()
			held = false
			<-done
			t.Fatal(fmt.Sprintf("%s waited for unrelated stripe", path))
		}
	}
}

func TestImageReadsBypassTheCollectionGateAndObserveDeletion(t *testing.T) {
	a := fixtureApp(t)
	_, token := register(t, a, "independent_image")
	openWindow(t, a, "2026-11-01")
	post := upload(t, a, "/api/posts", token, map[string]string{"visibility": "public", "location_kind": "none"}, 201)
	imageURL := post["imageUrl"].(string)
	a.globalGate.Lock()
	held := true
	defer func() {
		if held {
			a.globalGate.Unlock()
		}
	}()
	fetched := make(chan int, 1)
	go func() { status, _, _ := call(t, a, "GET", imageURL, "", nil); fetched <- status }()
	select {
	case status := <-fetched:
		if status != 200 {
			t.Fatalf("image status=%d", status)
		}
	case <-time.After(time.Second):
		a.globalGate.Unlock()
		held = false
		<-fetched
		t.Fatal("image waited for unrelated collection state")
	}
	a.globalGate.Unlock()
	held = false
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 20; i++ {
			status, _, _ := call(t, a, "GET", imageURL, "", nil)
			if status != 200 && status != 403 {
				t.Errorf("concurrent image status=%d", status)
				return
			}
		}
	}()
	requireCall(t, a, "DELETE", "/api/posts/"+post["id"].(string), token, nil, 204)
	<-done
	requireCall(t, a, "GET", imageURL, "", nil, 403)
}
