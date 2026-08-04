package oci

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/mrueg/git-remote-oci/pkg/lfs"
	opencontainers "github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// lfsLocksLockRef is the pseudo-ref whose ref lock serialises updates to the
// _lfs_locks manifest. Acquiring and releasing an LFS lock is a
// read-modify-write of one blob, so concurrent callers must take turns.
const lfsLocksLockRef = "_lfs_locks_index_lock"

// lfsLocksIndexWait bounds how long a client waits for the LFS lock index.
// How long it may hold it is Client.LFSLocksIndexTTL.
const lfsLocksIndexWait = 45 * time.Second

// withLFSLocksIndexLock takes the ref lock that serialises updates to the
// _lfs_locks manifest and returns a function that releases it.
func (c *Client) withLFSLocksIndexLock(ctx context.Context) (func(), error) {
	if _, err := c.AcquireRefLockWithRetry(ctx, lfsLocksLockRef, c.LFSLocksIndexTTL, lfsLocksIndexWait); err != nil {
		return nil, fmt.Errorf("failed to acquire the LFS lock index: %w", err)
	}
	return func() {
		// Use a context detached from ctx: if the caller's context has already
		// been cancelled, the release still needs to reach the registry, or the
		// index stays locked until its TTL expires.
		releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if err := c.ReleaseRefLock(releaseCtx, lfsLocksLockRef); err != nil {
			fmt.Fprintf(os.Stderr, "git-remote-oci: warning: failed to release the LFS lock index: %v\n", err)
		}
	}, nil
}

// defaultLockOwner derives a lock owner identity from the environment.
func defaultLockOwner() string {
	user := os.Getenv("USER")
	if user == "" {
		user = "git-user"
	}
	hostname, err := os.Hostname()
	if err != nil || hostname == "" {
		hostname = "localhost"
	}
	return fmt.Sprintf("%s@%s", user, hostname)
}

// FetchLFSLocks fetches the list of active Git LFS file locks from the OCI registry.
//
// A missing _lfs_locks manifest means "no locks yet" and yields an empty list.
// Every other failure is returned: callers use this list as the base of a
// read-modify-write, so treating an unreadable list as empty would republish a
// lock list that silently drops everyone else's locks.
func (c *Client) FetchLFSLocks(ctx context.Context) ([]lfs.LFSLock, error) {
	_, rc, err := c.Repo.FetchReference(ctx, lfs.TagLFSLocks)
	if err != nil {
		if IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to fetch LFS lock manifest: %w", err)
	}
	defer func() { _ = rc.Close() }()

	data, err := io.ReadAll(rc)
	if err != nil {
		return nil, fmt.Errorf("failed to read LFS lock manifest: %w", err)
	}

	var manifest ocispec.Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil, fmt.Errorf("failed to parse LFS lock manifest: %w", err)
	}

	configRc, err := c.Repo.Fetch(ctx, manifest.Config)
	if err != nil {
		if IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to fetch LFS lock list blob: %w", err)
	}
	defer func() { _ = configRc.Close() }()

	configData, err := io.ReadAll(configRc)
	if err != nil {
		return nil, fmt.Errorf("failed to read LFS lock list blob: %w", err)
	}

	var lockList lfs.LFSLockList
	if err := json.Unmarshal(configData, &lockList); err != nil {
		return nil, fmt.Errorf("failed to parse LFS lock list: %w", err)
	}

	return lockList.Locks, nil
}

// FindLFSLockByPath returns the lock held on path, or nil if there is none.
//
// Separate from releasing so a caller that only has a path resolves it
// explicitly and can report which lock it is about to release.
func (c *Client) FindLFSLockByPath(ctx context.Context, path string) (*lfs.LFSLock, error) {
	locks, err := c.FetchLFSLocks(ctx)
	if err != nil {
		return nil, err
	}
	for i := range locks {
		if locks[i].Path == path {
			return &locks[i], nil
		}
	}
	return nil, nil
}

// AcquireLFSLock acquires a Git LFS lock on path for ownerName.
func (c *Client) AcquireLFSLock(ctx context.Context, path string, ownerName string) (*lfs.LFSLock, error) {
	if path == "" {
		return nil, errors.New("file path cannot be empty")
	}
	if ownerName == "" {
		ownerName = defaultLockOwner()
	}

	// Acquiring an LFS lock is a read-modify-write of a single shared blob.
	// Serialise it, or two concurrent lockers each publish a list containing
	// only their own lock and one of them silently disappears.
	release, err := c.withLFSLocksIndexLock(ctx)
	if err != nil {
		return nil, err
	}
	defer release()

	existingLocks, err := c.FetchLFSLocks(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch existing LFS locks: %w", err)
	}

	for _, lock := range existingLocks {
		if lock.Path == path {
			return nil, fmt.Errorf("path %q is already locked by %s (lock ID: %s)", path, lock.Owner.Name, lock.ID)
		}
	}

	now := time.Now().UTC()
	lockID := lfs.GenerateLockID(path, ownerName, now)
	newLock := lfs.LFSLock{
		ID:       lockID,
		Path:     path,
		Owner:    lfs.LFSOwner{Name: ownerName},
		LockedAt: now,
	}

	// Copy rather than append in place: existingLocks may share a backing array
	// with the caller's slice.
	updatedLocks := make([]lfs.LFSLock, 0, len(existingLocks)+1)
	updatedLocks = append(updatedLocks, existingLocks...)
	updatedLocks = append(updatedLocks, newLock)
	lockList := lfs.LFSLockList{Locks: updatedLocks}
	lockListJSON, err := json.Marshal(lockList)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal LFS lock list: %w", err)
	}

	configDesc := ocispec.Descriptor{
		MediaType: MediaTypeGitConfig,
		Digest:    opencontainers.FromBytes(lockListJSON),
		Size:      int64(len(lockListJSON)),
	}
	if err := c.pushBlobOnce(ctx, configDesc, lockListJSON); err != nil {
		return nil, fmt.Errorf("failed to push LFS lock config blob: %w", err)
	}
	if err := c.pushEmptyBlob(ctx); err != nil {
		return nil, err
	}

	manifest := ocispec.Manifest{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageManifest,
		Config:    configDesc,
		Layers:    emptyLayers(),
		Annotations: map[string]string{
			"org.git.lfs.locks.count": fmt.Sprintf("%d", len(updatedLocks)),
		},
	}

	manifestData, err := json.Marshal(manifest)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal LFS lock manifest: %w", err)
	}

	manifestDesc := ocispec.Descriptor{
		MediaType: ocispec.MediaTypeImageManifest,
		Digest:    opencontainers.FromBytes(manifestData),
		Size:      int64(len(manifestData)),
	}

	if err := c.Repo.PushReference(ctx, manifestDesc, bytes.NewReader(manifestData), lfs.TagLFSLocks); err != nil {
		return nil, fmt.Errorf("failed to push LFS lock manifest %s: %w", lfs.TagLFSLocks, err)
	}

	c.manifestCache.Store(lfs.TagLFSLocks, &manifest)
	return &newLock, nil
}

// ReleaseLFSLock releases a Git LFS lock by ID or path.
//
// Unless force is set, ownerName must match the lock holder. An empty ownerName
// is resolved from the environment rather than skipping the check, so an
// unnamed caller cannot unlock someone else's file by omission.
func (c *Client) ReleaseLFSLock(ctx context.Context, lockID string, force bool, ownerName string) (*lfs.LFSLock, error) {
	if !force && ownerName == "" {
		ownerName = defaultLockOwner()
	}

	// Same read-modify-write hazard as AcquireLFSLock.
	release, err := c.withLFSLocksIndexLock(ctx)
	if err != nil {
		return nil, err
	}
	defer release()

	existingLocks, err := c.FetchLFSLocks(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch existing LFS locks: %w", err)
	}

	var targetLock *lfs.LFSLock
	remainingLocks := make([]lfs.LFSLock, 0, len(existingLocks))

	for _, lock := range existingLocks {
		// Match on the id only. Accepting a path here as well meant a path that
		// happened to equal another lock's id released the wrong record, and
		// there was no way for a caller to say which it meant. Callers that
		// hold a path resolve it to an id first; see FindLFSLockByPath.
		if lock.ID == lockID {
			l := lock
			targetLock = &l
		} else {
			remainingLocks = append(remainingLocks, lock)
		}
	}

	if targetLock == nil {
		return nil, fmt.Errorf("lock ID %q not found", lockID)
	}

	if !force && targetLock.Owner.Name != ownerName {
		return nil, fmt.Errorf("lock %s is owned by %s, cannot unlock without force", lockID, targetLock.Owner.Name)
	}

	lockList := lfs.LFSLockList{Locks: remainingLocks}
	lockListJSON, err := json.Marshal(lockList)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal remaining LFS locks: %w", err)
	}

	configDesc := ocispec.Descriptor{
		MediaType: MediaTypeGitConfig,
		Digest:    opencontainers.FromBytes(lockListJSON),
		Size:      int64(len(lockListJSON)),
	}
	if err := c.pushBlobOnce(ctx, configDesc, lockListJSON); err != nil {
		return nil, fmt.Errorf("failed to push LFS lock config blob: %w", err)
	}
	if err := c.pushEmptyBlob(ctx); err != nil {
		return nil, err
	}

	manifest := ocispec.Manifest{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageManifest,
		Config:    configDesc,
		Layers:    emptyLayers(),
		Annotations: map[string]string{
			"org.git.lfs.locks.count": fmt.Sprintf("%d", len(remainingLocks)),
		},
	}

	manifestData, err := json.Marshal(manifest)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal LFS lock manifest: %w", err)
	}

	manifestDesc := ocispec.Descriptor{
		MediaType: ocispec.MediaTypeImageManifest,
		Digest:    opencontainers.FromBytes(manifestData),
		Size:      int64(len(manifestData)),
	}

	if err := c.Repo.PushReference(ctx, manifestDesc, bytes.NewReader(manifestData), lfs.TagLFSLocks); err != nil {
		return nil, fmt.Errorf("failed to push LFS lock manifest %s: %w", lfs.TagLFSLocks, err)
	}

	c.manifestCache.Store(lfs.TagLFSLocks, &manifest)
	return targetLock, nil
}
