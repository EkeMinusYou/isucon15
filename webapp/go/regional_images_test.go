package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestRegionalImageUsesOriginalAndReauthorizesFallback(t *testing.T) {
	old := transportSecret
	transportSecret = "regional-test"
	t.Cleanup(func() { transportSecret = old })
	a := fixtureApp(t)
	_, token := register(t, a, "regional_owner")
	openWindow(t, a, "2026-11-01")
	post := upload(t, a, "/api/posts", token, map[string]string{"visibility": "public", "location_kind": "none"}, 201)
	p := a.s.Posts.get(post["id"].(string))
	original, err := os.ReadFile(filepath.Join(a.durable, "images", p.ImageID))
	if err != nil {
		t.Fatal(err)
	}
	im := *a.s.Images.get(p.ImageID)
	im.Part, im.Offset = "0000.bin", 7
	a.s.Images.set(im.ID, &im)
	part := append([]byte("prefix!"), original...)
	part = append(part, []byte("suffix")...)
	if err = os.WriteFile(filepath.Join(a.initial, "images", im.Part), part, 0600); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	local := filepath.Join(root, im.Part)
	os.WriteFile(local, part, 0600)
	server := httptest.NewServer(a)
	defer server.Close()
	request := func(internal bool) *http.Response {
		r, _ := http.NewRequest("GET", server.URL+post["imageUrl"].(string), nil)
		r.Header.Set(regionalImageRequest, "1")
		if internal {
			r.Header.Set("X-ISU-Transport", transportSecret)
		}
		response, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		return response
	}
	external := request(false)
	body, _ := io.ReadAll(external.Body)
	external.Body.Close()
	if external.StatusCode != 200 || !bytes.Equal(body, original) || external.Header.Get(regionalImagePart) != "" {
		t.Fatal("public client obtained internal delivery metadata")
	}
	response := request(true)
	if response.ContentLength != 0 || response.Header.Get(regionalImagePart) != im.Part {
		t.Fatal("owner did not return authorization metadata")
	}
	if err = serveRegionalImage(response, root, http.DefaultTransport, nil); err != nil {
		t.Fatal(err)
	}
	body, err = io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || !bytes.Equal(body, original) || response.ContentLength != int64(len(original)) || response.Header.Get(regionalImagePart) != "" {
		t.Fatal("regional original differs")
	}
	keyFile := filepath.Join(t.TempDir(), "transport-key")
	if err := os.WriteFile(keyFile, []byte(transportSecret), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("REGION", "europe")
	t.Setenv("OWNER_URL", server.URL)
	t.Setenv("TRANSPORT_KEY_FILE", keyFile)
	t.Setenv("IMAGE_CACHE_DIR", root)
	oldRegion := fixedRegion
	t.Cleanup(func() { fixedRegion = oldRegion })
	handler, edge, err := serviceHandler()
	if err != nil {
		t.Fatal(err)
	}
	// Both hosts share this test process; keep the owner in its own region.
	fixedRegion = oldRegion
	proxy := httptest.NewServer(handler)
	defer proxy.Close()
	public, err := http.Get(proxy.URL + post["imageUrl"].(string))
	if err != nil {
		t.Fatal(err)
	}
	body, err = io.ReadAll(public.Body)
	public.Body.Close()
	if err != nil || public.StatusCode != 200 || !bytes.Equal(body, original) || public.Header.Get(regionalImagePart) != "" {
		t.Fatal("regional proxy did not preserve the public response")
	}
	if edge.regionalImages.local.Load() != 1 || edge.regionalImages.bytes.Load() != uint64(len(original)) {
		t.Fatal("regional transfer counters differ from the response")
	}
	os.Remove(local)
	response = request(true)
	if err = serveRegionalImage(response, root, http.DefaultTransport, nil); err != nil {
		t.Fatal(err)
	}
	body, err = io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || !bytes.Equal(body, original) {
		t.Fatal("missing local original did not fall back")
	}
	requireCall(t, a, "DELETE", "/api/posts/"+p.ID, token, nil, 204)
	response = request(true)
	if err = serveRegionalImage(response, root, http.DefaultTransport, nil); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 403 || response.Header.Get("Cache-Control") != "no-store" {
		t.Fatal("deleted image was authorized for local delivery")
	}
}
