package oci

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sort"
	"strings"

	opencontainers "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// A packfile's object index: the ids it contains, published beside it.
//
// Every other annotation in this format is about commits — what a manifest's
// parents are, what its packfile was cut against, which ref it belongs to.
// None of them answer the question a partial clone asks, which is "which
// packfile holds this blob". Without an answer the only way to find out is to
// download packfiles and index them until the object turns up, which costs the
// repository to serve one object.
//
// So each packfile layer gets a sibling listing what is in it. A reader
// fetches the index — tens of bytes per object, against the object itself —
// and knows whether the pack is worth downloading at all.

// MediaTypeGitPackIndex is the layer listing a packfile's object ids.
const MediaTypeGitPackIndex = "application/vnd.git.repository.packindex.v1"

// packIndexLineLen is the on-disk stride for a SHA-1 index: forty hex digits
// and a newline. SHA-256 repositories use 64 + 1. The width is read from the
// blob rather than configured, which is the same rule the rest of the format
// follows: the algorithm is derived from the ids, so the two cannot disagree.
func packIndexStride(data []byte) (int, bool) {
	i := bytes.IndexByte(data, '\n')
	if i != 40 && i != 64 {
		return 0, false
	}
	return i + 1, true
}

// EncodePackIndex renders sorted object ids as an index blob.
//
// Fixed-width lines, so a reader can seek to the middle of the blob and
// binary-search without parsing what came before it. Hex rather than raw bytes
// because the width then says which hash algorithm this repository uses, and
// because an index that can be read with `less` is worth a few bytes.
func EncodePackIndex(oids []string) []byte {
	// Normalise first, sort second. Sorting the input and lowercasing on the
	// way out looks equivalent and is not: 'A' sorts before 'a', so a mixed-case
	// input comes out in an order the reader's binary search does not expect,
	// and the search then reports objects absent that are sitting in the blob.
	// A false absence is the one answer this must never give.
	normalised := make([]string, 0, len(oids))
	for _, oid := range oids {
		// Hex, not merely the right length. A forty-byte string that happens to
		// contain a newline would put the line break somewhere other than the
		// stride and make the whole blob unreadable -- which a reader has to
		// treat as "nothing known", silently costing the download this exists
		// to avoid. Dropping an entry is harmless by comparison: an absent id
		// only means a pack fetched that need not have been.
		if !isObjectID(oid) {
			continue
		}
		normalised = append(normalised, strings.ToLower(oid))
	}
	sort.Strings(normalised)

	// One width, or none at all. The stride is what makes the blob searchable
	// and it is read from the first line, so a SHA-1 id and a SHA-256 id in the
	// same index produce a ragged blob that is misread from the second entry
	// on. A repository is one algorithm throughout (FORMAT.md §10), so ids of
	// two widths are not a repository -- and publishing nothing leaves a reader
	// with "unknown", which it already knows how to handle, rather than an
	// index that answers confidently and wrongly.
	for _, oid := range normalised {
		if len(oid) != len(normalised[0]) {
			return nil
		}
	}

	var buf bytes.Buffer
	for _, oid := range normalised {
		buf.WriteString(oid)
		buf.WriteByte('\n')
	}
	return buf.Bytes()
}

// PackIndexContains reports whether an index blob lists any of the given ids.
//
// Any, not all: the caller is deciding whether a packfile is worth fetching,
// and one wanted object in it is reason enough.
//
// A blob it cannot make sense of reports true. The index is an optimisation,
// and the safe direction to fail in is fetching a pack that turns out not to
// help — the alternative is skipping the pack that had the object and telling
// the client its object does not exist.
func PackIndexContains(index []byte, oids []string) bool {
	stride, ok := packIndexStride(index)
	if !ok {
		// Unreadable and not empty: nothing is known about this pack, so it
		// cannot be ruled out. An empty index is different — it was written,
		// and it says the pack holds nothing.
		return len(index) != 0
	}
	n := len(index) / stride

	for _, oid := range oids {
		want := strings.ToLower(oid)
		if len(want)+1 != stride {
			// A different width to this index: a SHA-256 id cannot be in a
			// SHA-1 repository's pack.
			continue
		}
		i := sort.Search(n, func(i int) bool {
			return string(index[i*stride:i*stride+stride-1]) >= want
		})
		if i < n && string(index[i*stride:i*stride+stride-1]) == want {
			return true
		}
	}
	return false
}

// PushPackIndex uploads an index blob and returns its descriptor, for the
// caller to attach to the manifest as a layer.
func (c *Client) PushPackIndex(ctx context.Context, oids []string) (ocispec.Descriptor, error) {
	data := EncodePackIndex(oids)
	if len(data) == 0 {
		return ocispec.Descriptor{}, nil
	}

	desc := ocispec.Descriptor{
		MediaType: MediaTypeGitPackIndex,
		Digest:    opencontainers.FromBytes(data),
		Size:      int64(len(data)),
	}
	if err := c.pushBlobOnce(ctx, desc, data); err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("failed to push the pack index: %w", err)
	}
	return desc, nil
}

// PackIndexLayer returns a manifest's pack index layer, if it has one.
//
// Manifests written before this layer existed do not, and a reader must treat
// that as "unknown" rather than "empty" — see FetchPackIndex.
func PackIndexLayer(manifest *ocispec.Manifest) (ocispec.Descriptor, bool) {
	if manifest == nil {
		return ocispec.Descriptor{}, false
	}
	for _, layer := range manifest.Layers {
		if layer.MediaType == MediaTypeGitPackIndex {
			return layer, true
		}
	}
	return ocispec.Descriptor{}, false
}

// FetchPackIndex downloads a manifest's pack index.
//
// ok is false when the manifest has no index or it could not be read, which
// are the same thing to a caller: nothing is known about what that packfile
// holds, so it cannot be ruled out. A repository pushed by an older build has
// no indexes at all and must keep working, just without the shortcut.
func (c *Client) FetchPackIndex(ctx context.Context, manifest *ocispec.Manifest) ([]byte, bool) {
	desc, found := PackIndexLayer(manifest)
	if !found {
		return nil, false
	}
	rc, err := c.Repo.Fetch(ctx, desc)
	if err != nil {
		return nil, false
	}
	defer func() { _ = rc.Close() }()

	data, err := io.ReadAll(io.LimitReader(rc, desc.Size))
	if err != nil {
		return nil, false
	}
	return data, true
}
