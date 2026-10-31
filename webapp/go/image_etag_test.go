package main

import (
	"bytes"
	"image"
	"image/jpeg"
	"net/http/httptest"
	"net/url"
	"testing"
)

func conditionalImage(a *App, path, etag string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("GET", path, nil)
	r.Header.Set("If-None-Match", etag)
	w := httptest.NewRecorder()
	a.ServeHTTP(w, r)
	return w
}
func TestImageETagChecksValidityBeforeRevalidation(t *testing.T) {
	a := fixtureApp(t)
	_, owner := register(t, a, "etag_owner")
	_, actor := register(t, a, "etag_actor")
	openWindow(t, a, "2026-11-01")
	post := upload(t, a, "/api/posts", owner, map[string]string{"visibility": "public", "location_kind": "none"}, 201)
	upload(t, a, "/api/posts", actor, map[string]string{"visibility": "public", "location_kind": "none"}, 201)
	reaction := upload(t, a, "/api/posts/"+post["id"].(string)+"/realmojis", actor, map[string]string{"reactionType": "like"}, 201)
	var b bytes.Buffer
	jpeg.Encode(&b, image.NewRGBA(image.Rect(0, 0, 2, 2)), nil)
	r := httptest.NewRequest("PUT", "/api/me/profile-image", &b)
	r.Header.Set("Authorization", "Bearer "+owner)
	r.Header.Set("Content-Type", "image/jpeg")
	w := httptest.NewRecorder()
	a.ServeHTTP(w, r)
	if w.Code != 204 {
		t.Fatalf("profile upload: %d", w.Code)
	}
	profile := requireCall(t, a, "GET", "/api/me", owner, nil, 200)["profileImageUrl"].(string)
	cases := []struct {
		name, path string
		invalidate func()
	}{
		{"profile", profile, func() { requireCall(t, a, "DELETE", "/api/me/profile-image", owner, nil, 204) }},
		{"reaction", reaction["imageUrl"].(string), func() { requireCall(t, a, "DELETE", "/api/realmojis/"+reaction["id"].(string), actor, nil, 204) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			first := conditionalImage(a, tc.path, `"other"`)
			tag := first.Header().Get("ETag")
			if first.Code != 200 || tag == "" || first.Header().Get("Cache-Control") != "private, no-cache" {
				t.Fatalf("initial image: %d %v", first.Code, first.Header())
			}
			for _, candidate := range []string{tag, "W/" + tag, `"other", ` + tag, "*"} {
				cached := conditionalImage(a, tc.path, candidate)
				if cached.Code != 304 || cached.Body.Len() != 0 || cached.Header().Get("ETag") != tag {
					t.Fatalf("revalidation: %d %q", cached.Code, cached.Body.String())
				}
			}
			bad, _ := url.Parse(tc.path)
			q := bad.Query()
			q.Set("sig", "invalid")
			bad.RawQuery = q.Encode()
			if got := conditionalImage(a, bad.String(), tag); got.Code != 403 || got.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("invalid signature returned a cached representation")
			}
			q = bad.Query()
			q.Set("exp", "0")
			bad.RawQuery = q.Encode()
			if got := conditionalImage(a, bad.String(), tag); got.Code != 403 {
				t.Fatal("expired signature returned a cached representation")
			}
			tc.invalidate()
			if got := conditionalImage(a, tc.path, tag); got.Code != 403 || got.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("removed image returned a cached representation")
			}
		})
	}
}
