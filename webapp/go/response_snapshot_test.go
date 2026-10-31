package main

import (
	"encoding/json"
	"testing"
	"time"
)

func TestPostSnapshotSurvivesMutationAndReset(t *testing.T) {
	a := fixtureApp(t)
	uid, token := register(t, a, "snapshot_post")
	admin := requireCall(t, a, "POST", "/api/admin/auth/login", "", object{"username": "admin", "password": "password"}, 200)["token"].(string)
	requireCall(t, a, "POST", "/api/admin/posting-window", admin, object{"region": "asia", "localDate": "2026-11-01"}, 201)
	p := upload(t, a, "/api/posts", token, map[string]string{"caption": "captured", "visibility": "public", "location_kind": "exact", "latitude": "12.3", "longitude": "45.6"}, 201)
	post := a.s.Posts.get(p["id"].(string))
	snapshot := a.snapshotPost(post)
	want, err := json.Marshal(snapshot.object())
	if err != nil {
		t.Fatal(err)
	}
	a.s.Accounts.Get(uid).DisplayName = "changed"
	post.Caption = "changed"
	*post.Location.Latitude = 80
	a.s.Windows.Get(post.WindowID).Deadline = time.Now().Add(time.Hour)
	if err := a.reset(); err != nil {
		t.Fatal(err)
	}
	got, err := json.Marshal(snapshot.object())
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("snapshot changed: %s != %s", got, want)
	}
}

func TestRankingSnapshotOwnsGraph(t *testing.T) {
	s := emptyState()
	s.Adj = newShardedMapWith(map[string]map[string]bool{"viewer": {"a": true, "b": true}, "author": {"a": true, "x": true}, "a": {"viewer": true, "b": true}, "x": {"a": true}})
	r := s.snapshotRanking("viewer")
	r.capture(s, "author")
	r.capture(s, "a")
	r.capture(s, "x")
	s.Adj.Get("viewer")["x"] = true
	s.Adj.Get("author")["b"] = true
	delete(s.Adj.Get("a"), "b")
	if score := r.score("author", []string{"a", "a", "x"}); score != 15 {
		t.Fatalf("score=%d, want 15", score)
	}
}

func TestPostPeriodIndexHandlesOutOfOrderDatesAndReplay(t *testing.T) {
	a := fixtureApp(t)
	uid := id()
	for _, date := range []string{"2026-11-01", "2026-10-30", "2026-10-31"} {
		w := Window{ID: id(), Region: "asia", LocalDate: date}
		a.put("window", w)
		a.put("post", Post{ID: id(), AuthorID: uid, WindowID: w.ID})
	}
	a.commit()
	check := func(s *State) {
		t.Helper()
		if n := len(s.postsInPeriod(uid, 20261001, 20261101)); n != 2 {
			t.Fatalf("October: %d posts", n)
		}
		if n := len(s.postsInPeriod(uid, 20261101, 20261102)); n != 1 {
			t.Fatalf("November 1: %d posts", n)
		}
	}
	check(a.s)
	restored, err := newApp(a.initial, a.durable)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.journal.Close()
	defer restored.s.closeHistory()
	check(restored.s)
}
