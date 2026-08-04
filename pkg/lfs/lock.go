package lfs

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"
)

// TagLFSLocks is the OCI tag name storing the LFS locks index manifest.
const TagLFSLocks = "_lfs_locks"

// LFSOwner represents the owner of a Git LFS file lock.
type LFSOwner struct {
	Name string `json:"name"`
}

// LFSLock represents a Git LFS file lock record.
type LFSLock struct {
	ID       string    `json:"id"`
	Path     string    `json:"path"`
	Owner    LFSOwner  `json:"owner"`
	LockedAt time.Time `json:"locked_at"`
}

// LFSLockList holds all active Git LFS file locks for a repository.
type LFSLockList struct {
	Locks []LFSLock `json:"locks"`
}

// GenerateLockID generates a deterministic ID for an LFS file lock.
func GenerateLockID(path, ownerName string, t time.Time) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s:%s:%d", path, ownerName, t.UnixNano())))
	return hex.EncodeToString(sum[:16])
}
