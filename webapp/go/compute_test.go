package main

import (
	"bytes"
	"context"
	"image"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAuthenticatedComputePreservesResults(t *testing.T) {
	old := transportSecret
	transportSecret = "compute-test-secret"
	t.Cleanup(func() { transportSecret = old })
	worker := &App{signatures: map[string]string{}}
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
	var imageBody bytes.Buffer
	jpeg.Encode(&imageBody, image.NewRGBA(image.Rect(0, 0, 3, 3)), nil)
	if !validateJPEG(context.Background(), imageBody.Bytes()) {
		t.Fatal("valid delegated JPEG rejected")
	}
	if validateJPEG(context.Background(), []byte("invalid")) {
		t.Fatal("invalid delegated JPEG accepted")
	}
	a := &App{signatures: map[string]string{}}
	secret := secretHash("signing-secret")
	refs := []object{{"image": imageReference{a, "first", secret, 1800000000}}, {"image": imageReference{a, "second", secret, 1800000001}}}
	prefetchImageSignatures(context.Background(), refs)
	if a.remoteSignatures.Load() != 2 || worker.signatureMisses.Load() != 2 {
		t.Fatal("signatures were not delegated")
	}
	local := &App{signatures: map[string]string{}}
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
