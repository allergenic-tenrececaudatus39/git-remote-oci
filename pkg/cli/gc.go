package cli

import (
	"context"
	"flag"
	"fmt"

	"github.com/mrueg/git-remote-oci/pkg/gc"
	"github.com/mrueg/git-remote-oci/pkg/git"
)

func runGC(ctx context.Context, env Env) error {
	fs := flag.NewFlagSet("gc", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	dryRun := fs.Bool("dry-run", false, "report what would change without modifying the registry")
	fs.Usage = func() {
		fmt.Fprint(env.Stderr, `usage: git-remote-oci gc [flags] <oci-url>

Compacts a repository stored in an OCI registry.

Every push writes its own packfile and its own commit-SHA tag, and nothing
removes them, so a long-lived repository accumulates one of each per push.
This rewrites every ref as a single self-contained packfile and then prunes the
commit manifests that are no longer needed, along with released or expired
locks.

Run it from a clone that contains every commit the remote refs point at; the
consolidated packfiles are built from local objects.

Flags:
`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(env.Args[1:]); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return fmt.Errorf("gc takes exactly one oci:// URL")
	}

	client, err := clientFor(env, fs.Arg(0))
	if err != nil {
		return err
	}

	repo, err := git.OpenRepository()
	if err != nil {
		return fmt.Errorf("gc must run inside a git repository holding the commits to repack: %w", err)
	}

	logf := func(format string, a ...any) { fmt.Fprintf(env.Stderr, format, a...) }
	result, err := gc.Run(ctx, client, repo, gc.Options{DryRun: *dryRun, Logf: logf})
	if err != nil {
		return err
	}

	verb := "removed"
	if *dryRun {
		verb = "would remove"
	}
	fmt.Fprintf(env.Stdout,
		"repacked %d refs; %s %d commit manifests and %d lock tags (%d tags -> %d)\n",
		result.RefsConsolidated, verb, result.CommitTagsPruned, result.LockTagsPruned,
		result.TagsBefore, result.TagsAfter)
	return nil
}
