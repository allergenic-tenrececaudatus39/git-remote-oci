package oci

import "testing"

// TestBoundedMapStaysBounded pins the ceiling.
//
// The caches this replaces were plain sync.Maps that only ever grew, so a fetch
// of a repository with a long push history held every manifest it had parsed
// for the life of the process.
func TestBoundedMapStaysBounded(t *testing.T) {
	b := &boundedMap{max: 4}
	for i := range 100 {
		b.Store(i, i)
	}

	n := 0
	b.Range(func(any, any) bool { n++; return true })
	if n > 4 {
		t.Errorf("map holds %d entries, ceiling is 4", n)
	}
	if n == 0 {
		t.Error("map dropped everything and never refilled, so it is not a cache")
	}
}

// TestBoundedMapRoundTrips: a bounded cache is still a cache.
func TestBoundedMapRoundTrips(t *testing.T) {
	b := &boundedMap{max: 16}

	b.Store("k", "v")
	if got, ok := b.Load("k"); !ok || got != "v" {
		t.Fatalf("Load = %v, %v; want \"v\", true", got, ok)
	}

	b.Store("k", "v2")
	if got, _ := b.Load("k"); got != "v2" {
		t.Errorf("Store did not overwrite: %v", got)
	}

	b.Delete("k")
	if _, ok := b.Load("k"); ok {
		t.Error("entry survived Delete")
	}
	if got := b.n.Load(); got != 0 {
		t.Errorf("count = %d after deleting the only entry, want 0", got)
	}
}

// TestBoundedMapDefaultsWhenUnset guards the zero value: a boundedMap built
// without a ceiling must still have one.
func TestBoundedMapDefaultsWhenUnset(t *testing.T) {
	b := &boundedMap{}
	if b.limit() <= 0 {
		t.Fatalf("zero-value limit is %d, which is no limit at all", b.limit())
	}
}
