package helper

import (
	"context"
	"fmt"
	"strings"

	"github.com/mrueg/git-remote-oci/pkg/git"
)

// The v2 `object-info` command: how big is this object, without sending it.
//
// A partial clone is the caller. `--filter=blob:limit=1m` has to decide per
// blob whether it wants the thing, and asking costs a size rather than a
// download; `git cat-file --batch-check` against a promisor remote goes the
// same way. Without it a client that only wanted to know has to fetch to find
// out, which for the filter is precisely backwards.
//
// The honest caveat is that this remote cannot answer for free. A registry
// stores packfiles, not an object table, so a size that is not already on disk
// is found by fetching the packfile that holds it — the same work a fetch would
// do, minus sending the result. That still wins whenever the answer is "no,
// too big" for several objects in one pack, and it is never worse than the
// fetch the client would otherwise have issued. The pack index (FORMAT.md §4.4)
// narrows which packs get downloaded, exactly as it does for a lazy fetch.

// v2ObjectInfo serves `command=object-info`.
//
// Request arguments are an attribute list — only `size` is defined, and it is
// the only one advertised — followed by `oid <id>` lines. The response repeats
// the attribute line and then gives one `<oid> <size>` line per request, in the
// order asked.
func (h *Helper) v2ObjectInfo(ctx context.Context, w *pktWriter, req v2Request) error {
	var oids []string
	wantSize := false
	for _, arg := range req.args {
		switch {
		case arg == "size":
			wantSize = true
		case strings.HasPrefix(arg, "oid "):
			oid := strings.TrimSpace(strings.TrimPrefix(arg, "oid "))
			if oid != "" {
				oids = append(oids, oid)
			}
		}
	}

	// Nothing else is advertised, so anything else is git asking for something
	// it was never offered. Answering with sizes it did not request would be a
	// response it cannot parse.
	if !wantSize {
		if err := sendV2Error(w, "protocol v2: object-info supports only the size attribute"); err != nil {
			return err
		}
		return endResponse(w)
	}
	if len(oids) == 0 {
		if err := w.WriteLine("size"); err != nil {
			return err
		}
		return endResponse(w)
	}

	sizes, err := h.objectSizes(ctx, oids)
	if err != nil {
		// An ERR packet rather than a dropped connection: git reports the
		// former as the remote's own message and the latter as "the remote end
		// hung up unexpectedly", which names neither the cause nor the side.
		if sendErr := sendV2Error(w, fmt.Sprintf("protocol v2: %v", err)); sendErr != nil {
			return sendErr
		}
		return endResponse(w)
	}

	if err := w.WriteLine("size"); err != nil {
		return err
	}
	for _, oid := range oids {
		if err := w.WriteLine("%s %d", oid, sizes[oid]); err != nil {
			return err
		}
	}
	return endResponse(w)
}

// objectSizes resolves the size of every requested object, fetching whatever it
// takes to be able to.
//
// The local store is tried first and often answers: an object-info request
// during a partial clone frequently asks about things a previous fetch already
// brought down. Only what is left drives a staging pass, which is the same
// search a lazy fetch performs — ref by ref, skipping any whose published pack
// index rules it out.
func (h *Helper) objectSizes(ctx context.Context, oids []string) (map[string]int64, error) {
	if err := h.ensureGitRepo(); err != nil {
		return nil, err
	}

	// `size` is a fixed cost per object, so there is no progress worth
	// narrating and no filter to apply: the client asked about these
	// specifically, and skipping one would leave a line out of the response.
	st, cleanup, err := h.newStagingArea("", false)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	store, err := git.OpenObjectStore(st.dir)
	if err != nil {
		return nil, fmt.Errorf("could not read the staged object store: %w", err)
	}
	sizes, missing := git.ObjectSizes(store, oids)
	if len(missing) == 0 {
		return sizes, nil
	}

	h.logVerbose("git-remote-oci: [verbose] object-info: %d of %d objects are not local; searching\n",
		len(missing), len(oids))
	if err := h.stageUntilFound(ctx, missing, st, errWantNotServed); err != nil {
		return nil, err
	}

	// Re-open: the store cached the pack list it found when it was opened, and
	// staging has added to it since.
	staged, err := git.OpenObjectStore(st.dir)
	if err != nil {
		return nil, fmt.Errorf("could not re-read the staged object store: %w", err)
	}
	found, stillMissing := git.ObjectSizes(staged, missing)
	if len(stillMissing) > 0 {
		return nil, fmt.Errorf("object-info: %s is not something this remote can serve", stillMissing[0])
	}
	for oid, size := range found {
		sizes[oid] = size
	}
	return sizes, nil
}
