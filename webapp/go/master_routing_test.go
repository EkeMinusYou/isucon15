package main

import "testing"

func TestUserHomeRemainsFixedAfterRegionChange(t *testing.T) {
	a := fixtureApp(t)
	uid, token := register(t, a, "fixed_home")
	if homeOfNewID(uid) != "asia" || homeOfNewID(token) != "asia" {
		t.Fatal("new identifiers lost their assigned home")
	}
	requireCall(t, a, "PATCH", "/api/me", token, object{"region": "europe"}, 200)
	u := a.s.Accounts.Get(uid)
	if u.Region != "europe" || u.HomeRegion != "asia" {
		t.Fatal("moving region changed the data master")
	}
}
