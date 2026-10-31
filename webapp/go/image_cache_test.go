package main

import (
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestPostImageCachePreservesExpiryAndRevocation(t *testing.T) {
	a := fixtureApp(t)
	uid, token := register(t, a, "cache_owner")
	_, other := register(t, a, "cache_reactor")
	openWindow(t, a, "2026-11-01")
	post := upload(t, a, "/api/posts", token, map[string]string{"visibility": "public", "location_kind": "none"}, 201)
	imageURL := post["imageUrl"].(string)
	status, _, w := call(t, a, "GET", imageURL, "", nil)
	if status != 200 || w.Header().Get("Cache-Control") != "private, max-age=5, must-revalidate" {
		t.Fatalf("post cache: %d %v", status, w.Header())
	}
	if _, err := time.Parse(time.RFC1123, w.Header().Get("Date")); err != nil {
		t.Fatal(err)
	}

	parsed, _ := url.Parse(imageURL)
	qid := strings.TrimPrefix(parsed.Path, "/api/images/")
	for _, remaining := range []int64{2, -1} {
		exp := time.Now().Unix() + remaining
		q := parsed.Query()
		q.Set("exp", strconv.FormatInt(exp, 10))
		q.Set("sig", a.signImage(qid, a.s.Accounts.Get(uid).Secret, exp))
		parsed.RawQuery = q.Encode()
		status, _, w = call(t, a, "GET", parsed.String(), "", nil)
		if remaining > 0 {
			control := w.Header().Get("Cache-Control")
			if status != 200 || (control != "private, max-age=2, must-revalidate" && control != "private, max-age=1, must-revalidate") {
				t.Fatalf("short expiry: %d %v", status, w.Header())
			}
		} else if status != 403 || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("expired image became cacheable")
		}
	}

	upload(t, a, "/api/posts", other, map[string]string{"visibility": "public", "location_kind": "none"}, 201)
	reaction := upload(t, a, "/api/posts/"+post["id"].(string)+"/realmojis", other, map[string]string{"reactionType": "like"}, 201)
	status, _, w = call(t, a, "GET", reaction["imageUrl"].(string), "", nil)
	if status != 200 || w.Header().Get("Cache-Control") != "private, no-cache" || w.Header().Get("ETag") == "" {
		t.Fatal("reaction must be revalidated before reuse")
	}

	requireCall(t, a, "DELETE", "/api/posts/"+post["id"].(string), token, nil, 204)
	status, _, w = call(t, a, "GET", imageURL, "", nil)
	if status != 403 || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("deleted image became cacheable")
	}
}
