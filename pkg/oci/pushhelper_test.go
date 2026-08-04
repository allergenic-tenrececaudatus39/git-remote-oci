package oci_test

import (
	"bytes"
	"context"

	"github.com/mrueg/git-remote-oci/pkg/oci"
)

// pushCommitImage publishes a small in-memory packfile.
//
// This was oci.PushCommitImage: exported production API whose only callers were
// these tests. Keeping it as a test helper takes it out of the package's public
// surface without changing what the tests do, since it is only a buffered
// PushCommitStream.
func pushCommitImage(ctx context.Context, client *oci.Client, commitSHA, refName, refTag, parents string, data []byte) error {
	return client.PushCommitStream(ctx, oci.CommitPush{
		CommitSHA:   commitSHA,
		RefName:     refName,
		RefTag:      refTag,
		Parents:     parents,
		UpdateIndex: true,
	}, bytes.NewReader(data), int64(len(data)))
}
