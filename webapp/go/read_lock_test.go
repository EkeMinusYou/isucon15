package main

import (
	"fmt"
	"testing"
	"time"
)

func TestResourceReadsDoNotWaitForHashCollision(t *testing.T) {
	a := fixtureApp(t)
	uid, token := register(t, a, "independent_reader")
	openWindow(t, a, "2026-11-01")
	post := upload(t, a, "/api/posts", token, map[string]string{"visibility": "public", "location_kind": "none"}, 201)
	ownerStripe := int(stateHash("user:"+uid) % stateShardCount)
	key := ""
	for i := 0; ; i++ {
		key = fmt.Sprintf("user:unrelated_%d", i)
		if int(stateHash(key)%stateShardCount) == ownerStripe {
			break
		}
	}
	uses := a.transactions.reference([]string{key}, false)
	a.acquireResources(uses)
	held := true
	defer func() {
		if held {
			a.releaseResources(uses)
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
			a.releaseResources(uses)
			held = false
			<-done
			t.Fatalf("%s waited for unrelated resource with the same hash", path)
		}
	}
}
