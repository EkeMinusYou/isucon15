package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
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
	fixture := fixtureApp(t)
	files, err := newRegionalUploads(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	durable := t.TempDir()
	m, err := newRegionalMaster(fixture.initial, durable, filepath.Join(fixture.initial, "images"), "asia", files)
	if err != nil {
		t.Fatal(err)
	}
	uid, imageID, token := idInRegion("asia"), idInRegion("asia"), idInRegion("asia")
	os.WriteFile(filepath.Join(files.acceptedDir(), imageID), []byte("original"), 0600)
	u := Account{ID: uid, Username: "master_user", Secret: secretHash("secret"), Region: "asia", HomeRegion: "asia", Status: "active", ProfileImageID: &imageID}
	txn := masterTransaction{ID: id(), Changes: []change{
		masterChange(t, "account", u),
		masterChange(t, "image", Image{ID: imageID, OwnerID: uid, Size: 8, MediaType: "image/jpeg", StorageRegion: "asia"}),
		masterChange(t, "session", tokenChange{token, uid}),
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
	m.close()
	m, err = newRegionalMaster(fixture.initial, durable, filepath.Join(fixture.initial, "images"), "asia", files)
	if err != nil {
		t.Fatal(err)
	}
	defer m.close()
	if m.s.Accounts.Get(uid).HomeRegion != "asia" || m.s.Accounts.Get(uid).Region != "europe" || m.s.Sessions.Get(token) != uid || m.s.Images.get(imageID) == nil {
		t.Fatal("master records or fixed home were lost after restart")
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
