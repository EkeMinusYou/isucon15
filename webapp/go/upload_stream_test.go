package main

import (
	"bytes"
	"encoding/json"
	"image"
	"image/jpeg"
	"mime/multipart"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func opaqueJPEG(size int) []byte {
	var b bytes.Buffer
	jpeg.Encode(&b, image.NewRGBA(image.Rect(0, 0, 2, 2)), nil)
	if size > b.Len() {
		b.Write(make([]byte, size-b.Len()))
	}
	return b.Bytes()
}
func TestStreamedUploadPreservesBytesAndRemovesRejectedFiles(t *testing.T) {
	t.Setenv("JPEG_VALIDATION", "header")
	a := fixtureApp(t)
	uid, token := register(t, a, "stream_upload")
	openWindow(t, a, "2026-11-01")
	original := opaqueJPEG(1 << 20)
	submit := func(visibility string) *httptest.ResponseRecorder {
		var b bytes.Buffer
		mw := multipart.NewWriter(&b)
		f, _ := mw.CreateFormFile("image", "image.jpg")
		f.Write(original)
		mw.WriteField("caption", "body-caption")
		mw.WriteField("visibility", visibility)
		mw.WriteField("location_kind", "none")
		mw.Close()
		r := httptest.NewRequest("POST", "/api/posts?caption=query-caption", &b)
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Content-Type", mw.FormDataContentType())
		w := httptest.NewRecorder()
		a.ServeHTTP(w, r)
		return w
	}
	w := submit("public")
	if w.Code != 201 {
		t.Fatalf("upload: %d %s", w.Code, w.Body.String())
	}
	var result object
	json.Unmarshal(w.Body.Bytes(), &result)
	if result["caption"] != "query-caption" {
		t.Fatal("form precedence changed")
	}
	post := a.s.Posts.get(result["id"].(string))
	stored, err := os.ReadFile(filepath.Join(a.durable, "images", post.ImageID))
	if err != nil || !bytes.Equal(stored, original) {
		t.Fatal("original image bytes changed")
	}
	if w = submit("public"); w.Code != 409 {
		t.Fatalf("duplicate upload: %d", w.Code)
	}
	if w = submit("invalid"); w.Code != 400 {
		t.Fatalf("invalid fields: %d", w.Code)
	}
	files, err := os.ReadDir(filepath.Join(a.durable, "images"))
	if err != nil || len(files) != 1 {
		t.Fatalf("rejected upload files remain: %d %v", len(files), err)
	}
	r := httptest.NewRequest("PUT", "/api/me/profile-image", bytes.NewReader(original))
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("Content-Type", "image/jpeg")
	w = httptest.NewRecorder()
	a.ServeHTTP(w, r)
	if w.Code != 204 {
		t.Fatalf("profile upload: %d", w.Code)
	}
	stored, err = os.ReadFile(filepath.Join(a.durable, "images", *a.s.Accounts.Get(uid).ProfileImageID))
	if err != nil || !bytes.Equal(stored, original) {
		t.Fatal("profile original bytes changed")
	}
}

func BenchmarkProfileUploadStorage(b *testing.B) {
	b.Setenv("JPEG_VALIDATION", "header")
	original := opaqueJPEG(1 << 20)
	b.Run("buffered", func(b *testing.B) {
		target := filepath.Join(b.TempDir(), "image")
		b.ReportAllocs()
		b.SetBytes(int64(len(original)))
		for i := 0; i < b.N; i++ {
			p := prepareProfile(bytes.NewReader(original))
			if !p.ok {
				b.Fatal("invalid image")
			}
			if err := os.WriteFile(target, p.data, 0600); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("streamed", func(b *testing.B) {
		a := &App{appCore: &appCore{durable: b.TempDir()}}
		os.MkdirAll(filepath.Join(a.durable, "images"), 0755)
		b.ReportAllocs()
		b.SetBytes(int64(len(original)))
		for i := 0; i < b.N; i++ {
			p := a.stageImage(bytes.NewReader(original), 512)
			if !p.ok || p.storageErr != nil {
				b.Fatalf("invalid stage: %+v", p)
			}
			os.Remove(filepath.Join(a.durable, "images", p.fileID))
		}
	})
}
