package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

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
