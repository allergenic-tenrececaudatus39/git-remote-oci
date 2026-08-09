package cli

import (
	"context"
	"flag"
	"fmt"
	"sort"

	"github.com/mrueg/git-remote-oci/pkg/oci"
)

// runFsck checks that every published ref can actually be fetched.
//
// A registry validates nothing. It accepts any blob and any manifest whose
// blobs exist, and has no idea a packfile is a packfile, so the correctness of
// a repository rests entirely on whoever wrote it having been right. There is
// no server-side reachability check to fall back on, which is exactly why the
// pack-bases contract is a hard error at fetch time rather than a warning.
//
// This walks the same graph a fetch walks and reports what a fetch would hit,
// without downloading packfiles or touching the local repository. It cannot
// prove the objects inside a packfile are complete - only a real fetch does
// that - but it does catch the failure that matters: a manifest naming a base
// the registry no longer serves, which is a repository nobody can clone.
func runFsck(ctx context.Context, env Env) error {
	fs := flag.NewFlagSet("fsck", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	fs.Usage = func() {
		fmt.Fprint(env.Stderr, `usage: git-remote-oci fsck <oci-url>

Checks that every ref published in a repository is fetchable.

For each ref it follows io.git-remote-oci.pack-bases the way a fetch does, and
reports any manifest that is missing, malformed, or names a base the registry
does not serve. Nothing is downloaded and no local repository is needed.

Exits non-zero if any ref is unfetchable.
`)
	}
	if err := fs.Parse(env.Args[1:]); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return fmt.Errorf("fsck takes exactly one oci:// URL")
	}

	client, err := clientFor(env, fs.Arg(0))
	if err != nil {
		return err
	}

	refs, err := client.FetchRichRefIndex(ctx)
	if err != nil {
		if oci.IsNotFound(err) {
			fmt.Fprintln(env.Stdout, "the repository has no refs")
			return nil
		}
		return fmt.Errorf("failed to read the ref index: %w", err)
	}
	if len(refs) == 0 {
		fmt.Fprintln(env.Stdout, "the repository has no refs")
		return nil
	}

	refNames := make([]string, 0, len(refs))
	for name := range refs {
		refNames = append(refNames, name)
	}
	sort.Strings(refNames)

	// Shared across refs: repositories overwhelmingly share pack bases between
	// branches, and re-walking them per ref turns a linear check quadratic.
	checked := make(map[string]error)
	broken := 0

	for _, name := range refNames {
		entry := refs[name]
		if entry.SHA == "" {
			fmt.Fprintf(env.Stderr, "%s: no commit id recorded\n", name)
			broken++
			continue
		}
		// Start from the ref manifest, not from a commit id. For an annotated
		// tag the index records the tag object, and no manifest is tagged with
		// that - a fetch reaches it through the ref tag, so this must too.
		if err := checkRef(ctx, client, name, checked); err != nil {
			fmt.Fprintf(env.Stderr, "%s: %v\n", name, err)
			broken++
			continue
		}
		fmt.Fprintf(env.Stdout, "%s ok\n", name)
	}

	head, headErr := client.FetchHead(ctx)
	switch {
	case headErr != nil:
		fmt.Fprintf(env.Stderr, "could not read the recorded HEAD: %v\n", headErr)
	case head == "":
		fmt.Fprintln(env.Stdout, "HEAD: not recorded; readers will guess")
	default:
		if _, live := refs[head]; !live {
			fmt.Fprintf(env.Stderr, "HEAD points at %s, which is not a published ref\n", head)
			broken++
		} else {
			fmt.Fprintf(env.Stdout, "HEAD -> %s\n", head)
		}
	}

	if broken > 0 {
		return fmt.Errorf("%d of %d refs are not fetchable", broken, len(refNames))
	}
	fmt.Fprintf(env.Stdout, "all %d refs are fetchable\n", len(refNames))
	return nil
}

// checkRef verifies one ref the way a fetch resolves it: through the ref
// manifest, then down its declared pack bases.
func checkRef(ctx context.Context, client *oci.Client, refName string, checked map[string]error) error {
	desc, err := client.ResolveRefManifest(ctx, refName)
	if err != nil {
		return fmt.Errorf("no ref manifest on the registry: %w", err)
	}
	manifest, err := client.FetchManifest(ctx, desc.Digest.String())
	if err != nil {
		return fmt.Errorf("ref manifest could not be read: %w", err)
	}

	bases, err := oci.ParsePackBases(manifest.Annotations)
	if err != nil {
		return err
	}
	for _, base := range bases {
		if err := walkPackBases(ctx, client, base, checked, nil); err != nil {
			return fmt.Errorf("packed against %s, which is not fetchable: %w", short(base), err)
		}
	}
	return nil
}

// walkPackBases follows a commit's declared pack bases, as a fetch would.
//
// checked memoises results across refs. path carries the chain in progress so a
// cycle - which registry content could describe, since nothing validates it -
// is reported instead of recursing forever.
func walkPackBases(ctx context.Context, client *oci.Client, sha string, checked map[string]error, path []string) error {
	if result, seen := checked[sha]; seen {
		return result
	}
	for _, ancestor := range path {
		if ancestor == sha {
			return fmt.Errorf("pack bases form a cycle through %s", short(sha))
		}
	}

	manifest, err := client.FetchManifest(ctx, sha)
	if err != nil {
		result := fmt.Errorf("commit %s has no manifest on the registry: %w", short(sha), err)
		checked[sha] = result
		return result
	}

	bases, err := oci.ParsePackBases(manifest.Annotations)
	if err != nil {
		result := fmt.Errorf("commit %s: %w", short(sha), err)
		checked[sha] = result
		return result
	}

	for _, base := range bases {
		if err := walkPackBases(ctx, client, base, checked, append(path, sha)); err != nil {
			result := fmt.Errorf("commit %s was packed against %s, which is not fetchable: %w", short(sha), short(base), err)
			checked[sha] = result
			return result
		}
	}

	checked[sha] = nil
	return nil
}

// short abbreviates a commit id for a message.
func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
