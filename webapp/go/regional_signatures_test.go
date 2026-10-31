package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestRegionalSigningPreservesPublicURLsAndHidesPrivateData(t *testing.T) {
	oldSecret, oldRegion := transportSecret, fixedRegion
	transportSecret, fixedRegion = "regional-signing-test", "asia"
	t.Cleanup(func() { transportSecret, fixedRegion = oldSecret, oldRegion })
	a := fixtureApp(t)
	uid, token := register(t, a, "regional_signing")
	im := Image{ID: id(), OwnerID: uid, MediaType: "image/jpeg"}
	a.put("image", im)
	u := *a.s.Accounts.Get(uid)
	u.ProfileImageID = &im.ID
	a.put("account", u)
	a.commit()
	owner := httptest.NewServer(a)
	defer owner.Close()
	keyFile := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(keyFile, []byte(transportSecret), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("REGION", "asia")
	t.Setenv("OWNER_URL", owner.URL)
	t.Setenv("TRANSPORT_KEY_FILE", keyFile)
	t.Setenv("REGIONAL_STATE_DIR", t.TempDir())
	t.Setenv("IMAGE_CACHE_DIR", "")
	t.Setenv("COMPUTE_URL", "http://127.0.0.1:1")
	handler, edge, err := serviceHandler()
	if err != nil {
		t.Fatal(err)
	}
	proxy := httptest.NewServer(handler)
	defer proxy.Close()
	get := func(base, path string) (*http.Response, []byte) {
		t.Helper()
		r, _ := http.NewRequest("GET", base+path, nil)
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set(regionalSignRequest, "1")
		resp, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil || resp.StatusCode != 200 {
			t.Fatalf("GET %s: %v, status=%d", path, err, resp.StatusCode)
		}
		if resp.Header.Get(regionalSignResponse) != "" || strings.Contains(string(body), u.Secret) || strings.Contains(string(body), unsignedImageMarker) {
			t.Fatal("private signing data leaked to the public response")
		}
		return resp, body
	}
	_, body := get(proxy.URL, "/api/me")
	var profile map[string]any
	if err := json.Unmarshal(body, &profile); err != nil {
		t.Fatal(err)
	}
	imageURL, err := url.Parse(profile["profileImageUrl"].(string))
	if err != nil {
		t.Fatal(err)
	}
	exp, err := strconv.ParseInt(imageURL.Query().Get("exp"), 10, 64)
	if err != nil || imageURL.Query().Get("sig") != computeImageSignature(im.ID, u.Secret, exp) {
		t.Fatal("regional signature differs")
	}
	if edge.signatureMisses.Load() != 1 || edge.signatureRPCFailures.Load() != 0 || a.remoteSignatures.Load() != 0 {
		t.Fatal("signing did not finish locally")
	}
	get(owner.URL, "/api/me") // A forged public header must not enable private references.
	headStatus := 0
	for _, base := range []string{owner.URL, proxy.URL} {
		req, _ := http.NewRequest("HEAD", base+"/api/me", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		response, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if headStatus == 0 {
			headStatus = response.StatusCode
		} else if response.StatusCode != headStatus {
			t.Fatal("HEAD response changed")
		}
	}
	reset, _ := http.NewRequest("POST", proxy.URL+regionalResetPath, nil)
	reset.Header.Set("X-ISU-Transport", transportSecret)
	resetResponse, err := http.DefaultClient.Do(reset)
	if err != nil {
		t.Fatal(err)
	}
	resetResponse.Body.Close()
	if resetResponse.StatusCode != 204 || len(edge.signatures) != 0 {
		t.Fatal("regional cache was not reset")
	}
	resp, _ := get(proxy.URL, "/api/me/export")
	if resp.Header.Get("Content-Type") != "application/zip" {
		t.Fatal("export was transformed as JSON")
	}
}

func TestMalformedPrivateReferenceFailsClosed(t *testing.T) {
	resp := &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"_isu_image_reference":[null]}`))}
	resp.Header.Set(regionalSignResponse, "1")
	resp.Header.Set("Content-Type", "application/json")
	if err := signRegionalResponse(resp, &App{appCore: &appCore{}}); err == nil || resp.Header.Get(regionalSignResponse) != "" {
		t.Fatal("malformed private data was accepted")
	}
}
