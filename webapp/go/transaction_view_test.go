package main

import (
	"encoding/json"
	"testing"
	"time"
)

func TestPreparedStateDoesNotMutateCommittedIndexes(t *testing.T) {
	a := fixtureApp(t)
	uid, token := register(t, a, "prepared_owner")
	other, _ := register(t, a, "prepared_friend")
	openWindow(t, a, "2026-11-01")
	post := upload(t, a, "/api/posts", token, map[string]string{"visibility": "public", "location_kind": "none"}, 201)
	base := a.s
	prepared := base.fork()
	apply := func(kind string, value any) {
		t.Helper()
		b, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if err := prepared.apply(kind, b); err != nil {
			t.Fatal(err)
		}
	}
	user := *prepared.Accounts.Get(uid)
	user.DisplayName = "uncommitted"
	apply("account", user)
	apply("friend", Relation{ID: id(), User1ID: uid, User2ID: other})
	apply("session", tokenChange{token, ""})
	p := *prepared.Posts.get(post["id"].(string))
	now := time.Now()
	p.DeletedAt = &now
	apply("post", p)
	apply("window", Window{ID: p.WindowID, Region: "asia", EndedAt: &now})
	if base.Accounts.Get(uid).DisplayName == "uncommitted" || base.friend(uid, other) || base.Sessions.Get(token) == "" || !base.UserTokens.Get(uid)[token] || base.Posts.get(p.ID).DeletedAt != nil || len(base.LivePosts.Get(uid)) == 0 {
		t.Fatal("prepared update leaked into committed state")
	}
	if !prepared.friend(uid, other) || prepared.Sessions.Get(token) != "" || prepared.UserTokens.Get(uid)[token] || len(prepared.LivePosts.Get(uid)) != 0 {
		t.Fatal("prepared update lost read-your-writes semantics")
	}
}
