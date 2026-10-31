package main

import (
	"compress/gzip"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestHistoricalOverlayRestoresAndInitializes(t *testing.T) {
	a := fixtureApp(t)
	uid, pid, imid, wid := id(), id(), id(), id()
	rows := map[string]any{
		"accounts":              Account{ID: uid, Username: "history", Status: "active", Region: "asia", Secret: secretHash("secret")},
		"posting-windows":       Window{ID: wid, Region: "asia", LocalDate: "2026-01-01", StartsAt: time.Now().UTC(), Deadline: time.Now().UTC()},
		"posts":                 Post{ID: pid, AuthorID: uid, WindowID: wid, ImageID: imid, Caption: "initial", Visibility: "public", PostedAt: time.Now().UTC()},
		"images/index.part-000": Image{ID: imid, OwnerID: uid, MediaType: "image/jpeg", Part: "0000.bin", Size: 3},
	}
	for name, value := range rows {
		f, err := os.Create(filepath.Join(a.initial, name+".jsonl.gz"))
		if err != nil {
			t.Fatal(err)
		}
		g := gzip.NewWriter(f)
		if err = json.NewEncoder(g).Encode(value); err != nil {
			t.Fatal(err)
		}
		g.Close()
		f.Close()
	}
	if err := a.reset(); err != nil {
		t.Fatal(err)
	}
	if p := a.s.Posts.get(pid); p == nil || p.Caption != "initial" {
		t.Fatal("initial post missing")
	}
	if im := a.s.Images.get(imid); im == nil || im.Ref != pid || !a.validImage(im) {
		t.Fatal("historical image reference missing")
	}
	p := *a.s.Posts.get(pid)
	p.Caption = "updated"
	now := time.Now().UTC()
	p.DeletedAt = &now
	a.put("post", p)
	a.commit()
	restored, err := newApp(a.initial, a.durable)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.journal.Close()
	defer restored.s.closeHistory()
	if p := restored.s.Posts.get(pid); p == nil || p.Caption != "updated" || p.DeletedAt == nil {
		t.Fatal("historical mutation lost on restart")
	}
	if restored.validImage(restored.s.Images.get(imid)) {
		t.Fatal("deleted historical image became visible")
	}
	if err := restored.reset(); err != nil {
		t.Fatal(err)
	}
	if p := restored.s.Posts.get(pid); p.Caption != "initial" || p.DeletedAt != nil {
		t.Fatal("initialization did not restore historical row")
	}
	if !restored.validImage(restored.s.Images.get(imid)) {
		t.Fatal("initial image was not restored")
	}
}

func TestInitializeRemovesOnlyUploadedImages(t *testing.T) {
	a := fixtureApp(t)
	uploaded := filepath.Join(a.durable, "images", "uploaded")
	initial := filepath.Join(a.initial, "images", "original.bin")
	if err := os.WriteFile(uploaded, []byte("uploaded"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(initial, []byte("initial"), 0600); err != nil {
		t.Fatal(err)
	}
	restored, err := newApp(a.initial, a.durable)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.journal.Close()
	defer restored.s.closeHistory()
	if _, err := os.Stat(uploaded); err != nil {
		t.Fatal("restart removed an upload")
	}
	if err := restored.reset(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(uploaded); !os.IsNotExist(err) {
		t.Fatal("initialize retained an old upload")
	}
	if _, err := os.Stat(initial); err != nil {
		t.Fatal("initialize removed provided image bytes")
	}
}
