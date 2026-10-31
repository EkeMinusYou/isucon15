package main

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func masterChange(t *testing.T, kind string, value any) change {
	t.Helper()
	b, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return change{kind, b}
}

func TestRegionalMasterDurabilityAndEpoch(t *testing.T) {
	for _, shared := range []bool{false, true} {
		t.Run(fmt.Sprintf("shared=%t", shared), func(t *testing.T) { testRegionalMasterDurability(t, shared) })
	}
}

func testRegionalMasterDurability(t *testing.T, shared bool) {
	fixture := fixtureApp(t)
	files, err := newRegionalUploads(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	durable := t.TempDir()
	var base *State
	if shared {
		base = fixture.s
	}
	open := func() (*regionalMaster, error) {
		return openRegionalMaster(fixture.initial, durable, filepath.Join(fixture.initial, "images"), "asia", files, base)
	}
	m, err := open()
	if err != nil {
		t.Fatal(err)
	}
	uid, imageID := idInRegion("asia"), idInRegion("asia")
	os.WriteFile(filepath.Join(files.acceptedDir(), imageID), []byte("original"), 0600)
	u := Account{ID: uid, Username: "master_user", Secret: secretHash("secret"), Region: "asia", HomeRegion: "asia", Status: "active", ProfileImageID: &imageID}
	txn := masterTransaction{ID: id(), Changes: []change{
		masterChange(t, "account", u),
		masterChange(t, "image", Image{ID: imageID, OwnerID: uid, Size: 8, MediaType: "image/jpeg", StorageRegion: "asia"}),
	}}
	if err := m.commit([]masterTransaction{txn}); err != nil {
		t.Fatal(err)
	}
	info, _ := m.journal.Stat()
	if err := m.commit([]masterTransaction{txn}); err != nil {
		t.Fatal(err)
	}
	again, _ := m.journal.Stat()
	if info.Size() != again.Size() {
		t.Fatal("retry duplicated a committed master transaction")
	}
	u.Region = "europe"
	if err := m.commit([]masterTransaction{{ID: id(), Changes: []change{masterChange(t, "account", u)}}}); err != nil {
		t.Fatal(err)
	}
	if shared && (m.initialState != base || base.Accounts.Get(uid) != nil) {
		t.Fatal("master failed to isolate updates from shared initial state")
	}
	m.close()
	m, err = open()
	if err != nil {
		t.Fatal(err)
	}
	defer m.close()
	if m.s.Accounts.Get(uid).HomeRegion != "asia" || m.s.Accounts.Get(uid).Region != "europe" || m.s.Images.get(imageID) == nil {
		t.Fatal("master records or fixed home were lost after restart")
	}
	exp := time.Now().Unix() + 3600
	path := fmt.Sprintf("/api/images/%s?exp=%d&sig=%s", imageID, exp, computeImageSignature(imageID, u.Secret, exp))
	response := httptest.NewRecorder()
	m.getImage(response, httptest.NewRequest("GET", path, nil), imageID)
	if response.Code != 200 || response.Body.String() != "original" || response.Header().Get("ETag") == "" {
		t.Fatal("master did not serve its current original")
	}
	conditional := httptest.NewRequest("GET", path, nil)
	conditional.Header.Set("If-None-Match", response.Header().Get("ETag"))
	response = httptest.NewRecorder()
	m.getImage(response, conditional, imageID)
	if response.Code != 304 || response.Body.Len() != 0 {
		t.Fatal("master image validator changed")
	}
	u.Status = "deleted"
	if err := m.commit([]masterTransaction{{ID: id(), Changes: []change{masterChange(t, "account", u)}}}); err != nil {
		t.Fatal(err)
	}
	response = httptest.NewRecorder()
	m.getImage(response, httptest.NewRequest("GET", path, nil), imageID)
	if response.Code != 403 {
		t.Fatal("master served an image after account deletion")
	}
	if err := m.reset("next-epoch"); err != nil {
		t.Fatal(err)
	}
	if err := m.commit([]masterTransaction{txn}); err == nil {
		t.Fatal("pre-initialize transaction crossed epochs")
	}
	if m.s.Accounts.Get(uid) != nil {
		t.Fatal("initialize retained a previous user")
	}
	if _, err := os.Stat(filepath.Join(files.acceptedDir(), imageID)); !os.IsNotExist(err) {
		t.Fatal("initialize retained an uploaded original")
	}
}
