package main

import "testing"

func TestRelationshipFeedsAndFollowReplay(t *testing.T) {
	a := fixtureApp(t)
	ids, tokens := make([]string, 5), make([]string, 5)
	for i, name := range []string{"viewer", "friend_one", "friend_two", "mutual_author", "followed_author"} {
		ids[i], tokens[i] = register(t, a, name)
	}
	admin := openWindow(t, a, "2026-11-01")
	requireCall(t, a, "PUT", "/api/admin/users/"+ids[4]+"/certification", admin, nil, 204)
	postIDs := make([]string, len(tokens))
	for i, token := range tokens {
		post := upload(t, a, "/api/posts", token, map[string]string{"visibility": "public", "location_kind": "none"}, 201)
		postIDs[i] = post["id"].(string)
	}
	for _, pair := range [][2]int{{0, 1}, {0, 2}, {1, 3}, {2, 3}} {
		a.put("friend", Relation{ID: id(), User1ID: ids[pair[0]], User2ID: ids[pair[1]]})
	}
	a.commit()
	requireCall(t, a, "PUT", "/api/follows/"+ids[4], tokens[0], nil, 204)
	check := func(app *App, kind string, want ...string) {
		t.Helper()
		response := requireCall(t, app, "GET", "/api/feeds/"+kind, tokens[0], nil, 200)
		items := response["items"].([]any)
		if len(items) != len(want) {
			t.Fatalf("%s: got %d posts, want %d", kind, len(items), len(want))
		}
		authors := map[string]bool{}
		for _, item := range items {
			authors[item.(map[string]any)["author"].(map[string]any)["id"].(string)] = true
		}
		for _, id := range want {
			if !authors[id] {
				t.Fatalf("%s: missing author %s", kind, id)
			}
		}
	}
	check(a, "friends", ids[1], ids[2])
	check(a, "friends-of-friends", ids[3])
	check(a, "following", ids[4])
	check(a, "public", ids[1], ids[2], ids[3], ids[4])

	requireCall(t, a, "PATCH", "/api/posts/"+postIDs[3], tokens[3], object{"visibility": "friends"}, 200)
	check(a, "friends-of-friends")
	requireCall(t, a, "PATCH", "/api/posts/"+postIDs[3], tokens[3], object{"visibility": "friends_of_friends"}, 200)
	check(a, "friends-of-friends", ids[3])
	requireCall(t, a, "PATCH", "/api/posts/"+postIDs[4], tokens[4], object{"visibility": "friends"}, 200)
	check(a, "following")
	requireCall(t, a, "PATCH", "/api/posts/"+postIDs[4], tokens[4], object{"visibility": "public"}, 200)
	check(a, "following", ids[4])
	requireCall(t, a, "DELETE", "/api/follows/"+ids[4], tokens[0], nil, 204)
	check(a, "following")
	restored, err := newApp(a.initial, a.durable)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.journal.Close()
	defer restored.s.closeHistory()
	check(restored, "friends", ids[1], ids[2])
	check(restored, "friends-of-friends", ids[3])
	check(restored, "following")
	requireCall(t, restored, "DELETE", "/api/posts/"+postIDs[0], tokens[0], nil, 204)
	for _, kind := range []string{"friends", "friends-of-friends", "following", "public"} {
		check(restored, kind)
	}
}
