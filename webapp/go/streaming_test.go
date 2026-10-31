package main

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
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

func TestRejectedUploadRemovesStagedFile(t *testing.T) {
	a := fixtureApp(t)
	_, token := register(t, a, "staged_upload")
	upload(t, a, "/api/posts", token, map[string]string{"caption": "no window", "visibility": "public", "location_kind": "none"}, 409)
	files, err := os.ReadDir(filepath.Join(a.durable, "images"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 0 {
		t.Fatalf("rejected upload retained %d files", len(files))
	}
}

type discardResponseWriter struct {
	header       http.Header
	status, size int
}

func (w *discardResponseWriter) Header() http.Header    { return w.header }
func (w *discardResponseWriter) WriteHeader(status int) { w.status = status }
func (w *discardResponseWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = 200
	}
	w.size += len(p)
	return len(p), nil
}

func BenchmarkImageResponseBuffering(b *testing.B) {
	a := &App{s: emptyState(), durable: b.TempDir(), signatures: map[string]string{}}
	uid, pid, iid := id(), id(), id()
	a.s.Accounts[uid] = &Account{ID: uid, Status: "active", Secret: secretHash("key")}
	a.s.Posts.set(pid, &Post{ID: pid, AuthorID: uid})
	a.s.Images.set(iid, &Image{ID: iid, OwnerID: uid, MediaType: "image/jpeg", Size: 1 << 20, Ref: pid, Kind: 'p'})
	if err := os.MkdirAll(filepath.Join(a.durable, "images"), 0755); err != nil {
		b.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(a.durable, "images", iid), make([]byte, 1<<20), 0600); err != nil {
		b.Fatal(err)
	}
	exp := time.Now().Add(time.Hour).Unix()
	url := "/api/images/" + iid + "?exp=" + strconv.FormatInt(exp, 10) + "&sig=" + a.signImage(iid, a.s.Accounts[uid].Secret, exp)
	r := httptest.NewRequest("GET", url, nil)
	b.ReportAllocs()
	b.SetBytes(1 << 20)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		w := &discardResponseWriter{header: make(http.Header)}
		a.ServeHTTP(w, r)
		if w.status != 200 || w.size != 1<<20 {
			b.Fatalf("image response: status=%d bytes=%d", w.status, w.size)
		}
	}
}
