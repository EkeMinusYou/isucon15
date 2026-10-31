package main

import (
	"testing"
	"time"
)

func TestDayPostsTracksChangesAndRegions(t *testing.T) {
	a := fixtureApp(t)
	u := Account{ID: id(), Region: "asia"}
	a.put("account", u)
	as := Window{ID: id(), Region: "asia", LocalDate: "2026-10-31"}
	eu := Window{ID: id(), Region: "europe", LocalDate: as.LocalDate}
	a.put("window", as)
	a.put("window", eu)
	check := func(wantID string, wantDeleted int) {
		t.Helper()
		for i := 0; i < 2; i++ {
			p, deleted := a.s.dayPosts(&u)
			gotID := ""
			if p != nil {
				gotID = p.ID
			}
			if gotID != wantID || deleted != wantDeleted {
				t.Fatalf("got (%s,%d), want (%s,%d)", gotID, deleted, wantID, wantDeleted)
			}
		}
	}
	check("", 0)
	p := Post{ID: id(), AuthorID: u.ID, WindowID: as.ID, Caption: "first"}
	a.put("post", p)
	check(p.ID, 0)
	p.Caption = "edited"
	a.put("post", p)
	if got, _ := a.s.dayPosts(&u); got.Caption != "edited" {
		t.Fatal("cached post did not reflect an edit")
	}
	u.Region = "europe"
	check(p.ID, 0)
	now := time.Now().UTC()
	p.DeletedAt = &now
	a.put("post", p)
	check("", 1)
	q := Post{ID: id(), AuthorID: u.ID, WindowID: eu.ID}
	a.put("post", q)
	check(q.ID, 1)
	q.DeletedAt = &now
	a.put("post", q)
	check("", 2)
	eu.EndedAt = &now
	a.put("window", eu)
	check("", 0)
	next := Window{ID: id(), Region: "europe", LocalDate: "2026-11-01"}
	a.put("window", next)
	check("", 0)
	u.Region = "asia"
	check("", 2)
	if err := a.reset(); err != nil {
		t.Fatal(err)
	}
	check("", 0)
}
