package main

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestStreamedExportPreservesArchiveAndErrors(t *testing.T) {
	a := fixtureApp(t)
	_, token := register(t, a, "export_stream")
	admin := requireCall(t, a, "POST", "/api/admin/auth/login", "", object{"username": "admin", "password": "password"}, 200)["token"].(string)
	requireCall(t, a, "POST", "/api/admin/posting-window", admin, object{"region": "asia", "localDate": "2026-11-01"}, 201)
	post := upload(t, a, "/api/posts", token, map[string]string{"caption": "original", "visibility": "public", "location_kind": "none"}, 201)
	_, _, original := call(t, a, "GET", post["imageUrl"].(string), "", nil)
	status, _, response := call(t, a, "GET", "/api/me/export", token, nil)
	if status != 200 || response.Header().Get("Content-Type") != "application/zip" {
		t.Fatal("invalid ZIP response")
	}
	zr, err := zip.NewReader(bytes.NewReader(response.Body.Bytes()), int64(response.Body.Len()))
	if err != nil || len(zr.File) != 2 {
		t.Fatalf("invalid archive: %v", err)
	}
	for _, f := range zr.File {
		r, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(r)
		r.Close()
		if err != nil {
			t.Fatal(err)
		}
		switch f.Name {
		case "images/000001.jpg":
			if f.Method != zip.Store || !bytes.Equal(data, original.Body.Bytes()) {
				t.Fatal("image bytes changed")
			}
		case "posts.json":
			var posts []object
			if json.Unmarshal(data, &posts) != nil || len(posts) != 1 || posts[0]["id"] != post["id"] || posts[0]["imagePath"] != "images/000001.jpg" {
				t.Fatal("post metadata changed")
			}
		default:
			t.Fatalf("unexpected entry %s", f.Name)
		}
	}
	p := a.s.Posts.get(post["id"].(string))
	if err := os.Remove(filepath.Join(a.durable, "images", p.ImageID)); err != nil {
		t.Fatal(err)
	}
	requireCall(t, a, "GET", "/api/me/export", token, nil, 500)
}
