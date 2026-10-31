package main

import (
	"net/url"
	"testing"
)

func TestIndexedSearchTracksUnicodeProfileChangesAndReplay(t *testing.T) {
	a := fixtureApp(t)
	_, viewer := register(t, a, "search_viewer")
	target, token := register(t, a, "other_person")
	check := func(app *App, query string, found bool) {
		t.Helper()
		v := requireCall(t, app, "GET", "/api/users?q="+url.QueryEscape(query), viewer, nil, 200)
		items := v["items"].([]any)
		if len(items) != 0 && (len(items) != 1 || items[0].(map[string]any)["id"] != target) {
			t.Fatalf("query %q returned unexpected users: %v", query, items)
		}
		if (len(items) == 1) != found {
			t.Fatalf("query %q: found=%v want=%v", query, len(items) == 1, found)
		}
	}
	requireCall(t, a, "PATCH", "/api/me", token, object{"displayName": "CAFÉ海猫Z"}, 200)
	check(a, "cafe\u0301", true)
	check(a, "é海", true)
	check(a, "z", true)
	requireCall(t, a, "PATCH", "/api/me", token, object{"displayName": "renamed"}, 200)
	check(a, "café", false)
	check(a, "renam", true)
	requireCall(t, a, "PUT", "/api/blocks/"+target, viewer, nil, 204)
	check(a, "renam", false)
	requireCall(t, a, "DELETE", "/api/blocks/"+target, viewer, nil, 204)
	restored, err := newApp(a.initial, a.durable)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.journal.Close()
	defer restored.s.closeHistory()
	check(restored, "café", false)
	check(restored, "renam", true)
}
