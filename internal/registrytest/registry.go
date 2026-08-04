// Package registrytest provides an in-process OCI registry and a seeded
// repository for tests.
//
// It exists because the mock was being written again for each package that
// needed one, and the copies drifted: one could not serve a manifest by digest,
// so every deletion assertion in that package silently passed without a DELETE
// ever being issued. A registry mock is fiddly in exactly the ways that matter
// — deletion arrives by digest, not by tag — so there should be one.
//
// It is in internal/ rather than pkg/ because it is test scaffolding, not part
// of the tool's own dependency graph.
package registrytest

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/go-git/go-git/v6/plumbing"
	opencontainers "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/mrueg/git-remote-oci/pkg/git"
	"github.com/mrueg/git-remote-oci/pkg/oci"
)

// Registry is an in-process OCI registry with enough behaviour for a push, a
// fetch, a garbage collection and an fsck.
type Registry struct {
	mu        sync.Mutex
	blobs     map[string][]byte
	manifests map[string][]byte
	byDigest  map[string][]byte
	tags      []string

	// RefuseDelete models GHCR and friends, which restrict manifest deletion.
	RefuseDelete bool

	deleted []string
}

// New returns an empty registry.
func New() *Registry {
	return &Registry{
		blobs:     map[string][]byte{},
		manifests: map[string][]byte{},
		byDigest:  map[string][]byte{},
	}
}

// Serve starts the registry and returns its test server.
func (r *Registry) Serve(t *testing.T) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(r.handle))
	t.Cleanup(ts.Close)
	return ts
}

func (r *Registry) handle(w http.ResponseWriter, req *http.Request) {
	path := req.URL.Path
	r.mu.Lock()
	defer r.mu.Unlock()

	switch {
	case path == "/v2/":
		w.WriteHeader(http.StatusOK)

	case strings.HasSuffix(path, "/tags/list"):
		_ = json.NewEncoder(w).Encode(map[string]any{"name": "repo", "tags": r.tags})

	case req.Method == http.MethodPost && strings.Contains(path, "/blobs/uploads/"):
		w.Header().Set("Location", "/v2/repo/blobs/uploads/s1")
		w.WriteHeader(http.StatusAccepted)

	case req.Method == http.MethodPut && strings.Contains(path, "/blobs/uploads/"):
		data, _ := io.ReadAll(req.Body)
		r.blobs[req.URL.Query().Get("digest")] = data
		w.WriteHeader(http.StatusCreated)

	case req.Method == http.MethodHead && strings.Contains(path, "/blobs/"):
		if _, ok := r.blobs[after(path, "/blobs/")]; ok {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)

	case req.Method == http.MethodGet && strings.Contains(path, "/blobs/"):
		if data, ok := r.blobs[after(path, "/blobs/")]; ok {
			_, _ = w.Write(data)
			return
		}
		w.WriteHeader(http.StatusNotFound)

	case req.Method == http.MethodPut && strings.Contains(path, "/manifests/"):
		ref := after(path, "/manifests/")
		data, _ := io.ReadAll(req.Body)
		r.manifests[ref] = data
		r.byDigest[Digest(data)] = data
		r.addTag(ref)
		w.Header().Set("Docker-Content-Digest", Digest(data))
		w.WriteHeader(http.StatusCreated)

	case req.Method == http.MethodDelete && strings.Contains(path, "/manifests/"):
		if r.RefuseDelete {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		r.deleteManifest(after(path, "/manifests/"))
		w.WriteHeader(http.StatusAccepted)

	default: // GET or HEAD of a manifest, by tag or by digest
		ref := after(path, "/manifests/")
		data, ok := r.manifests[ref]
		if !ok {
			data, ok = r.byDigest[ref]
		}
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var probe struct {
			MediaType string `json:"mediaType"`
		}
		_ = json.Unmarshal(data, &probe)
		if probe.MediaType == "" {
			probe.MediaType = ocispec.MediaTypeImageManifest
		}
		w.Header().Set("Content-Type", probe.MediaType)
		w.Header().Set("Docker-Content-Digest", Digest(data))
		if req.Method == http.MethodHead {
			w.Header().Set("Content-Length", strconv.Itoa(len(data)))
			w.WriteHeader(http.StatusOK)
			return
		}
		_, _ = w.Write(data)
	}
}

// deleteManifest removes a manifest and untags everything pointing at it.
//
// A client resolves a tag to a digest and deletes by digest, so a mock that
// only matches tag names never removes anything and every deletion assertion
// passes without a deletion having happened.
func (r *Registry) deleteManifest(ref string) {
	delete(r.byDigest, ref)

	kept := make([]string, 0, len(r.tags))
	for _, tag := range r.tags {
		data, ok := r.manifests[tag]
		if tag == ref || (ok && Digest(data) == ref) {
			delete(r.manifests, tag)
			continue
		}
		kept = append(kept, tag)
	}
	r.tags = kept
	r.deleted = append(r.deleted, ref)
}

func (r *Registry) addTag(tag string) {
	for _, t := range r.tags {
		if t == tag {
			return
		}
	}
	r.tags = append(r.tags, tag)
}

// Tags returns the current tag list.
func (r *Registry) Tags() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.tags...)
}

// Deletions returns the digests and tags deletion was requested for.
func (r *Registry) Deletions() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.deleted...)
}

// Blobs returns a copy of the stored blob contents.
func (r *Registry) Blobs() [][]byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([][]byte, 0, len(r.blobs))
	for _, b := range r.blobs {
		out = append(out, append([]byte(nil), b...))
	}
	return out
}

// DropManifest removes a manifest by tag without untagging anything else,
// which is how a repository becomes incomplete in the way fsck exists to find.
func (r *Registry) DropManifest(tag string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if data, ok := r.manifests[tag]; ok {
		delete(r.byDigest, Digest(data))
	}
	delete(r.manifests, tag)
	kept := r.tags[:0]
	for _, t := range r.tags {
		if t != tag {
			kept = append(kept, t)
		}
	}
	r.tags = append([]string(nil), kept...)
}

// Client returns an oci.Client pointed at the server.
func Client(t *testing.T, ts *httptest.Server) *oci.Client {
	t.Helper()
	u, err := url.Parse(ts.URL)
	if err != nil {
		t.Fatalf("parse server URL: %v", err)
	}
	c, err := oci.NewClient(u.Host+"/repo", true)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return c
}

// URL renders the server as the oci:// URL a user would type.
func URL(ts *httptest.Server) string {
	return "oci://" + strings.TrimPrefix(ts.URL, "http://") + "/repo"
}

// SeedRepository creates a local repository with n commits and pushes each one
// separately, which is what leaves a packfile and a commit tag per push.
//
// It sets GIT_DIR for the duration of the test and returns the tip.
func SeedRepository(t *testing.T, client *oci.Client, n int) (*git.Repository, string) {
	t.Helper()

	dir := t.TempDir()
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
			"GIT_AUTHOR_NAME=T", "GIT_AUTHOR_EMAIL=t@example.com",
			"GIT_COMMITTER_NAME=T", "GIT_COMMITTER_EMAIL=t@example.com")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run("init", "-q", "-b", "main", ".")

	t.Setenv("GIT_DIR", filepath.Join(dir, ".git"))
	repo, err := git.OpenRepository()
	if err != nil {
		t.Fatalf("OpenRepository: %v", err)
	}

	ctx := context.Background()
	var tip string
	var prev []string
	for i := range n {
		name := "f" + strconv.Itoa(i) + ".txt"
		if err := os.WriteFile(filepath.Join(dir, name), []byte(name+"\n"), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
		run("add", name)
		run("commit", "-q", "-m", "commit "+strconv.Itoa(i))
		tip = run("rev-parse", "HEAD")

		var pack bytes.Buffer
		if err := repo.CreatePackfileTo(&pack, plumbing.NewHash(tip), hashes(prev)); err != nil {
			t.Fatalf("CreatePackfileTo: %v", err)
		}
		if err := client.PushCommitStream(ctx, oci.CommitPush{
			CommitSHA: tip,
			RefName:   "refs/heads/main",
			RefTag:    oci.EncodeRefTag("refs/heads/main"),
			PackBases: prev,
		}, bytes.NewReader(pack.Bytes()), int64(pack.Len())); err != nil {
			t.Fatalf("push %d: %v", i, err)
		}
		prev = []string{tip}
	}

	if err := client.PushRichRefIndex(ctx, map[string]oci.RefEntry{
		"refs/heads/main": {SHA: tip},
	}, nil); err != nil {
		t.Fatalf("push ref index: %v", err)
	}
	return repo, tip
}

// Digest is the registry's content address for a manifest.
func Digest(data []byte) string {
	return opencontainers.FromBytes(data).String()
}

func hashes(shas []string) []plumbing.Hash {
	out := make([]plumbing.Hash, 0, len(shas))
	for _, s := range shas {
		out = append(out, plumbing.NewHash(s))
	}
	return out
}

func after(path, sep string) string {
	i := strings.LastIndex(path, sep)
	if i < 0 {
		return ""
	}
	return path[i+len(sep):]
}

// SetPackBases rewrites a tag's pack-bases annotation.
//
// Registry content is untrusted, so this is a registry describing something a
// correct writer never would — a missing base, or a cycle — which is precisely
// what a reader has to survive.
func (r *Registry) SetPackBases(t *testing.T, tag string, bases ...string) {
	t.Helper()

	r.mu.Lock()
	defer r.mu.Unlock()

	raw, ok := r.manifests[tag]
	if !ok {
		t.Fatalf("registry has no manifest tagged %q", tag)
	}
	var m ocispec.Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal manifest %q: %v", tag, err)
	}
	if m.Annotations == nil {
		m.Annotations = map[string]string{}
	}
	m.Annotations[oci.AnnotationGitPackBases] = strings.Join(bases, ",")

	out, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal manifest %q: %v", tag, err)
	}
	r.manifests[tag] = out
	// Keep it addressable by its new digest: a reader resolves the tag first
	// and then fetches by digest.
	r.byDigest[Digest(out)] = out
}
