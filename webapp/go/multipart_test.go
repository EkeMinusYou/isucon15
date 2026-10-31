package main

import (
	"bytes"
	"io"
	"mime/multipart"
	"net/http/httptest"
	"reflect"
	"testing"
)

func TestMultipartImagePreservesSelectionAndValues(t *testing.T) {
	for _, fileCount := range []int{0, 1, 2} {
		var wire bytes.Buffer
		mw := multipart.NewWriter(&wire)
		mw.WriteField("caption", "body one")
		mw.WriteField("caption", "日本語")
		mw.WriteField("image", "ordinary field")
		for i := 0; i < fileCount; i++ {
			f, _ := mw.CreateFormFile("image", "image.jpg")
			f.Write([]byte{byte(i), 1, 2, 3})
		}
		f, _ := mw.CreateFormFile("other", "other.bin")
		f.Write([]byte("ignored"))
		mw.Close()
		makeRequest := func() *bytes.Reader { return bytes.NewReader(wire.Bytes()) }
		want := httptest.NewRequest("POST", "/api/posts?caption=query&extra=value", makeRequest())
		want.Header.Set("Content-Type", mw.FormDataContentType())
		if err := want.ParseMultipartForm(32 << 20); err != nil {
			t.Fatal(err)
		}
		var expected []byte
		file, _, err := want.FormFile("image")
		if err == nil {
			expected, _ = io.ReadAll(file)
			file.Close()
		}
		got := httptest.NewRequest("POST", "/api/posts?caption=query&extra=value", makeRequest())
		got.Header.Set("Content-Type", mw.FormDataContentType())
		var data []byte
		found := readMultipartSelected(got, func(part io.Reader) error {
			var err error
			data, err = io.ReadAll(part)
			return err
		})
		if found != (fileCount != 0) || !bytes.Equal(data, expected) {
			t.Fatalf("file selection differs for %d files: %v %v", fileCount, data, expected)
		}
		if !reflect.DeepEqual(got.Form, want.Form) || !reflect.DeepEqual(got.PostForm, want.PostForm) || got.FormValue("caption") != want.FormValue("caption") {
			t.Fatalf("form precedence differs: got %v/%v, want %v/%v", got.Form, got.PostForm, want.Form, want.PostForm)
		}
		want.MultipartForm.RemoveAll()
	}
}
