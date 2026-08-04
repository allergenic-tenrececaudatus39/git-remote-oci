package oci

import (
	"sync"
	"sync/atomic"
)

// Ceilings for the client's two caches.
//
// Both are sized to hold everything a normal command touches, so the eviction
// path below is reached only by a repository far larger than the caches were
// meant for.
const (
	// maxCachedManifests bounds the manifest cache. One entry is a parsed
	// manifest of a few hundred bytes, and a fetch stores two per commit — the
	// tag and the digest.
	maxCachedManifests = 8192

	// maxCachedPushedBlobs bounds the set of blob digests already uploaded.
	// One entry is a digest string.
	maxCachedPushedBlobs = 16384
)

// boundedMap is a concurrent map that will not grow without limit.
//
// The client's caches were plain sync.Maps that only ever grew. Nothing evicted
// and nothing was scoped to a single operation, so a fetch of a repository with
// a very long push history accumulated every manifest it had ever parsed and
// held them for the life of the process.
//
// Eviction is deliberately crude: at the ceiling the whole map is dropped
// rather than a victim chosen. These are within-command caches whose miss costs
// exactly one request that would have been made anyway, and a real LRU needs a
// mutex on every read — which is the cost sync.Map is here to avoid. Correctness
// never depends on a hit.
type boundedMap struct {
	m sync.Map
	// n is an approximation. It can drift under concurrent writers, which only
	// moves the point at which the map is dropped, never correctness.
	n   atomic.Int64
	max int64
}

func (b *boundedMap) limit() int64 {
	if b.max <= 0 {
		return maxCachedManifests
	}
	return b.max
}

func (b *boundedMap) Load(key any) (any, bool) {
	return b.m.Load(key)
}

func (b *boundedMap) Store(key, value any) {
	if b.n.Load() >= b.limit() {
		b.clear()
	}
	if _, loaded := b.m.Swap(key, value); !loaded {
		b.n.Add(1)
	}
}

func (b *boundedMap) Delete(key any) {
	if _, loaded := b.m.LoadAndDelete(key); loaded {
		b.n.Add(-1)
	}
}

func (b *boundedMap) Range(f func(key, value any) bool) {
	b.m.Range(f)
}

// clear drops every entry. A concurrent reader may still see an entry being
// removed, which is a cache miss and nothing more.
func (b *boundedMap) clear() {
	b.m.Range(func(key, _ any) bool {
		b.m.Delete(key)
		return true
	})
	b.n.Store(0)
}
