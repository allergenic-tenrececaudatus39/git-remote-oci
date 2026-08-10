package oci

import (
	"context"
	"encoding/json"
	"io"
	"sort"

	opencontainers "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// The shape of the pack-base graph, published in one place.
//
// Every push writes a thin packfile cut against the previous push's tip, so
// `io.git-remote-oci.pack-bases` (§4.2) forms a chain: push N names push N-1,
// which names push N-2. A reader that learns the chain by reading those
// annotations learns exactly one link per request, and the links are strictly
// sequential — there is nothing to parallelise, because the reader does not
// know what to ask for next until the current answer arrives.
//
// That makes clone latency linear in the number of pushes since the last gc. A
// repository with five hundred pushes costs five hundred sequential round trips
// before a single byte of packfile moves, which on a 100ms link is most of a
// minute of doing nothing. The 50000-manifest ceiling in the fetch path, and
// the "run gc" advice in the error when it is hit, are both symptoms of this.
//
// So the chain is published as a whole, on the `_refs` manifest that every
// operation already reads. A reader that has it knows the full set of manifests
// it needs up front and fetches them in one parallel wave.

// MediaTypePackChain is the layer holding the whole pack-base graph.
const MediaTypePackChain = "application/vnd.git.repository.packchain.v1+json"

// FetchPackChain reads the published pack-base graph: commit id to the commits
// its packfile was cut against.
//
// ok is false when the repository publishes no chain, which every repository
// written before this layer existed does. The chain is an accelerator and
// nothing depends on it being present.
//
// The result is cached for the life of the client. It is read on the fetch path
// and again when a push republishes it, and re-reading it would spend the round
// trip this exists to save.
func (c *Client) FetchPackChain(ctx context.Context) (map[string][]string, bool) {
	if cached, ok := c.packChain.Load().(map[string][]string); ok {
		return cached, len(cached) > 0
	}

	chain := c.fetchPackChainUncached(ctx)
	c.packChain.Store(chain)
	return chain, len(chain) > 0
}

func (c *Client) fetchPackChainUncached(ctx context.Context) map[string][]string {
	manifest, err := c.FetchManifest(ctx, TagRefIndex)
	if err != nil {
		return map[string][]string{}
	}

	var desc *ocispec.Descriptor
	for i := range manifest.Layers {
		if manifest.Layers[i].MediaType == MediaTypePackChain {
			desc = &manifest.Layers[i]
			break
		}
	}
	if desc == nil {
		return map[string][]string{}
	}

	rc, err := c.Repo.Fetch(ctx, *desc)
	if err != nil {
		return map[string][]string{}
	}
	defer func() { _ = rc.Close() }()

	data, err := io.ReadAll(io.LimitReader(rc, desc.Size))
	if err != nil {
		return map[string][]string{}
	}

	var chain map[string][]string
	if err := json.Unmarshal(data, &chain); err != nil {
		// A chain that cannot be parsed is treated as one that is not there.
		// Every use of it is an optimisation over reading the annotations, so
		// discarding it costs round trips and nothing else.
		return map[string][]string{}
	}
	return sanitisePackChain(chain)
}

// sanitisePackChain drops anything that is not a pair of object ids.
//
// Everything in here came out of a blob the registry served, and every id in it
// becomes a tag on the next request. ParsePackBases validates the annotation
// form for exactly that reason -- "these become tag names on the next request,
// so they are validated here rather than at the point of use" -- and the chain
// is the same data arriving by a different route, so it gets the same
// treatment at the same boundary.
//
// Dropping rather than rejecting: an entry that cannot be trusted is an entry
// the reader has no shortcut for, which puts it back on the annotation walk.
// That is the same outcome as a chain that never mentioned it.
func sanitisePackChain(chain map[string][]string) map[string][]string {
	clean := make(map[string][]string, len(chain))
	for sha, bases := range chain {
		if !isObjectID(sha) {
			continue
		}
		checked := make([]string, 0, len(bases))
		bad := false
		for _, base := range bases {
			if !isObjectID(base) {
				bad = true
				break
			}
			checked = append(checked, base)
		}
		if bad {
			// A partially readable edge list is worse than none: it would look
			// like a complete answer with a base missing, which is the one
			// thing the chain must never do.
			continue
		}
		clean[sha] = checked
	}
	return clean
}

// recordPackChain notes that commitSHA's packfile was cut against bases, so the
// next `_refs` push can publish it.
//
// An empty bases list is recorded, not skipped: "this packfile stands alone" is
// where a reader stops walking, and leaving it out would be indistinguishable
// from a commit the chain says nothing about.
func (c *Client) recordPackChain(commitSHA string, bases []string) {
	if commitSHA == "" {
		return
	}
	recorded := make([]string, 0, len(bases))
	recorded = append(recorded, bases...)
	c.packChainEdges.Store(commitSHA, recorded)
}

// ResetPackChain discards the published chain, so the next `_refs` push writes
// only the edges recorded since.
//
// This is for gc. Consolidation rewrites every ref as a self-contained packfile
// and prunes the intermediate commit manifests, so every edge in the old chain
// names a manifest that is about to stop existing. Merging with it would carry
// that wreckage forward for the life of the repository, which is the opposite
// of what the run was for.
func (c *Client) ResetPackChain() {
	c.packChainReset.Store(true)
	c.packChain.Store(map[string][]string{})
	// Edges this client recorded earlier go too. A client that pushed and then
	// ran gc in the same process would otherwise carry its own pushes' edges
	// past the consolidation that superseded them -- rare in production, where
	// gc is its own command, and exactly what a test doing both would hit.
	c.packChainEdges.Range(func(k, _ any) bool {
		c.packChainEdges.Delete(k)
		return true
	})
}

// packChainLayer builds the chain blob to attach to the `_refs` manifest, and
// returns ok=false when there is nothing to publish.
func (c *Client) packChainLayer(ctx context.Context) (ocispec.Descriptor, bool) {
	merged := map[string][]string{}
	if !c.packChainReset.Load() {
		for sha, bases := range c.fetchPackChainCached(ctx) {
			merged[sha] = bases
		}
	}
	c.packChainEdges.Range(func(k, v any) bool {
		sha, _ := k.(string)
		bases, _ := v.([]string)
		merged[sha] = bases
		return true
	})
	// Publishing only what a reader would accept, so a malformed entry is a
	// bug caught here rather than a shortcut silently lost at every clone.
	merged = sanitisePackChain(merged)
	if len(merged) == 0 {
		return ocispec.Descriptor{}, false
	}

	// Sorted, so the same graph produces the same digest and a re-push of an
	// unchanged repository does not upload a new blob. Go's map iteration would
	// otherwise make every push look like a change.
	for _, bases := range merged {
		sort.Strings(bases)
	}
	// encoding/json sorts map keys, so an unchanged graph marshals to the same
	// bytes and the same digest, and the registry skips the upload.
	data, err := json.Marshal(merged)
	if err != nil {
		return ocispec.Descriptor{}, false
	}

	desc := ocispec.Descriptor{
		MediaType: MediaTypePackChain,
		Digest:    opencontainers.FromBytes(data),
		Size:      int64(len(data)),
	}
	if err := c.pushBlobOnce(ctx, desc, data); err != nil {
		// The chain is an accelerator. A push that could not publish it is
		// still a correct push, and failing here would trade a working push
		// for a faster clone.
		return ocispec.Descriptor{}, false
	}
	// What was just published is what a reader would now get, so the cache can
	// be brought forward rather than invalidated.
	c.packChain.Store(merged)
	return desc, true
}

// fetchPackChainCached is FetchPackChain without the ok, for internal callers
// that treat absent and empty the same way.
func (c *Client) fetchPackChainCached(ctx context.Context) map[string][]string {
	chain, _ := c.FetchPackChain(ctx)
	return chain
}
