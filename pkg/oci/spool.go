package oci

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	opencontainers "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// A registry blob must be described by its digest and length before the upload
// can start, but a packfile is produced by a generator whose output size is not
// known in advance. Measuring it by buffering it costs as much memory as the
// packfile is large.
//
// spooledBlob stages the bytes on disk instead, digesting them as they are
// written, so memory stays flat regardless of how much history is being pushed.

// spoolDir picks somewhere with real disk behind it.
//
// $GIT_DIR is preferred over the system temporary directory because /tmp is a
// tmpfs on many distributions, which would put the artifact back into RAM and
// defeat the point.
func spoolDir() string {
	if gitDir := os.Getenv("GIT_DIR"); gitDir != "" {
		dir := filepath.Join(gitDir, "git-remote-oci-tmp")
		if err := os.MkdirAll(dir, 0755); err == nil {
			return dir
		}
	}
	return os.TempDir()
}

// spooledBlob is an artifact staged on disk together with its descriptor.
type spooledBlob struct {
	file *os.File
	desc ocispec.Descriptor
}

// Reader returns the staged content, positioned at the start.
func (s *spooledBlob) Reader() io.Reader { return s.file }

// Close releases the staged file. It is safe to call more than once.
func (s *spooledBlob) Close() error {
	if s.file == nil {
		return nil
	}
	name := s.file.Name()
	err := s.file.Close()
	s.file = nil
	if rmErr := os.Remove(name); err == nil && !os.IsNotExist(rmErr) {
		err = rmErr
	}
	return err
}

// spoolBlob stages everything write produces, digesting as it goes, and returns
// the staged file rewound to the start along with a descriptor for it.
//
// write receives the destination and reports how many *logical* bytes it
// consumed from its source, which differs from the number written whenever a
// compressor sits in between.
func spoolBlob(mediaType, namePrefix string, write func(io.Writer) (int64, error)) (blob *spooledBlob, logical int64, err error) {
	f, err := os.CreateTemp(spoolDir(), namePrefix+"-*")
	if err != nil {
		return nil, 0, fmt.Errorf("failed to create a spool file: %w", err)
	}
	defer func() {
		if err != nil {
			name := f.Name()
			_ = f.Close()
			_ = os.Remove(name)
		}
	}()

	digester := opencontainers.SHA256.Digester()
	logical, err = write(io.MultiWriter(f, digester.Hash()))
	if err != nil {
		return nil, 0, err
	}

	size, err := f.Seek(0, io.SeekCurrent)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to measure the spooled blob: %w", err)
	}
	if _, err = f.Seek(0, io.SeekStart); err != nil {
		return nil, 0, fmt.Errorf("failed to rewind the spooled blob: %w", err)
	}

	return &spooledBlob{
		file: f,
		desc: ocispec.Descriptor{MediaType: mediaType, Digest: digester.Digest(), Size: size},
	}, logical, nil
}
