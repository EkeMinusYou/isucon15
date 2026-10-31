package main

import (
	"bytes"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOwnerIngressUsesLocalPermitsWithoutPublishingThem(t *testing.T) {
	a := fixtureApp(t)
	_, token := register(t, a, "owner_image_cache")
	openWindow(t, a, "2026-11-01")
	post := upload(t, a, "/api/posts", token, map[string]string{"visibility": "public", "location_kind": "none"}, 201)
	stored := a.s.Posts.get(post["id"].(string))
	original, err := os.ReadFile(filepath.Join(a.durable, "images", stored.ImageID))
	if err != nil {
		t.Fatal(err)
	}
	a.imagePermits = newOwnerImagePermits()
	h := ownerHandler(a, "asia")
	_, _, info := call(t, h, "GET", "/api/posts/"+stored.ID, token, nil)
	if info.Code != 200 {
		t.Fatal(info.Body.String())
	}
	for name := range info.Header() {
		if strings.HasPrefix(strings.ToLower(name), "x-isu-") {
			t.Fatal("public response exposed an internal permit")
		}
	}
	before := a.routeMetrics[metricRoute("/api/images/")].requests.Load()
	_, _, response := call(t, h, "GET", post["imageUrl"].(string), "", nil)
	if response.Code != 200 || !bytes.Equal(response.Body.Bytes(), original) || a.regionalPermits.hits.Load() != 1 {
		t.Fatal("owner ingress did not serve the locally authorized original")
	}
	if a.routeMetrics[metricRoute("/api/images/")].requests.Load() != before {
		t.Fatal("local permit still entered the owner's image transaction")
	}
	if !strings.Contains(response.Header().Get("Cache-Control"), "max-age=5,") || response.Header().Get("Age") == "" {
		t.Fatal("local image freshness headers changed")
	}
	requireCall(t, h, "DELETE", "/api/posts/"+stored.ID, token, nil, 204)
	requireCall(t, h, "GET", post["imageUrl"].(string), "", nil, 403)
	if a.imagePermits.failures.Load() != 0 {
		t.Fatal("local invalidation attempted an external RPC")
	}
	r := httptest.NewRequest("PUT", "/api/me/profile-image", bytes.NewReader(original))
	r.Header.Set("Content-Type", "image/jpeg")
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 204 {
		t.Fatal(w.Body.String())
	}
	me := requireCall(t, h, "GET", "/api/me", token, nil, 200)
	profileURL := me["profileImageUrl"].(string)
	requireCall(t, h, "GET", profileURL, "", nil, 200)
	requireCall(t, h, "POST", "/api/me/account-deletion", token, nil, 204)
	requireCall(t, h, "GET", profileURL, "", nil, 403)
}
