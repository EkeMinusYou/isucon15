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
	a := fixtureApp(t)
	uid, token := register(t, a, "stream_upload")
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
	if rejected := submit("public"); rejected.Code != 409 {
		t.Fatalf("post without a window: %d", rejected.Code)
	}
	files, err := os.ReadDir(filepath.Join(a.durable, "images"))
	if err != nil || len(files) != 0 {
		t.Fatal("post without a window retained its staged file")
	}
	openWindow(t, a, "2026-11-01")
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
	files, err = os.ReadDir(filepath.Join(a.durable, "images"))
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

type rejectTailReader struct{ reads int }

func (r *rejectTailReader) Read(p []byte) (int, error) {
	r.reads++
	if r.reads > 1 {
		panic("invalid JPEG tail must not be consumed")
	}
	return copy(p, "not a jpeg"), nil
}

func TestImageHeaderValidation(t *testing.T) {
	a := fixtureApp(t)
	if p := a.stageImage(&rejectTailReader{}, 512); p.ok || !p.invalidJPEG {
		t.Fatal("non-JPEG must be rejected before reading its tail")
	}
	var b bytes.Buffer
	jpeg.Encode(&b, image.NewRGBA(image.Rect(0, 0, 128, 128)), nil)
	truncated := b.Bytes()[:b.Len()-20]
	p := a.stageImage(bytes.NewReader(truncated), 512)
	got, err := os.ReadFile(filepath.Join(a.durable, "images", p.fileID))
	if !p.ok || err != nil || !bytes.Equal(got, truncated) {
		t.Fatal("valid header must retain the opaque payload")
	}
	b.Reset()
	jpeg.Encode(&b, image.NewRGBA(image.Rect(0, 0, 513, 2)), nil)
	if p := a.stageImage(bytes.NewReader(b.Bytes()), 512); p.ok || p.invalidJPEG {
		t.Fatal("dimension rejection must remain distinct from a malformed header")
	}
}
