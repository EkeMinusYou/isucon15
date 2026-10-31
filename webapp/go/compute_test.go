package main

import (
	"bytes"
	"context"
	"image"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

func TestJPEGWorkerPreservesValidation(t *testing.T) {
	old := transportSecret
	transportSecret = "jpeg-test-secret"
	t.Cleanup(func() { transportSecret = old })
	server := httptest.NewServer(computeHandler(&App{appCore: &appCore{}}))
	defer server.Close()
	t.Setenv("COMPUTE_URL", server.URL)
	var b bytes.Buffer
	if err := jpeg.Encode(&b, image.NewRGBA(image.Rect(0, 0, 128, 128)), nil); err != nil {
		t.Fatal(err)
	}
	valid := b.Bytes()
	for _, tc := range []struct {
		data []byte
		want bool
	}{{valid, true}, {valid[:len(valid)-20], false}, {[]byte("invalid"), false}} {
		got, err := remoteJPEG(context.Background(), tc.data)
		if err != nil || got != tc.want {
			t.Fatalf("remote JPEG got %v, %v; want %v", got, err, tc.want)
		}
	}
	response, err := http.Post(server.URL+"/api/internal/compute/jpeg", "application/octet-stream", bytes.NewReader(valid))
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 403 {
		t.Fatal("JPEG worker accepted an unauthenticated request")
	}
}

func TestAuthenticatedComputePreservesResults(t *testing.T) {
	old := transportSecret
	transportSecret = "compute-test-secret"
	t.Cleanup(func() { transportSecret = old })
	worker := &App{appCore: &appCore{signatures: map[string]string{}}}
	server := httptest.NewServer(computeHandler(worker))
	defer server.Close()
	t.Setenv("COMPUTE_URL", server.URL)
	response, err := http.Post(server.URL+"/api/internal/compute/signatures", "application/json", bytes.NewBufferString("[]"))
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 403 {
		t.Fatal("compute endpoint accepted an unauthenticated request")
	}
	a := &App{appCore: &appCore{signatures: map[string]string{}}}
	secret := secretHash("signing-secret")
	refs := []object{{"image": imageReference{a, "first", secret, 1800000000}}, {"image": imageReference{a, "second", secret, 1800000001}}}
	var wg sync.WaitGroup
	start := make(chan struct{})
	for n := 0; n < 32; n++ {
		wg.Add(1)
		go func() { defer wg.Done(); <-start; prefetchImageSignatures(context.Background(), refs) }()
	}
	close(start)
	wg.Wait()
	if a.remoteSignatures.Load() != 2 || worker.signatureMisses.Load() != 2 {
		t.Fatal("signatures were not delegated")
	}
	local := &App{appCore: &appCore{signatures: map[string]string{}}}
	for _, id := range []string{"first", "second"} {
		exp := int64(1800000000)
		if id == "second" {
			exp++
		}
		if a.signImage(id, secret, exp) != local.signImage(id, secret, exp) {
			t.Fatal("delegated signature differs")
		}
	}
}

func TestSignatureCacheUsesEveryInput(t *testing.T) {
	a := &App{appCore: &appCore{signatures: map[string]string{}}}
	for _, secret := range []string{secretHash("old"), secretHash("new")} {
		got := a.signImage("image", secret, 1800000000)
		if got != computeImageSignature("image", secret, 1800000000) {
			t.Fatal("cache reused a different signing key")
		}
	}
}
