package main

import (
	"maps"
	"slices"
	"testing"
)

func TestOverlayIterationMergesChangesAndStopsEarly(t *testing.T) {
	base := new(shardedMap[string, int])
	base.Set("kept", 1)
	base.Set("updated", 2)
	base.Set("deleted", 3)
	changed := forkMap(base, nil)
	changed.Set("updated", 20)
	changed.Delete("deleted")
	changed.Set("added", 4)
	nested := forkMap(changed, nil)
	nested.Delete("added")
	nested.Set("new", 5)
	got := map[string]int{}
	for key, value := range nested.All() {
		if _, seen := got[key]; seen {
			t.Fatal("duplicate iteration key")
		}
		got[key] = value
		if nested.Get(key) != value {
			t.Fatal("iteration differs from lookup")
		}
	}
	if !maps.Equal(got, map[string]int{"kept": 1, "updated": 20, "new": 5}) {
		t.Fatalf("merged values: %v", got)
	}
	if !maps.Equal(base.Snapshot(), map[string]int{"kept": 1, "updated": 2, "deleted": 3}) {
		t.Fatal("iteration changed the parent")
	}
	count := 0
	for range nested.All() {
		count++
		break
	}
	if count != 1 {
		t.Fatal("iteration did not stop early")
	}
	nested.Set("after", 6)
	if nested.Get("after") != 6 {
		t.Fatal("early stop retained a map lock")
	}
}

func TestOverlaySnapshotOwnsMutableValues(t *testing.T) {
	base := new(shardedMap[string, []int])
	base.Set("values", []int{1, 2})
	child := forkMap(base, slices.Clone[[]int])
	values := child.Snapshot()
	values["values"][0] = 7
	if base.Get("values")[0] != 1 || child.Get("values")[0] != 7 {
		t.Fatal("mutable snapshot shared its parent's storage")
	}
}
