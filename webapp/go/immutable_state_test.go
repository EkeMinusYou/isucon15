package main

import (
	"net/http/httptest"
	"testing"
	"time"
)

func TestPreparedStateRemainsPrivateUntilCommit(t *testing.T) {
	a := fixtureApp(t)
	uid, token := register(t, a, "private_state")
	writer := &App{appCore: a.appCore}
	unlock := writer.lockWriteState()
	defer unlock()
	account := *writer.state().Accounts.Get(uid)
	account.DisplayName = "prepared display"
	writer.put("account", account)
	done := make(chan object, 1)
	go func() { _, response, _ := call(t, a, "GET", "/api/me", token, nil); done <- response }()
	select {
	case response := <-done:
		if response["displayName"] == account.DisplayName {
			t.Fatal("uncommitted state was exposed")
		}
	case <-time.After(time.Second):
		t.Fatal("a private writer blocked a reader")
	}
	writer.commit()
	if a.published.Load().Accounts.Get(uid).DisplayName != account.DisplayName {
		t.Fatal("committed state was not published")
	}
}

func TestOldStateSurvivesConcurrentUpdates(t *testing.T) {
	a := fixtureApp(t)
	uid, token := register(t, a, "old_state_a")
	peer, peerToken := register(t, a, "old_state_b")
	openWindow(t, a, "2026-11-01")
	post := upload(t, a, "/api/posts", token, map[string]string{"visibility": "public", "location_kind": "none"}, 201)
	upload(t, a, "/api/posts", peerToken, map[string]string{"visibility": "public", "location_kind": "none"}, 201)
	request := requireCall(t, a, "POST", "/api/friend-requests", token, object{"recipientId": peer}, 201)
	requireCall(t, a, "POST", "/api/friend-requests/"+request["relationId"].(string)+"/accept", peerToken, nil, 200)
	reader := &App{appCore: a.appCore}
	unlock := reader.lockReadRequest(httptest.NewRequest("GET", "/api/feeds/friends", nil))
	defer unlock()
	old := reader.state()
	oldPost, _ := old.dayPosts(old.Accounts.Get(uid))
	done := make(chan struct{})
	go func() {
		defer close(done)
		requireCall(t, a, "PATCH", "/api/me", token, object{"displayName": "fresh_label"}, 200)
		requireCall(t, a, "POST", "/api/posts/"+oldPost.ID+"/comments", peerToken, object{"body": "fresh comment"}, 201)
		requireCall(t, a, "DELETE", "/api/friendships/"+peer, token, nil, 204)
		requireCall(t, a, "DELETE", "/api/posts/"+oldPost.ID, token, nil, 204)
		requireCall(t, a, "POST", "/api/me/account-deletion", token, nil, 204)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("a reader prevented updates from publishing")
	}
	if old.Accounts.Get(uid).DisplayName == "fresh_label" || !old.active(uid) || !old.friend(uid, peer) {
		t.Fatal("account or friendship changed in an old root")
	}
	if old.Sessions.Get(token) != uid || len(old.UserTokens.Get(uid)) == 0 || len(old.PostComments.Get(oldPost.ID)) != 0 {
		t.Fatal("nested token or comment indexes changed in an old root")
	}
	if old.Posts.get(oldPost.ID).DeletedAt != nil || len(old.LivePosts.Get(uid)) != 1 || oldPost.ImageID == "" {
		t.Fatal("post or posting index changed in an old root")
	}
	if ids, _ := old.search.candidates("fresh_label"); len(ids) != 0 {
		t.Fatal("search index changed in an old root")
	}
	current := a.published.Load()
	if current.active(uid) || current.friend(uid, peer) || current.Sessions.Get(token) != "" || len(current.PostComments.Get(oldPost.ID)) != 1 {
		t.Fatal("latest root lost an update")
	}
	if current.Posts.get(post["id"].(string)).DeletedAt == nil {
		t.Fatal("post deletion was not published")
	}
}
