package oci

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mrueg/git-remote-oci/pkg/config"
	"github.com/mrueg/git-remote-oci/pkg/lfs"
	opencontainers "github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/errdef"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/credentials"
	"oras.land/oras-go/v2/registry/remote/errcode"
)

const (
	MediaTypeGitConfig = ocispec.MediaTypeImageConfig
	MediaTypeGitIndex  = "application/vnd.git.repository.index.v1+json"
	TagRefIndex        = "_refs"
	TagOCIIndex        = "_index"

	AnnotationGitRef      = "io.git-remote-oci.ref"
	AnnotationGitParents  = "io.git-remote-oci.parents"
	AnnotationGitPushCert = "org.opencontainers.image.signature"
	AnnotationGitType     = "io.git-remote-oci.type"

	// AnnotationGitPackBases names the commits this manifest's packfile was cut
	// against, as a comma-separated list of hex commit ids, or the
	// literal PackBasesNone when the packfile is self-contained.
	//
	// It is the packfile dependency graph, which is not the same thing as the
	// commit graph in AnnotationGitParents: a push carrying several commits
	// publishes a manifest only for its tip, so the tip's parent usually has no
	// manifest at all while the base it was packed against does. Fetch has to
	// follow this annotation, not the parents, or it stops at the first parent
	// that was never a tip and silently leaves the object graph incomplete.
	//
	// It is mandatory. A manifest without it is rejected rather than guessed
	// at, which is why the self-contained case is spelled out as PackBasesNone
	// instead of being left empty.
	AnnotationGitPackBases = "io.git-remote-oci.pack-bases"

	AnnotationGitTagger     = "io.git-remote-oci.tagger"
	AnnotationGitTagMessage = "io.git-remote-oci.tag-message"
	AnnotationGitTagSig     = "io.git-remote-oci.tag-signature"
	AnnotationGitTagObj     = "io.git-remote-oci.tag-object"

	// AnnotationFormatVersion records the on-registry format version on the
	// _refs index manifest. Readers refuse anything they do not recognise.
	AnnotationFormatVersion = "io.git-remote-oci.format-version"

	// AnnotationGitDeleted marks a ref tag as a tombstone: the ref was deleted,
	// but the registry would not let the manifest itself be removed.
	//
	// Without it, a tag that survives deletion is rediscovered by tag
	// enumeration and the ref reappears on the next push.
	AnnotationGitDeleted = "io.git-remote-oci.deleted"

	// AnnotationGitHead records which ref the remote's HEAD points at, on the
	// _refs index and the _index image index.
	//
	// Without it a reader has to guess, and the guess was wrong for any
	// repository whose default branch is neither main nor master.
	AnnotationGitHead = "io.git-remote-oci.head"

	// PackBasesNone is the AnnotationGitPackBases value for a packfile that
	// depends on nothing else.
	PackBasesNone = "none"
)

// FormatVersion is the on-registry format this build reads and writes. It is
// the only version there is.
//
// It stays at 1 for the whole 0.x series. The format is explicitly unstable and
// carries no compatibility path, so bumping on every change would mint versions
// nobody reads and gain nothing. The number is a tripwire against a layout this
// build does not understand, not a changelog.
//
// FORMAT.md is the changelog, and still has to be updated in the same commit as
// any layout change. The version starts moving at 1.0.
const FormatVersion = "1"

// ErrUnsupportedFormat reports a repository written in a format this build does
// not implement.
//
// Refusing is the point. Silently reading a layout whose meaning has changed is
// how a fetch ends up quietly missing objects, so an unrecognised version is a
// hard stop with an explanation rather than a best effort.
var ErrUnsupportedFormat = errors.New("unsupported on-registry format version")

var (
	ErrNotAnImageManifest = errors.New("reference is not an OCI image manifest")
	ErrManifestNotFound   = errors.New("manifest reference not found")
)

const (
	// refsIndexLockRef is the pseudo-ref whose lock serialises updates to the
	// _refs index, which every push read-modify-writes.
	refsIndexLockRef  = "_refs_index_lock"
	refsIndexLockWait = 45 * time.Second
	// refsIndexMaxAttempts bounds the optimistic-concurrency retry in
	// PushRichRefIndex. Contention is expected to be rare; a client that loses
	// three times running is better off reporting it than spinning.
	refsIndexMaxAttempts = 3
)

// indexAnnotations builds the annotation set for a ref index manifest.
func indexAnnotations(head string) map[string]string {
	a := map[string]string{
		// Declares the layout of this repository. A reader that does not
		// implement this version refuses the repository; see
		// checkFormatVersion.
		AnnotationFormatVersion: FormatVersion,
	}
	if head != "" {
		a[AnnotationGitHead] = head
	}
	return a
}

// currentHead reads the HEAD recorded on the published ref index.
//
// A repository with no index yet, or none recorded, reports "" with no error:
// that is the normal state of something nobody has pushed a branch to.
func (c *Client) currentHead(ctx context.Context) (string, error) {
	c.manifestCache.Delete(TagRefIndex)
	manifest, err := c.FetchManifest(ctx, TagRefIndex)
	if err != nil {
		if IsNotFound(err) {
			return "", nil
		}
		return "", err
	}
	return manifest.Annotations[AnnotationGitHead], nil
}

// FetchHead returns the ref the remote's HEAD points at, or "" if none is
// recorded.
func (c *Client) FetchHead(ctx context.Context) (string, error) {
	head, err := c.currentHead(ctx)
	if err == nil && head != "" {
		return head, nil
	}
	if err != nil && !IsNotFound(err) {
		return "", err
	}
	// _index stands in when _refs is missing. A repository with neither simply
	// records no HEAD, which is not an error; anything else is reported, so a
	// caller can tell "none recorded" from "could not find out".
	idx, idxErr := c.FetchOCIImageIndex(ctx, TagOCIIndex)
	if idxErr != nil {
		if IsNotFound(idxErr) {
			return "", nil
		}
		return "", idxErr
	}
	return idx.Annotations[AnnotationGitHead], nil
}

// checkFormatVersion rejects a repository this build cannot read.
//
// An absent annotation is treated as unrecognised rather than assumed current:
// every version this build writes sets it, so its absence means the repository
// was written by something else.
func checkFormatVersion(version string) error {
	if version == FormatVersion {
		return nil
	}
	found := version
	if found == "" {
		found = "none"
	}
	return fmt.Errorf(
		"%w: the repository declares format version %s, this build implements %s. "+
			"The on-registry format is not stable and no compatibility path is provided",
		ErrUnsupportedFormat, found, FormatVersion)
}

// IsNotFound reports whether err means "this does not exist on the registry",
// as opposed to "we could not find out".
//
// The distinction matters: a missing repository or index is the normal state of
// a registry that has never been pushed to, whereas an authentication failure,
// a 5xx, or a network error leaves the remote's contents unknown. Treating the
// second case as "empty" makes an unreachable remote look like a fresh one.
func IsNotFound(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrManifestNotFound) || errors.Is(err, errdef.ErrNotFound) {
		return true
	}
	var errResp *errcode.ErrorResponse
	if errors.As(err, &errResp) && errResp.StatusCode == http.StatusNotFound {
		return true
	}
	var codeErr errcode.Error
	if errors.As(err, &codeErr) {
		switch codeErr.Code {
		case errcode.ErrorCodeNameUnknown, errcode.ErrorCodeManifestUnknown:
			return true
		}
	}
	return false
}

// authOrigin identifies which credential answered the registry's challenge, so
// an authentication failure can say which one to go and fix.
//
// It is an enum with a String method rather than string constants because
// constants named after credentials trip gosec's hardcoded-credential check,
// and silencing that check here would silence it for the whole file.
type authOrigin int

const (
	originAnonymous authOrigin = iota
	originEnvBearer
	originEnvToken
	originEnvUserPass
	originDockerStore
)

func (o authOrigin) String() string {
	switch o {
	case originEnvBearer:
		return "the OCI_BEARER_TOKEN environment variable"
	case originEnvToken:
		return "the OCI_TOKEN environment variable"
	case originEnvUserPass:
		return "the OCI_USERNAME/OCI_PASSWORD environment variables"
	case originDockerStore:
		return "the Docker credential store (~/.docker/config.json or a credential helper)"
	default:
		return "anonymous access"
	}
}

// Default TTLs for the index locks. Both are Client fields rather than
// constants because the right value depends on the link and the repository,
// not on the code.
const (
	// DefaultRefsIndexLockTTL has to cover the whole critical section:
	// fetching the current index, listing refs, and pushing the index blob,
	// the config blob, the index manifest and _index. At 15 seconds that
	// routinely expired mid-update on a slow link or a repository with many
	// refs, after which another client legitimately took the lock and the two
	// interleaved their read-modify-write of _refs — the exact loss the lock
	// exists to prevent.
	//
	// Erring long costs a stalled ref until the TTL runs out; erring short
	// costs correctness. Hence five minutes, and hence configurable.
	DefaultRefsIndexLockTTL = 5 * time.Minute

	// DefaultLFSLocksIndexTTL covers a read-modify-write of one blob, which is
	// a much shorter critical section than the ref index.
	DefaultLFSLocksIndexTTL = 15 * time.Second
)

// ApplyConfig copies the registry-side tunables from a resolved git config.
//
// The client is *told* its settings rather than going looking for them: it has
// no business discovering a git repository, and the subcommands can be run
// outside one. Callers that have a remote name should resolve the config with
// it, so `remote.<name>.oci*` takes effect.
func (c *Client) ApplyConfig(cfg *config.Config) {
	// The environment keeps precedence. OCI_COMPRESSION predates this and a
	// one-off override should not require editing a config file.
	if c.Compression == "" {
		c.Compression = cfg.String(config.KeyCompression, "")
	}
	c.RefsIndexLockTTL = cfg.Duration(config.KeyIndexLockTTL, DefaultRefsIndexLockTTL)
	c.LFSLocksIndexTTL = cfg.Duration(config.KeyLFSIndexLockTTL, DefaultLFSLocksIndexTTL)
}

type Client struct {
	Repo *remote.Repository

	// Compression is the layer compression algorithm: "none", "gzip" or
	// "zstd". NewClient seeds it from OCI_COMPRESSION; a caller that reads
	// git config may overwrite it before the client is used.
	Compression string

	// RefsIndexLockTTL and LFSLocksIndexTTL bound how long this client may
	// hold the lock on the _refs and _lfs_locks indexes. They were constants;
	// see the comments on their defaults for why the right value depends on
	// the link and the repository rather than on the code.
	RefsIndexLockTTL time.Duration
	LFSLocksIndexTTL time.Duration

	manifestCache    boundedMap
	pushedBlobsCache boundedMap
	// refTagDigests maps a ref tag to the digest this client last published
	// under it, so a re-push of the same content is skipped without skipping a
	// re-push of *different* content.
	refTagDigests boundedMap
	// authFrom records which credential source the client was built with.
	authFrom atomic.Value
	// authWorked records that the registry accepted a request at least once.
	//
	// A 401 on the first request means the credentials are wrong or missing. A
	// 401 after the registry has already served this client means something
	// expired mid-operation, which is a different problem with a different
	// answer, and reporting both as "unauthorized" sends people to look in the
	// wrong place.
	authWorked atomic.Bool
	// heldLocks maps ref name -> lock id for locks this client acquired, so
	// ReleaseRefLock can refuse to release someone else's lock.
	heldLocks sync.Map
}

func (c *Client) pushBlobOnce(ctx context.Context, desc ocispec.Descriptor, content []byte) error {
	digestStr := desc.Digest.String()
	if _, cached := c.pushedBlobsCache.Load(digestStr); cached {
		return nil
	}
	if err := c.Repo.Push(ctx, desc, bytes.NewReader(content)); err != nil && !errors.Is(err, errdef.ErrAlreadyExists) {
		return err
	}
	c.pushedBlobsCache.Store(digestStr, true)
	return nil
}

// NewClientForURL builds a client for a URL as a user typed it, applying the
// scheme and plain-HTTP rules that every entry point has to agree on.
//
// The remote helper and the subcommands both receive an oci:// URL on the
// command line and both have to strip the scheme and decide whether to speak
// plain HTTP. They used to do it in two places with two copies of the rule,
// which can only diverge silently: a subcommand reaching a local registry over
// HTTPS while the helper reaches it over HTTP, or the reverse.
//
// getenv reads the environment; nil means os.Getenv.
func NewClientForURL(rawURL string, getenv func(string) string) (*Client, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	repoRef := strings.TrimPrefix(rawURL, "oci://")

	insecure := getenv("OCI_INSECURE")
	plainHTTP := insecure == "1" || insecure == "true" ||
		strings.HasPrefix(repoRef, "localhost:") ||
		strings.HasPrefix(repoRef, "127.0.0.1:")

	return NewClient(repoRef, plainHTTP)
}

// NewClient initialises a new OCI registry client for the given repository reference (e.g. "registry.example.com/repo").
func NewClient(repoRef string, plainHTTP bool) (*Client, error) {
	repo, err := remote.NewRepository(repoRef)
	if err != nil {
		return nil, fmt.Errorf("failed to parse OCI repository reference %q: %w", repoRef, err)
	}
	repo.PlainHTTP = plainHTTP

	// Configure transport-level timeouts to prevent hanging during TCP dial, TLS handshake, or header reads
	tr := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   32,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ForceAttemptHTTP2:     true,
	}

	retryTr := newRetryTransport(tr)

	authClient := &auth.Client{
		Client: &http.Client{
			Transport: retryTr,
		},
		Cache: auth.NewCache(),
	}

	var dockerStore credentials.Store
	if store, err := credentials.NewStoreFromDocker(credentials.StoreOptions{}); err == nil {
		dockerStore = store
	}

	client := &Client{
		Repo:             repo,
		manifestCache:    boundedMap{max: maxCachedManifests},
		pushedBlobsCache: boundedMap{max: maxCachedPushedBlobs},
		refTagDigests:    boundedMap{max: maxCachedManifests},
		Compression:      os.Getenv("OCI_COMPRESSION"),
		RefsIndexLockTTL: DefaultRefsIndexLockTTL,
		LFSLocksIndexTTL: DefaultLFSLocksIndexTTL,
	}
	// The transport reports back so the client can tell a credential that never
	// worked from one that stopped working part-way through.
	retryTr.observe = client.noteResponse

	client.authFrom.Store(originAnonymous)

	authClient.Credential = func(ctx context.Context, serverAddress string) (auth.Credential, error) {
		// 1. Environment variable overrides
		if token := os.Getenv("OCI_BEARER_TOKEN"); token != "" {
			client.authFrom.Store(originEnvBearer)
			return auth.Credential{AccessToken: token}, nil
		}
		if token := os.Getenv("OCI_TOKEN"); token != "" {
			client.authFrom.Store(originEnvToken)
			return auth.Credential{AccessToken: token}, nil
		}
		user := os.Getenv("OCI_USERNAME")
		pass := os.Getenv("OCI_PASSWORD")
		if user != "" || pass != "" {
			client.authFrom.Store(originEnvUserPass)
			return auth.Credential{Username: user, Password: pass}, nil
		}

		// 2. Docker credential store (~/.docker/config.json and native credential helpers)
		if dockerStore != nil {
			cred, err := credentials.Credential(dockerStore)(ctx, serverAddress)
			if err == nil && (cred.Username != "" || cred.Password != "" || cred.AccessToken != "" || cred.RefreshToken != "") {
				client.authFrom.Store(originDockerStore)
				return cred, nil
			}
		}

		// 3. Fallback to empty credential for public access
		client.authFrom.Store(originAnonymous)
		return auth.EmptyCredential, nil
	}

	repo.Client = authClient
	return client, nil
}

// IsAuthError reports whether err is the registry rejecting our credentials.
func IsAuthError(err error) bool {
	if err == nil {
		return false
	}
	var errResp *errcode.ErrorResponse
	if errors.As(err, &errResp) {
		switch errResp.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden:
			return true
		}
	}
	var codeErr errcode.Error
	if errors.As(err, &codeErr) {
		switch codeErr.Code {
		case errcode.ErrorCodeUnauthorized, errcode.ErrorCodeDenied:
			return true
		}
	}
	// A Basic challenge with nothing to answer it is rejected by the auth
	// client before a request is ever sent, so it carries no HTTP status.
	return strings.Contains(err.Error(), "credential not found")
}

// explainAuth annotates an authentication failure with the credential that was
// actually used.
//
// A bare "401 Unauthorized" gives no clue whether the request went out
// anonymously, with a stale token from the environment, or with something the
// Docker credential store produced - which is exactly what the user needs to
// know, and is invisible from the outside because the resolution order is
// internal.
func (c *Client) explainAuth(err error) error {
	if !IsAuthError(err) {
		return err
	}
	source, _ := c.authFrom.Load().(authOrigin)

	// Rejected after the registry had already been serving this client: the
	// credential was good and has stopped being good. Saying "they may be
	// wrong" here would be actively misleading — they demonstrably were not.
	if c.authWorked.Load() {
		if source == originEnvBearer || source == originEnvToken {
			return fmt.Errorf("%w (the token from %s was accepted earlier in this operation and is now being rejected, so it has most likely expired; "+
				"a static token cannot be renewed automatically, so reissue it and run the command again)", err, source)
		}
		return fmt.Errorf("%w (the credentials from %s were accepted earlier in this operation and are now being rejected; "+
			"the registry session has most likely expired, and re-authenticating — `docker login`, or fresh OCI_USERNAME/OCI_PASSWORD — should clear it)", err, source)
	}

	if source == originAnonymous {
		return fmt.Errorf("%w (the request was made anonymously; set OCI_USERNAME and OCI_PASSWORD, or OCI_BEARER_TOKEN, or run `docker login` for this registry)", err)
	}
	return fmt.Errorf("%w (the credentials came from %s and were never accepted by this registry; they may be wrong or lack access to this repository)", err, source)
}

// noteResponse records whether the registry is accepting this client.
func (c *Client) noteResponse(statusCode int) {
	switch statusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		// Not evidence either way: the token exchange itself answers 401 as
		// part of the challenge, before any credential has been presented.
	default:
		if statusCode < 500 {
			c.authWorked.Store(true)
		}
	}
}

// emptyLayers returns the layer list for a manifest that carries its payload in
// its config or its annotations rather than in a layer.
//
// The field cannot simply be omitted: ocispec.Manifest tags Layers as `layers`
// with no omitempty, so leaving it nil serialises as `"layers": null`, and the
// image-spec requires an array. Registries that validate manifests reject that,
// which broke ref locking and LFS locking on them while ordinary push and fetch
// kept working - a confusing partial failure.
//
// The single empty-JSON descriptor is the idiom the spec defines for exactly
// this case, and is better supported than an empty array.
func emptyLayers() []ocispec.Descriptor {
	return []ocispec.Descriptor{ocispec.DescriptorEmptyJSON}
}

// pushEmptyBlob uploads the `{}` blob that emptyLayers refers to.
//
// A manifest whose layer is not in the registry is rejected by anything that
// validates, so this has to run before the manifest that names it.
func (c *Client) pushEmptyBlob(ctx context.Context) error {
	if err := c.pushBlobOnce(ctx, ocispec.DescriptorEmptyJSON, ocispec.DescriptorEmptyJSON.Data); err != nil {
		return fmt.Errorf("failed to push the empty layer blob: %w", err)
	}
	return nil
}

// objectIDLenSHA1 and objectIDLenSHA256 are the hex lengths of the two hash
// algorithms git uses.
const (
	objectIDLenSHA1   = 40
	objectIDLenSHA256 = 64
)

// isObjectID reports whether s is a git object id in hex, under either hash
// algorithm.
//
// Accepting both is what makes SHA-256 repositories work: object ids appear as
// tag names, as pack-base entries and as revision annotations, and every one of
// those checks used to insist on exactly 40 characters.
func isObjectID(s string) bool {
	if len(s) != objectIDLenSHA1 && len(s) != objectIDLenSHA256 {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') && (r < 'A' || r > 'F') {
			return false
		}
	}
	return true
}

// IsCommitID reports whether s is a commit id this implementation can address.
//
// Exported so callers can validate ids that arrive in registry annotations
// before using them as tag names or filesystem components.
func IsCommitID(s string) bool { return isObjectID(s) }

// FormatPackBases renders bases for AnnotationGitPackBases.
//
// An empty list is PackBasesNone rather than the empty string, so that a
// self-contained packfile is a positive statement instead of an absent one.
func FormatPackBases(bases []string) string {
	if len(bases) == 0 {
		return PackBasesNone
	}
	return strings.Join(bases, ",")
}

// ParsePackBases reads AnnotationGitPackBases.
//
// The annotation is mandatory. Absent, empty or malformed is an error rather
// than an empty list, because an empty list means "self-contained" and acting
// on that guess is exactly how a fetch ends up silently missing objects.
func ParsePackBases(annotations map[string]string) ([]string, error) {
	raw, ok := annotations[AnnotationGitPackBases]
	if !ok {
		return nil, fmt.Errorf("manifest has no %s annotation", AnnotationGitPackBases)
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, fmt.Errorf("manifest has an empty %s annotation; a self-contained packfile must say %q", AnnotationGitPackBases, PackBasesNone)
	}
	if raw == PackBasesNone {
		return nil, nil
	}
	var bases []string
	for _, field := range strings.Split(raw, ",") {
		base := strings.TrimSpace(field)
		if base == "" {
			continue
		}
		// These become tag names on the next request, so they are validated
		// here rather than at the point of use.
		if !isObjectID(base) {
			return nil, fmt.Errorf("malformed %s entry %q: not a hex commit id of 40 or 64 characters", AnnotationGitPackBases, base)
		}
		bases = append(bases, base)
	}
	return bases, nil
}

// CommitManifestExists reports whether the registry serves a manifest for
// commitSHA.
//
// The error is returned rather than folded into a false so callers can tell
// "the registry does not have it" from "we could not find out". A push that
// cannot find out must not go on to cut a packfile against that commit.
func (c *Client) CommitManifestExists(ctx context.Context, commitSHA string) (bool, error) {
	if !isObjectID(commitSHA) {
		return false, fmt.Errorf("invalid commit SHA %q: must be a hex object id of 40 (SHA-1) or 64 (SHA-256) characters", commitSHA)
	}
	if _, cached := c.manifestCache.Load(commitSHA); cached {
		return true, nil
	}
	if _, err := c.Repo.Resolve(ctx, commitSHA); err != nil {
		if IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("failed to check for commit manifest %s: %w", commitSHA, err)
	}
	return true, nil
}

// ListRefs lists all git references published in the OCI registry.
// Uses fast O(1) _refs index lookup if present, with fallback to tag enumeration.
func (c *Client) ListRefs(ctx context.Context) (map[string]string, error) {
	// 1. Try fast O(1) lookup using repository _refs index
	if indexRefs, err := c.FetchRefIndex(ctx); err == nil {
		return indexRefs, nil
	}

	// 2. Fall back to enumerating tags. This is a repair path for a repository
	//    whose index is missing or damaged: it costs a round trip per tag and
	//    cannot see refs whose tags were truncated, so it is never the fast path.
	refs := make(map[string]string) // refName -> commitSHA

	err := c.Repo.Tags(ctx, "", func(tags []string) error {
		for _, tag := range tags {
			if tag == TagRefIndex {
				continue
			}
			// Skip commit-id tags up front: they are ref-agnostic
			// commit manifests containing no Git ref annotations. Skipping them avoids
			// unnecessary manifest fetch round-trips for every historical commit.
			if isObjectID(tag) {
				continue
			}

			manifest, err := c.FetchManifest(ctx, tag)
			if err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				// Skip non-image manifest tags cleanly using typed error assertions
				if errors.Is(err, ErrNotAnImageManifest) || errors.Is(err, ErrManifestNotFound) {
					continue
				}
				return fmt.Errorf("failed to fetch manifest for tag %s: %w", tag, err)
			}

			if !hasGitPackfileLayer(manifest) {
				// Skip manifests that do not contain a Git packfile layer (e.g. standard container images).
				continue
			}

			commitSHA := manifest.Annotations[ocispec.AnnotationRevision]
			if commitSHA == "" || !isObjectID(commitSHA) {
				// Skip manifests without a valid hex revision annotation.
				// This prevents returning non-SHA values to callers expecting Git object IDs.
				continue
			}

			gitRef := manifest.Annotations[AnnotationGitRef]
			if gitRef != "" {
				refs[gitRef] = commitSHA
			}
		}
		return nil
	})

	if err != nil {
		if errors.Is(err, errdef.ErrNotFound) {
			return refs, nil
		}
		var errResp *errcode.ErrorResponse
		if errors.As(err, &errResp) && errResp.StatusCode == http.StatusNotFound {
			return refs, nil
		}
		return nil, c.explainAuth(fmt.Errorf("failed to list tags from registry: %w", err))
	}

	return refs, nil
}

// EnumerateTagRefs directly enumerates all Git ref tags from registry tag listings.
func (c *Client) EnumerateTagRefs(ctx context.Context) (map[string]string, error) {
	refs := make(map[string]string)
	err := c.Repo.Tags(ctx, "", func(tags []string) error {
		for _, tag := range tags {
			if tag == TagRefIndex || tag == TagOCIIndex || isObjectID(tag) {
				continue
			}
			c.manifestCache.Delete(tag)
			manifest, err := c.FetchManifest(ctx, tag)
			if err != nil {
				continue
			}
			// A tombstone means the ref was deleted on a registry that would
			// not remove the manifest. Enumerating it would resurrect the ref.
			if manifest.Annotations[AnnotationGitDeleted] == "true" {
				continue
			}
			if !hasGitPackfileLayer(manifest) {
				continue
			}
			commitSHA := manifest.Annotations[ocispec.AnnotationRevision]
			if commitSHA == "" || !isObjectID(commitSHA) {
				continue
			}
			gitRef := manifest.Annotations[AnnotationGitRef]
			if gitRef != "" {
				refs[gitRef] = commitSHA
			}
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, errdef.ErrNotFound) {
			return refs, nil
		}
		var errResp *errcode.ErrorResponse
		if errors.As(err, &errResp) && errResp.StatusCode == http.StatusNotFound {
			return refs, nil
		}
		return nil, c.explainAuth(fmt.Errorf("failed to enumerate tags from registry: %w", err))
	}
	return refs, nil
}

// IsCommitManifestCached returns true if the commit manifest for the given SHA is already cached.
func (c *Client) IsCommitManifestCached(commitSHA string) bool {
	if cached, ok := c.manifestCache.Load(commitSHA); ok && cached != nil {
		return true
	}
	return false
}

// RefManifestTag returns the tag under which the ref manifest for refName is
// published, or "" if refName cannot be represented as an OCI tag.
//
// There is exactly one tag per ref, for both reading and writing.
//
// It is not simply EncodeRefTag: a ref whose encoded name happens to look like
// a commit id is prefixed so it cannot be mistaken for one of the ref-agnostic
// commit manifests. Anything reasoning about what a tag means - garbage
// collection, for one - must use this rather than EncodeRefTag.
func RefManifestTag(refName string) string {
	tag := EncodeRefTag(refName)
	if tag == "" {
		return ""
	}
	// A branch whose encoded name happens to look like a commit id would
	// collide with the ref-agnostic commit-SHA manifests, so give it a prefix.
	if isObjectID(tag) {
		return "ref-" + tag
	}
	return tag
}

// ResolveRefManifest resolves the manifest descriptor for refName.
func (c *Client) ResolveRefManifest(ctx context.Context, refName string) (ocispec.Descriptor, error) {
	tag := RefManifestTag(refName)
	if tag == "" {
		return ocispec.Descriptor{}, fmt.Errorf("ref %q has no representable tag", refName)
	}
	return c.Repo.Resolve(ctx, tag)
}

// IsRefFullyPushed reports whether both the ref-agnostic commit manifest for
// commitSHA and the ref manifest for refName have already been pushed by this
// process. Callers use it to skip redundant work on multi-ref pushes.
//
// Both halves must be present: a cached commit manifest alone does not mean the
// ref tag exists, and skipping on that basis leaves the ref discoverable only
// through the _refs index.
func (c *Client) IsRefFullyPushed(commitSHA, refName string) bool {
	if !c.IsCommitManifestCached(commitSHA) {
		return false
	}
	if refName == "" {
		return true
	}
	targetTag := RefManifestTag(refName)
	if targetTag == "" {
		return false
	}
	_, ok := c.manifestCache.Load(targetTag)
	return ok
}

// RefEntry represents cached reference metadata inside the _refs JSON index payload.
type RefEntry struct {
	SHA        string `json:"sha"`
	Author     string `json:"author,omitempty"`
	Timestamp  int64  `json:"timestamp,omitempty"`
	Message    string `json:"message,omitempty"`
	TagSig     string `json:"tag_sig,omitempty"`
	Tagger     string `json:"tagger,omitempty"`
	TagMessage string `json:"tag_message,omitempty"`
	TagObject  string `json:"tag_object,omitempty"`
}

// FetchRichRefIndex retrieves the rich ref mapping from the _refs index tag,
// with automatic fallback to the _index OCI Image Index manifest.
func (c *Client) FetchRichRefIndex(ctx context.Context) (map[string]RefEntry, error) {
	c.manifestCache.Delete(TagRefIndex)
	manifest, err := c.FetchManifest(ctx, TagRefIndex)
	if err != nil {
		// _index carries the same ref set and is written alongside _refs, so it
		// stands in when _refs is missing. It is not a legacy path: both are
		// current, and FetchOCIImageIndexRefs applies the same version check.
		if ociRefs, ociErr := c.FetchOCIImageIndexRefs(ctx, TagOCIIndex); ociErr == nil && len(ociRefs) > 0 {
			return ociRefs, nil
		}
		return nil, fmt.Errorf("failed to fetch _refs index manifest: %w", err)
	}

	// The _refs index is the first thing every operation reads, which makes it
	// the one place worth checking the format version. Doing it here means an
	// unreadable repository is refused before anything acts on its contents.
	if err := checkFormatVersion(manifest.Annotations[AnnotationFormatVersion]); err != nil {
		return nil, err
	}

	var indexDesc *ocispec.Descriptor
	for i := range manifest.Layers {
		if manifest.Layers[i].MediaType == MediaTypeGitIndex {
			indexDesc = &manifest.Layers[i]
			break
		}
	}
	if indexDesc == nil {
		return nil, fmt.Errorf("no %s layer in the _refs manifest", MediaTypeGitIndex)
	}

	rc, err := c.Repo.Fetch(ctx, *indexDesc)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch _refs index blob: %w", err)
	}
	defer func() { _ = rc.Close() }()

	data, err := io.ReadAll(rc)
	if err != nil {
		return nil, fmt.Errorf("failed to read _refs index content: %w", err)
	}

	var richRefs map[string]RefEntry
	if err := json.Unmarshal(data, &richRefs); err != nil {
		return nil, fmt.Errorf("failed to unmarshal _refs index: %w", err)
	}

	return richRefs, nil
}

// FetchRefIndex retrieves the ref mapping (ref -> SHA) from the _refs index tag.
func (c *Client) FetchRefIndex(ctx context.Context) (map[string]string, error) {
	richRefs, err := c.FetchRichRefIndex(ctx)
	if err != nil {
		return nil, err
	}

	refs := make(map[string]string, len(richRefs))
	for refName, entry := range richRefs {
		refs[refName] = entry.SHA
	}
	return refs, nil
}

// PushOCIImageIndex publishes an OCI Image Index (application/vnd.oci.image.index.v1+json)
// grouping multiple commit branch heads and repository references under a single manifest index tag.
func (c *Client) PushOCIImageIndex(ctx context.Context, tag string, refs map[string]RefEntry, head string) error {
	if tag == "" {
		tag = TagOCIIndex
	}

	manifests := make([]ocispec.Descriptor, 0, len(refs))
	for refName, entry := range refs {
		if entry.SHA == "" {
			continue
		}
		desc, err := c.ResolveRefManifest(ctx, refName)
		if err != nil {
			desc, _ = c.Repo.Resolve(ctx, entry.SHA)
		}
		if desc.Digest == "" {
			// Neither the ref manifest nor the commit manifest could be
			// resolved. Emitting a zero-valued descriptor would produce an
			// index entry with an empty digest, which no client can follow.
			continue
		}

		// Carry the resolved media type rather than assuming an OCI manifest:
		// a registry that stored the ref as a Docker manifest would otherwise be
		// misdescribed here, and clients follow the index's word for it.
		childMediaType := desc.MediaType
		if childMediaType == "" {
			childMediaType = ocispec.MediaTypeImageManifest
		}

		manifestDesc := ocispec.Descriptor{
			MediaType: childMediaType,
			Digest:    desc.Digest,
			Size:      desc.Size,
			// Without a platform, an index entry cannot be selected by
			// `docker pull` or anything else that matches on one. Git data is
			// platform-agnostic, and unknown/unknown is the convention for
			// exactly that, matching the config blob written for each commit.
			Platform: &ocispec.Platform{
				Architecture: "unknown",
				OS:           "unknown",
			},
			Annotations: map[string]string{
				ocispec.AnnotationRefName:  EncodeRefTag(refName),
				AnnotationGitRef:           refName,
				ocispec.AnnotationRevision: entry.SHA,
			},
		}

		if entry.TagSig != "" {
			manifestDesc.Annotations[AnnotationGitPushCert] = entry.TagSig
			manifestDesc.Annotations[AnnotationGitTagSig] = entry.TagSig
		}
		if entry.Tagger != "" {
			manifestDesc.Annotations[AnnotationGitTagger] = entry.Tagger
		}
		if entry.TagMessage != "" {
			manifestDesc.Annotations[AnnotationGitTagMessage] = entry.TagMessage
		}
		if entry.TagObject != "" {
			manifestDesc.Annotations[AnnotationGitTagObj] = entry.TagObject
		}
		if entry.Author != "" {
			manifestDesc.Annotations[ocispec.AnnotationAuthors] = entry.Author
		}
		if entry.Timestamp > 0 {
			manifestDesc.Annotations[ocispec.AnnotationCreated] = time.Unix(entry.Timestamp, 0).UTC().Format(time.RFC3339)
		}
		if entry.Message != "" {
			msgLines := strings.Split(strings.TrimSpace(entry.Message), "\n")
			if len(msgLines) > 0 && msgLines[0] != "" {
				manifestDesc.Annotations[ocispec.AnnotationDescription] = msgLines[0]
			}
		}
		manifestDesc.Annotations[ocispec.AnnotationTitle] = refName
		manifestDesc.Annotations[ocispec.AnnotationVendor] = "git-remote-oci"

		manifests = append(manifests, manifestDesc)
	}

	sort.Slice(manifests, func(i, j int) bool {
		return manifests[i].Annotations[AnnotationGitRef] < manifests[j].Annotations[AnnotationGitRef]
	})

	indexObj := ocispec.Index{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageIndex,
		Manifests: manifests,
		Annotations: map[string]string{
			AnnotationGitType: "repository-index",
			// _index stands in for _refs when _refs is missing, so it has to
			// declare the layout too. Without this the fallback was a way into
			// a repository this build cannot read.
			AnnotationFormatVersion: FormatVersion,
			AnnotationGitHead:       head,
		},
	}

	indexBytes, err := json.Marshal(indexObj)
	if err != nil {
		return fmt.Errorf("failed to marshal OCI Image Index: %w", err)
	}

	indexDigest := opencontainers.FromBytes(indexBytes)
	indexDesc := ocispec.Descriptor{
		MediaType: ocispec.MediaTypeImageIndex,
		Digest:    indexDigest,
		Size:      int64(len(indexBytes)),
	}

	err = c.Repo.PushReference(ctx, indexDesc, bytes.NewReader(indexBytes), tag)
	if err == nil {
		c.manifestCache.Store(tag, &indexObj)
		c.manifestCache.Store(indexDigest.String(), &indexObj)
	}
	return err
}

// FetchOCIImageIndex fetches an OCI Image Index manifest from the registry by tag or digest.
func (c *Client) FetchOCIImageIndex(ctx context.Context, tagOrDigest string) (*ocispec.Index, error) {
	if tagOrDigest == "" {
		tagOrDigest = TagOCIIndex
	}

	if cached, ok := c.manifestCache.Load(tagOrDigest); ok {
		if idx, ok := cached.(*ocispec.Index); ok {
			return idx, nil
		}
	}

	_, rc, err := c.Repo.FetchReference(ctx, tagOrDigest)
	if err != nil {
		return nil, c.explainAuth(fmt.Errorf("failed to fetch reference %s: %w", tagOrDigest, err))
	}
	defer func() { _ = rc.Close() }()

	data, err := io.ReadAll(rc)
	if err != nil {
		return nil, fmt.Errorf("failed to read OCI Image Index content: %w", err)
	}

	var index ocispec.Index
	if err := json.Unmarshal(data, &index); err != nil {
		return nil, fmt.Errorf("failed to unmarshal OCI Image Index JSON: %w", err)
	}

	c.manifestCache.Store(tagOrDigest, &index)
	return &index, nil
}

// FetchOCIImageIndexRefs retrieves the ref mapping (ref -> RefEntry) from an OCI Image Index tag.
func (c *Client) FetchOCIImageIndexRefs(ctx context.Context, tagOrDigest string) (map[string]RefEntry, error) {
	index, err := c.FetchOCIImageIndex(ctx, tagOrDigest)
	if err != nil {
		return nil, err
	}

	// Same check as on _refs. This path exists because _index stands in when
	// _refs is missing, and an unchecked stand-in is just an unchecked way in.
	if err := checkFormatVersion(index.Annotations[AnnotationFormatVersion]); err != nil {
		return nil, err
	}

	refs := make(map[string]RefEntry, len(index.Manifests))
	for _, m := range index.Manifests {
		// Every entry this build writes carries the full ref name. There used
		// to be a fallback that guessed refs/tags/ from a leading "v" and
		// refs/heads/ otherwise, which was wrong for a branch called v2 or any
		// tag not starting with v.
		gitRef := m.Annotations[AnnotationGitRef]

		sha := m.Annotations[ocispec.AnnotationRevision]
		if gitRef != "" && sha != "" {
			entry := RefEntry{
				SHA:        sha,
				Tagger:     m.Annotations[AnnotationGitTagger],
				TagMessage: m.Annotations[AnnotationGitTagMessage],
				TagSig:     m.Annotations[AnnotationGitTagSig],
				TagObject:  m.Annotations[AnnotationGitTagObj],
			}
			if entry.TagSig == "" {
				entry.TagSig = m.Annotations[AnnotationGitPushCert]
			}
			refs[gitRef] = entry
		}
	}
	return refs, nil
}

// PushRichRefIndex publishes an updated rich ref mapping JSON to the _refs tag.
//
// The index is merged with whatever the remote currently holds, so that a
// client pushing one ref does not drop refs another client added concurrently.
// deleted names refs this caller has just removed; they are excluded from the
// merge. Without that, the tag-enumeration fallback below re-adds a ref whose
// index entry was removed but whose OCI tag deletion the registry refused,
// resurrecting it on the very next push.
// PushRichRefIndex publishes the ref index, preserving whatever HEAD is already
// recorded.
func (c *Client) PushRichRefIndex(ctx context.Context, refs map[string]RefEntry, deleted map[string]bool) error {
	return c.PushRichRefIndexWithHead(ctx, refs, deleted, "")
}

// PushRichRefIndexWithHead publishes the ref index and, when the repository has
// no HEAD recorded yet, adopts headHint.
//
// First writer wins: a later push does not move HEAD, because nothing in the
// remote-helper protocol tells the helper what the remote's default branch
// should be, and silently retargeting it on every push would be worse than
// leaving it alone.
func (c *Client) PushRichRefIndexWithHead(ctx context.Context, refs map[string]RefEntry, deleted map[string]bool, headHint string) error {
	// Read-modify-write under optimistic concurrency control.
	//
	// The digest check is what protects the data, not the lock. A registry
	// offers no compare-and-swap, so lock acquisition is itself check-then-write
	// and two clients can both believe they hold it. Comparing the index digest
	// from before the merge against the digest immediately before the write
	// catches the case that actually loses data — another client updating _refs
	// while this one was busy merging — and retries against fresh state instead
	// of overwriting them.
	//
	// Because the digest check is the real guard, the merge is computed
	// *outside* the lock and the lock covers only the re-check and the write.
	// The merge is the expensive half: it re-reads the index with its own
	// retries and enumerates every tag in the repository, which on a wide
	// repository over a slow link took long enough to routinely outlast the
	// lock's TTL — and a lock that expires mid-update is worse than no lock,
	// because the next client acquires it legitimately and the two interleave
	// exactly the update the lock exists to serialise. Anything that changes
	// while the merge runs unlocked is caught by the re-check under the lock.
	//
	// Retrying converges: each attempt re-reads whatever is on the registry and
	// layers this push's refs on top, so concurrent updates to *different* refs
	// all survive. Concurrent updates to the *same* ref are still last-writer-
	// wins, but they are no longer silent.
	var lastConflict error
	for attempt := 1; attempt <= refsIndexMaxAttempts; attempt++ {
		baseline, baseErr := c.refIndexDigest(ctx)
		if baseErr != nil {
			return fmt.Errorf("failed to read the _refs index state: %w", baseErr)
		}

		remoteRefs := c.mergeRemoteRefs(ctx, refs, deleted)

		head, headErr := c.currentHead(ctx)
		if headErr != nil {
			return fmt.Errorf("failed to read the recorded HEAD: %w", headErr)
		}
		if head == "" {
			head = headHint
		}
		if head != "" {
			if _, stillThere := remoteRefs[head]; !stillThere {
				// The recorded HEAD was deleted. Drop it rather than advertise
				// a ref that is no longer there.
				head = ""
			}
		}

		conflict, err := c.commitRefIndex(ctx, baseline, remoteRefs, head)
		if err != nil {
			return err
		}
		if conflict != nil {
			lastConflict = conflict
			continue
		}
		return nil
	}
	return fmt.Errorf("gave up updating the _refs index after %d attempts: %w", refsIndexMaxAttempts, lastConflict)
}

// commitRefIndex takes the index lock, verifies nothing moved since baseline,
// and writes.
//
// A non-nil first return is a lost race, not a failure: the caller re-merges
// against the newer state and tries again.
func (c *Client) commitRefIndex(ctx context.Context, baseline string, remoteRefs map[string]RefEntry, head string) (conflict error, err error) {
	if lockErr := c.acquireRefsIndexLock(ctx); lockErr != nil {
		return nil, lockErr
	}
	defer c.releaseRefsIndexLock(ctx)

	// Re-check under the lock. Anything that changed since baseline means the
	// merge was computed from stale state.
	current, curErr := c.refIndexDigest(ctx)
	if curErr != nil {
		return nil, fmt.Errorf("failed to re-read the _refs index state: %w", curErr)
	}
	if current != baseline {
		return fmt.Errorf("the _refs index changed from %s to %s while this push was merging",
			shortDigest(baseline), shortDigest(current)), nil
	}

	if pushErr := c.pushRichRefIndexDirect(ctx, remoteRefs, head); pushErr != nil {
		return nil, pushErr
	}
	return nil, nil
}

// acquireRefsIndexLock takes the lock serialising _refs updates.
func (c *Client) acquireRefsIndexLock(ctx context.Context) error {
	if _, err := c.AcquireRefLockWithRetry(ctx, refsIndexLockRef, c.RefsIndexLockTTL, refsIndexLockWait); err != nil {
		return fmt.Errorf("failed to acquire _refs index lock: %w", err)
	}
	return nil
}

// releaseRefsIndexLock releases it, on a context of its own so that a cancelled
// push still gives the lock back rather than leaving the ref stalled until the
// TTL runs out.
func (c *Client) releaseRefsIndexLock(ctx context.Context) {
	releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := c.ReleaseRefLock(releaseCtx, refsIndexLockRef); err != nil {
		fmt.Fprintf(os.Stderr, "git-remote-oci: warning: failed to release the _refs index lock: %v\n", err)
	}
}

// mergeRemoteRefs reads the current published refs and layers this push on top.
func (c *Client) mergeRemoteRefs(ctx context.Context, refs map[string]RefEntry, deleted map[string]bool) map[string]RefEntry {
	var remoteRefs map[string]RefEntry
	for retry := 0; retry < 5; retry++ {
		c.manifestCache.Delete(TagRefIndex)
		rRefs, fetchErr := c.FetchRichRefIndex(ctx)
		if fetchErr == nil && rRefs != nil {
			remoteRefs = rRefs
			break
		}
		if IsNotFound(fetchErr) {
			break
		}
		select {
		case <-ctx.Done():
			return map[string]RefEntry{}
		case <-time.After(50 * time.Millisecond):
		}
	}
	if remoteRefs == nil {
		remoteRefs = make(map[string]RefEntry, len(refs))
	}

	if listRefs, err := c.ListRefs(ctx); err == nil && len(listRefs) > 0 {
		for rName, rSHA := range listRefs {
			if _, exists := remoteRefs[rName]; !exists {
				remoteRefs[rName] = RefEntry{SHA: rSHA}
			}
		}
	}

	for k, v := range refs {
		remoteRefs[k] = v
	}
	for name := range deleted {
		delete(remoteRefs, name)
	}
	return remoteRefs
}

// refIndexDigest returns the digest of the published _refs manifest, or "" when
// the repository has no index yet.
//
// "" is a real state, not an error: it is what a fresh repository looks like,
// and two pushes racing to create the first index must be able to tell that
// apart from one of them having already won.
func (c *Client) refIndexDigest(ctx context.Context) (string, error) {
	desc, err := c.Repo.Resolve(ctx, TagRefIndex)
	if err != nil {
		if IsNotFound(err) {
			return "", nil
		}
		return "", err
	}
	return desc.Digest.String(), nil
}

// shortDigest abbreviates a digest for an error message.
func shortDigest(d string) string {
	if d == "" {
		return "absent"
	}
	if i := strings.IndexByte(d, ':'); i >= 0 && len(d) > i+13 {
		return d[:i+13]
	}
	return d
}

func (c *Client) pushRichRefIndexDirect(ctx context.Context, refs map[string]RefEntry, head string) error {
	c.manifestCache.Delete(TagRefIndex)

	var lastErr error
	for attempt := 0; attempt < 5; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(50*attempt) * time.Millisecond)
			c.manifestCache.Delete(TagRefIndex)
		}

		indexBytes, err := json.Marshal(refs)
		if err != nil {
			return fmt.Errorf("failed to marshal ref index: %w", err)
		}

		indexDigest := opencontainers.FromBytes(indexBytes)
		indexDesc := ocispec.Descriptor{
			MediaType: MediaTypeGitIndex,
			Digest:    indexDigest,
			Size:      int64(len(indexBytes)),
		}

		if err := c.Repo.Push(ctx, indexDesc, bytes.NewReader(indexBytes)); err != nil {
			lastErr = fmt.Errorf("failed to push ref index blob: %w", err)
			continue
		}

		configObj := ocispec.Image{
			Platform: ocispec.Platform{
				Architecture: "unknown",
				OS:           "unknown",
			},
		}
		configBytes, err := json.Marshal(configObj)
		if err != nil {
			return fmt.Errorf("failed to marshal config blob: %w", err)
		}

		configDigest := opencontainers.FromBytes(configBytes)
		configDesc := ocispec.Descriptor{
			MediaType: MediaTypeGitConfig,
			Digest:    configDigest,
			Size:      int64(len(configBytes)),
		}
		if err := c.pushBlobOnce(ctx, configDesc, configBytes); err != nil {
			lastErr = fmt.Errorf("failed to push ref index config blob: %w", err)
			continue
		}

		manifest := ocispec.Manifest{
			Versioned:   specs.Versioned{SchemaVersion: 2},
			MediaType:   ocispec.MediaTypeImageManifest,
			Config:      configDesc,
			Layers:      []ocispec.Descriptor{indexDesc},
			Annotations: indexAnnotations(head),
		}

		manifestBytes, err := json.Marshal(manifest)
		if err != nil {
			return fmt.Errorf("failed to marshal index manifest: %w", err)
		}

		manifestDigest := opencontainers.FromBytes(manifestBytes)
		manifestDesc := ocispec.Descriptor{
			MediaType: ocispec.MediaTypeImageManifest,
			Digest:    manifestDigest,
			Size:      int64(len(manifestBytes)),
		}

		err = c.Repo.PushReference(ctx, manifestDesc, bytes.NewReader(manifestBytes), TagRefIndex)
		if err == nil {
			c.manifestCache.Store(TagRefIndex, &manifest)
			c.manifestCache.Store(manifestDigest.String(), &manifest)
			_ = c.PushOCIImageIndex(ctx, TagOCIIndex, refs, head)
			return nil
		}
		lastErr = err
	}
	return lastErr
}

// PushRefIndex publishes an updated ref mapping (ref -> SHA) to the _refs tag.
func (c *Client) PushRefIndex(ctx context.Context, refs map[string]string) error {
	richRefs := make(map[string]RefEntry, len(refs))
	for refName, sha := range refs {
		richRefs[refName] = RefEntry{SHA: sha}
	}
	return c.PushRichRefIndex(ctx, richRefs, nil)
}

// isDeletionUnsupported reports whether err means the registry will not delete
// manifests at all, as opposed to this particular delete having failed.
//
// The distinction decides whether to fall back to a tombstone or to report the
// deletion as failed: falling back on a transient error would leave a tombstone
// over a ref that could have been deleted properly.
func isDeletionUnsupported(err error) bool {
	if err == nil {
		return false
	}
	var errResp *errcode.ErrorResponse
	if errors.As(err, &errResp) {
		switch errResp.StatusCode {
		case http.StatusMethodNotAllowed, http.StatusForbidden, http.StatusNotImplemented:
			return true
		}
	}
	var codeErr errcode.Error
	if errors.As(err, &codeErr) {
		switch codeErr.Code {
		case errcode.ErrorCodeUnsupported, errcode.ErrorCodeDenied:
			return true
		}
	}
	return false
}

// pushRefTombstone overwrites a ref tag with a manifest that marks the ref as
// deleted.
//
// It carries no packfile layer and is annotated as deleted, so neither the
// layer check nor the annotation check in EnumerateTagRefs will treat it as a
// live ref.
func (c *Client) pushRefTombstone(ctx context.Context, refName, targetTag string) error {
	if err := c.pushEmptyBlob(ctx); err != nil {
		return err
	}

	manifest := ocispec.Manifest{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageManifest,
		Config:    ocispec.DescriptorEmptyJSON,
		Layers:    emptyLayers(),
		Annotations: map[string]string{
			AnnotationGitDeleted:            "true",
			AnnotationGitRef:                refName,
			ocispec.AnnotationTitle:         refName + " (deleted)",
			ocispec.AnnotationVendor:        "git-remote-oci",
			ocispec.AnnotationDocumentation: "https://github.com/mrueg/git-remote-oci",
		},
	}

	data, err := json.Marshal(manifest)
	if err != nil {
		return fmt.Errorf("failed to marshal the tombstone manifest for %s: %w", refName, err)
	}
	desc := ocispec.Descriptor{
		MediaType: ocispec.MediaTypeImageManifest,
		Digest:    opencontainers.FromBytes(data),
		Size:      int64(len(data)),
	}
	if err := c.Repo.PushReference(ctx, desc, bytes.NewReader(data), targetTag); err != nil {
		return fmt.Errorf("failed to push the tombstone for %s: %w", targetTag, err)
	}
	c.manifestCache.Delete(targetTag)
	return nil
}

// RefTagSnapshot records what a ref tag pointed at, so a failed batch can put
// it back.
//
// Exists reports whether the tag was present at all; a ref created by the batch
// has nothing to restore and must be removed instead.
type RefTagSnapshot struct {
	RefName string
	Desc    ocispec.Descriptor
	Exists  bool
}

// SnapshotRefTag captures the current target of a ref's tag.
func (c *Client) SnapshotRefTag(ctx context.Context, refName string) (RefTagSnapshot, error) {
	snap := RefTagSnapshot{RefName: refName}
	desc, err := c.ResolveRefManifest(ctx, refName)
	if err != nil {
		if IsNotFound(err) {
			return snap, nil
		}
		return snap, fmt.Errorf("failed to read the current target of %s: %w", refName, err)
	}
	snap.Desc, snap.Exists = desc, true
	return snap, nil
}

// RestoreRefTag puts a ref tag back where the snapshot found it.
//
// Restoring is re-tagging an existing manifest, which is an ordinary tag write
// and works on every registry. Removing a tag the batch created needs deletion,
// which some registries refuse; that failure is reported so the caller can say
// so rather than implying the rollback was complete.
func (c *Client) RestoreRefTag(ctx context.Context, snap RefTagSnapshot) error {
	tag := RefManifestTag(snap.RefName)
	if tag == "" {
		return fmt.Errorf("ref %q cannot be represented as an OCI tag", snap.RefName)
	}
	c.InvalidateManifestCache(tag)

	if !snap.Exists {
		desc, err := c.Repo.Resolve(ctx, tag)
		if err != nil {
			if IsNotFound(err) {
				return nil
			}
			return fmt.Errorf("failed to resolve %s for removal: %w", tag, err)
		}
		if err := c.Repo.Delete(ctx, desc); err != nil {
			return fmt.Errorf("failed to remove %s, which this push created: %w", tag, err)
		}
		return nil
	}

	if err := c.Repo.Tag(ctx, snap.Desc, tag); err != nil {
		return fmt.Errorf("failed to restore %s to %s: %w", tag, snap.Desc.Digest, err)
	}
	return nil
}

// DeleteRef deletes a reference tag from the OCI registry and updates the _refs index.
func (c *Client) DeleteRef(ctx context.Context, refName string) error {
	if _, lockErr := c.AcquireRefLockWithRetry(ctx, refsIndexLockRef, c.RefsIndexLockTTL, refsIndexLockWait); lockErr != nil {
		return fmt.Errorf("failed to acquire _refs index lock for deletion: %w", lockErr)
	}
	defer func() {
		releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if err := c.ReleaseRefLock(releaseCtx, refsIndexLockRef); err != nil {
			fmt.Fprintf(os.Stderr, "git-remote-oci: warning: failed to release the _refs index lock: %v\n", err)
		}
	}()

	c.ClearManifestCache()

	richRefs, err := c.FetchRichRefIndex(ctx)
	if err != nil {
		refs, listErr := c.ListRefs(ctx)
		if listErr != nil {
			return fmt.Errorf("failed to list refs prior to deletion: %w", listErr)
		}
		richRefs = make(map[string]RefEntry, len(refs))
		for k, v := range refs {
			richRefs[k] = RefEntry{SHA: v}
		}
	}

	delete(richRefs, refName)

	// One tag per ref, so there is exactly one thing to remove.
	for _, targetTag := range []string{RefManifestTag(refName)} {
		if targetTag == "" {
			continue
		}
		c.ClearManifestCache()
		desc, err := c.Repo.Resolve(ctx, targetTag)
		switch {
		case err == nil:
			delErr := c.Repo.Delete(ctx, desc)
			switch {
			case delErr == nil:
			case isDeletionUnsupported(delErr):
				// Several hosted registries refuse manifest deletion outright.
				// Leaving the tag as it is would let the tag-enumeration
				// fallback rediscover it and resurrect the ref on the next
				// push, so overwrite it with a tombstone instead: the tag
				// survives, but it no longer describes a ref.
				if tombErr := c.pushRefTombstone(ctx, refName, targetTag); tombErr != nil {
					return fmt.Errorf("registry refuses manifest deletion (%s) and the tombstone for %s could not be written: %w", delErr.Error(), targetTag, tombErr)
				}
			default:
				// Any other failure leaves a live tag behind, which would be
				// rediscovered and resurrect the ref. Report it rather than
				// claiming a deletion that will not stay done.
				return fmt.Errorf("failed to delete tag %s from the registry: %w", targetTag, delErr)
			}
		case IsNotFound(err):
			// Already gone; nothing to delete.
		default:
			return fmt.Errorf("failed to resolve tag %s for deletion: %w", targetTag, err)
		}
	}

	c.ClearManifestCache()

	// Carry HEAD across the deletion. Writing the index without it silently
	// erased the recorded default branch on the first `git push origin :ref`.
	head, headErr := c.currentHead(ctx)
	if headErr != nil {
		return fmt.Errorf("failed to read the recorded HEAD before deleting %s: %w", refName, headErr)
	}
	if _, stillThere := richRefs[head]; head != "" && !stillThere {
		head = ""
	}
	return c.pushRichRefIndexDirect(ctx, richRefs, head)
}

// CommitPush describes one commit publication.
//
// It is a struct rather than a parameter list because the call has ten-odd
// inputs, most of them strings, and a positional call was already impossible to
// read at the call site.
type CommitPush struct {
	// CommitSHA is the commit the manifest is published for.
	CommitSHA string
	// RefName is the full git ref name, e.g. "refs/heads/main". Required
	// whenever RefTag is set.
	RefName string
	// RefTag is the tag the ref manifest is published under. Empty publishes
	// only the ref-agnostic commit manifest.
	RefTag string
	// Parents is the comma-separated list of the commit's git parents. It is
	// metadata; see PackBases for what fetch actually follows.
	Parents string
	// PackBases are the commits the packfile was cut against. Empty means the
	// packfile is self-contained and is recorded as PackBasesNone.
	PackBases []string
	// PushCert records the pushcert option value. It is not a signature.
	PushCert string
	// UpdateIndex additionally rewrites the _refs index.
	UpdateIndex bool
	// TagAnnotations are merged into the ref manifest's annotations.
	TagAnnotations map[string]string
	// ExtraLayers are appended after the packfile layer, e.g. LFS blobs.
	ExtraLayers []ocispec.Descriptor
	// Rewrite republishes a commit this client may already have pushed, with
	// different content.
	//
	// The skip-if-already-pushed caches below are keyed on the commit id and
	// the ref name, not on what is being published, which is right for the
	// case they exist for — one push touching the same commit from two
	// branches — and wrong for gc, whose whole job is to replace a commit's
	// packfile with a self-contained one. Without this, a consolidation run by
	// a client that had already pushed that ref silently did nothing.
	Rewrite bool
}

// validate rejects a push that could publish an unfetchable manifest.
func (p *CommitPush) validate() error {
	if !isObjectID(p.CommitSHA) {
		return fmt.Errorf("invalid commit SHA %q: must be a hex object id of 40 (SHA-1) or 64 (SHA-256) characters", p.CommitSHA)
	}
	for _, base := range p.PackBases {
		if !isObjectID(base) {
			return fmt.Errorf("invalid pack base %q for commit %s: must be a hex object id of 40 (SHA-1) or 64 (SHA-256) characters", base, p.CommitSHA)
		}
		if base == p.CommitSHA {
			return fmt.Errorf("commit %s cannot be its own pack base", p.CommitSHA)
		}
	}
	return nil
}

// PushCommitStream pushes a packfile layer stream, config blob, and manifest tagged with p.CommitSHA and p.RefTag.
// If p.RefTag sanitises to a 40-character hex string (e.g. branch named after a SHA or matching p.CommitSHA), the ref manifest
// is tagged with "ref-<sanitisedTag>" so ListRefs can discover it and commitSHA tags remain ref-agnostic.
// If packfileSize > 0, the stream is validated to contain exactly packfileSize bytes.
// If packfileSize <= 0, the stream is read until EOF.
func (c *Client) PushCommitStream(
	ctx context.Context,
	p CommitPush,
	packfileReader io.Reader,
	packfileSize int64,
) error {
	if err := p.validate(); err != nil {
		return err
	}

	reader := io.Reader(packfileReader)
	if packfileSize > 0 {
		limit := packfileSize
		if limit < math.MaxInt64 {
			limit++
		}
		reader = io.LimitReader(packfileReader, limit)
	}

	// rootfs.diff_ids must name the *uncompressed* layer, so it is digested on
	// the way in rather than reusing the layer digest. Those coincide only when
	// OCI_COMPRESSION is unset or "none", which is the default and is why the
	// discrepancy went unnoticed under gzip and zstd.
	//
	// Teeing keeps this streaming: the uncompressed bytes are never held, only
	// hashed as they pass through into the compressor.
	diffDigester := opencontainers.SHA256.Digester()
	reader = io.TeeReader(reader, diffDigester.Hash())

	compMode := c.Compression
	mediaType, err := compressedMediaType(compMode)
	if err != nil {
		return err
	}

	// Stage the packfile on disk rather than in memory. The registry needs its
	// digest and length before the upload can begin, and a packfile is as large
	// as the history it carries.
	blob, rawSize, err := spoolBlob(mediaType, "packfile", func(w io.Writer) (int64, error) {
		cw, _, cErr := CompressStream(w, compMode)
		if cErr != nil {
			return 0, fmt.Errorf("failed to create compression stream writer: %w", cErr)
		}
		n, copyErr := io.Copy(cw, reader)
		if closeErr := cw.Close(); closeErr != nil && copyErr == nil {
			copyErr = fmt.Errorf("compression writer close failed: %w", closeErr)
		}
		if copyErr != nil {
			return 0, fmt.Errorf("failed to write packfile stream: %w", copyErr)
		}
		return n, nil
	})
	if err != nil {
		return err
	}
	defer func() { _ = blob.Close() }()

	// Validate that the raw uncompressed stream length matches packfileSize exactly when packfileSize > 0.
	if packfileSize > 0 {
		if rawSize < packfileSize {
			return fmt.Errorf("packfile size mismatch: expected %d bytes, got %d bytes", packfileSize, rawSize)
		}
		if rawSize > packfileSize {
			return fmt.Errorf("packfile size mismatch: stream exceeds expected size of %d bytes", packfileSize)
		}
	}

	return c.pushCommitArtifacts(ctx, p, blob.desc, diffDigester.Digest(), blob.Reader())
}

// pushCommitArtifacts pushes the packfile layer blob, config blob, commit manifest, and optional ref manifest.
func (c *Client) pushCommitArtifacts(
	ctx context.Context,
	p CommitPush,
	packfileDesc ocispec.Descriptor,
	packfileDiffID opencontainers.Digest,
	packfileReader io.Reader,
) error {
	commitSHA := p.CommitSHA
	refName := p.RefName
	// 0. Fast check: skip only when *both* the ref-agnostic commit manifest and
	// the ref manifest have already been pushed in this process (e.g. a
	// multi-branch push touching the same commit).
	//
	// IsRefFullyPushed takes the ref *name*: it derives the tag itself, so
	// handing it p.RefTag would ask about a doubly-encoded tag that is never in
	// the cache.
	if !p.Rewrite && c.IsRefFullyPushed(commitSHA, refName) {
		return nil
	}

	// 1. Fast HEAD check: if packfile layer blob already exists on OCI registry, skip blob upload!
	exists, err := c.Repo.Blobs().Exists(ctx, packfileDesc)
	if err != nil || !exists {
		if pushErr := c.Repo.Push(ctx, packfileDesc, packfileReader); pushErr != nil {
			return c.explainAuth(fmt.Errorf("failed to push packfile layer blob stream: %w", pushErr))
		}
	}

	// 2. Push Minimal OCI Image Config Blob for broad registry compatibility.
	// Use "unknown" platform since Git packfiles are platform-agnostic artifacts.
	// RootFS.DiffIDs names the uncompressed layer, which equals the layer digest
	// only when the packfile is stored raw.
	diffID := packfileDiffID
	if diffID == "" {
		diffID = packfileDesc.Digest
	}
	configObj := ocispec.Image{
		Platform: ocispec.Platform{
			Architecture: "unknown",
			OS:           "unknown",
		},
		RootFS: ocispec.RootFS{
			Type:    "layers",
			DiffIDs: []opencontainers.Digest{diffID},
		},
	}

	configBytes, err := json.Marshal(configObj)
	if err != nil {
		return fmt.Errorf("failed to marshal config blob: %w", err)
	}

	configDigest := opencontainers.FromBytes(configBytes)
	configDesc := ocispec.Descriptor{
		MediaType: MediaTypeGitConfig,
		Digest:    configDigest,
		Size:      int64(len(configBytes)),
	}

	err = c.pushBlobOnce(ctx, configDesc, configBytes)
	if err != nil {
		return fmt.Errorf("failed to push config blob: %w", err)
	}

	// 3a. Create Ref-Agnostic Commit SHA Manifest (contains revision & parents only)
	commitAnnotations := map[string]string{
		ocispec.AnnotationRevision:      commitSHA,
		ocispec.AnnotationTitle:         commitSHA,
		ocispec.AnnotationVendor:        "git-remote-oci",
		ocispec.AnnotationDocumentation: "https://github.com/mrueg/git-remote-oci",
		// Mandatory, including for a self-contained packfile, which says
		// PackBasesNone rather than nothing at all.
		AnnotationGitPackBases: FormatPackBases(p.PackBases),
	}
	if p.Parents != "" {
		commitAnnotations[AnnotationGitParents] = p.Parents
	}
	if p.PushCert != "" {
		commitAnnotations[AnnotationGitPushCert] = p.PushCert
	}

	manifestLayers := append([]ocispec.Descriptor{packfileDesc}, p.ExtraLayers...)
	commitManifest := ocispec.Manifest{
		Versioned:   specs.Versioned{SchemaVersion: 2},
		MediaType:   ocispec.MediaTypeImageManifest,
		Config:      configDesc,
		Layers:      manifestLayers,
		Annotations: commitAnnotations,
	}

	commitManifestData, err := json.Marshal(commitManifest)
	if err != nil {
		return fmt.Errorf("failed to marshal commit manifest: %w", err)
	}

	commitManifestDesc := ocispec.Descriptor{
		MediaType: ocispec.MediaTypeImageManifest,
		Digest:    opencontainers.FromBytes(commitManifestData),
		Size:      int64(len(commitManifestData)),
	}

	// Push ref-agnostic manifest tagged with commitSHA if not already cached
	if _, ok := c.manifestCache.Load(commitSHA); p.Rewrite || !ok {
		err = c.Repo.PushReference(ctx, commitManifestDesc, bytes.NewReader(commitManifestData), commitSHA)
		if err != nil {
			return fmt.Errorf("failed to push commit SHA tag %s: %w", commitSHA, err)
		}
		c.manifestCache.Store(commitSHA, &commitManifest)
		c.manifestCache.Store(commitManifestDesc.Digest.String(), &commitManifest)
	}

	// 3b. If refTag is provided, push a separate manifest for the ref tag containing AnnotationGitRef.
	// To keep the commitSHA tag immutable and prevent collision/overwriting when sanitisedTag == commitSHA,
	// publish the ref-annotated manifest under a "ref-" prefixed tag if a collision occurs.
	if p.RefTag != "" {
		if refName == "" {
			return fmt.Errorf("refName cannot be empty when refTag %q is provided", p.RefTag)
		}
		// Encode from the ref name, not from refTag: refTag is only a display
		// hint, while refName is what has to map injectively onto a tag.
		targetTag := RefManifestTag(refName)
		if targetTag == "" {
			return fmt.Errorf("ref %q cannot be represented as an OCI tag", refName)
		}

		refAnnotations := map[string]string{
			ocispec.AnnotationRevision:      commitSHA,
			AnnotationGitRef:                refName,
			ocispec.AnnotationTitle:         refName,
			ocispec.AnnotationVendor:        "git-remote-oci",
			ocispec.AnnotationDocumentation: "https://github.com/mrueg/git-remote-oci",
			// Recorded here as well as on the commit manifest: a fetch that
			// resolves a ref goes straight to this manifest and never reads the
			// commit-tagged one.
			AnnotationGitPackBases: FormatPackBases(p.PackBases),
		}
		if p.Parents != "" {
			refAnnotations[AnnotationGitParents] = p.Parents
		}
		if p.PushCert != "" {
			refAnnotations[AnnotationGitPushCert] = p.PushCert
		}
		for k, v := range p.TagAnnotations {
			if v != "" {
				refAnnotations[k] = v
			}
		}

		refManifest := ocispec.Manifest{
			Versioned:   specs.Versioned{SchemaVersion: 2},
			MediaType:   ocispec.MediaTypeImageManifest,
			Config:      configDesc,
			Layers:      manifestLayers,
			Annotations: refAnnotations,
		}

		refManifestData, err := json.Marshal(refManifest)
		if err != nil {
			return fmt.Errorf("failed to marshal ref manifest: %w", err)
		}

		refManifestDesc := ocispec.Descriptor{
			MediaType: ocispec.MediaTypeImageManifest,
			Digest:    opencontainers.FromBytes(refManifestData),
			Size:      int64(len(refManifestData)),
		}

		// Skip the push only when this client already published *this exact
		// manifest* under the tag. The guard used to be "is the tag in the
		// manifest cache at all", which is a question about the tag rather
		// than about its content: once a ref manifest had been pushed, the
		// same client pushing a later commit to the same ref silently did not
		// move the tag. A fresh process per push hid it; anything reusing a
		// client for two pushes of one ref did not.
		digest := refManifestDesc.Digest.String()
		if published, ok := c.refTagDigests.Load(targetTag); p.Rewrite || !ok || published != digest {
			err = c.Repo.PushReference(ctx, refManifestDesc, bytes.NewReader(refManifestData), targetTag)
			if err != nil {
				return fmt.Errorf("failed to push ref tag %s: %w", targetTag, err)
			}
			c.refTagDigests.Store(targetTag, digest)
			c.manifestCache.Store(targetTag, &refManifest)
			c.manifestCache.Store(digest, &refManifest)
		}
	}

	// 4. Update the repository _refs index for O(1) list queries
	if p.UpdateIndex && refName != "" {
		currentRefs, _ := c.ListRefs(ctx)
		if currentRefs == nil {
			currentRefs = make(map[string]string)
		}
		currentRefs[refName] = commitSHA
		if err := c.PushRefIndex(ctx, currentRefs); err != nil {
			fmt.Fprintf(os.Stderr, "git-remote-oci: warning: failed to update _refs index: %v\n", err)
		}
	}

	return nil
}

// FetchManifest fetches an OCI image manifest by tag or digest, utilising the manifest cache if available.
func (c *Client) FetchManifest(ctx context.Context, tagOrDigest string) (*ocispec.Manifest, error) {
	if tagOrDigest != "" && tagOrDigest != TagRefIndex && tagOrDigest != TagOCIIndex {
		if cached, ok := c.manifestCache.Load(tagOrDigest); ok {
			if manifest, isManifest := cached.(*ocispec.Manifest); isManifest {
				return manifest, nil
			}
		}
	}

	desc, rc, err := c.Repo.FetchReference(ctx, tagOrDigest)
	if err != nil {
		if errors.Is(err, errdef.ErrNotFound) {
			return nil, fmt.Errorf("%w: %s: %w", ErrManifestNotFound, tagOrDigest, err)
		}
		var errResp *errcode.ErrorResponse
		if errors.As(err, &errResp) && errResp.StatusCode == http.StatusNotFound {
			return nil, fmt.Errorf("%w: %s: %w", ErrManifestNotFound, tagOrDigest, err)
		}
		return nil, c.explainAuth(fmt.Errorf("failed to fetch reference %s: %w", tagOrDigest, err))
	}
	defer func() { _ = rc.Close() }()

	// Validate media type descriptor up front (parsing media type to strip parameters like "; charset=utf-8")
	mediaType, _, err := mime.ParseMediaType(desc.MediaType)
	if err != nil {
		mediaType = strings.TrimSpace(strings.Split(desc.MediaType, ";")[0])
	}
	if mediaType != ocispec.MediaTypeImageManifest && mediaType != "application/vnd.docker.distribution.manifest.v2+json" {
		return nil, fmt.Errorf("%w: reference %s has media type %s", ErrNotAnImageManifest, tagOrDigest, desc.MediaType)
	}

	data, err := io.ReadAll(rc)
	if err != nil {
		return nil, fmt.Errorf("failed to read manifest data: %w", err)
	}

	var manifest ocispec.Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil, fmt.Errorf("%w: failed to unmarshal manifest for %s: %w", ErrNotAnImageManifest, tagOrDigest, err)
	}

	if tagOrDigest != "" {
		c.manifestCache.Store(tagOrDigest, &manifest)
	}
	if digestStr := desc.Digest.String(); digestStr != "" {
		c.manifestCache.Store(digestStr, &manifest)
	}

	return &manifest, nil
}

// InvalidateManifestCache removes a cached manifest entry for the given tag or digest.
func (c *Client) InvalidateManifestCache(tagOrDigest string) {
	if tagOrDigest != "" {
		c.manifestCache.Delete(tagOrDigest)
	}
}

// ClearManifestCache clears all cached manifests.
func (c *Client) ClearManifestCache() {
	c.manifestCache.Range(func(key, value any) bool {
		c.manifestCache.Delete(key)
		return true
	})
}

func isPackfileMediaType(mediaType string) bool {
	return mediaType == MediaTypeGitPackfile ||
		mediaType == MediaTypeGitPackfileGzip ||
		mediaType == MediaTypeGitPackfileZstd
}

func hasGitPackfileLayer(manifest *ocispec.Manifest) bool {
	for i := range manifest.Layers {
		// A snapshot layer carries a packfile media type too, so it has to be
		// excluded by name rather than by relying on layer order.
		if isSnapshotLayer(manifest.Layers[i]) {
			continue
		}
		if isPackfileMediaType(baseMediaType(manifest.Layers[i].MediaType)) {
			return true
		}
	}
	return false
}

// FetchPackfileStream returns a ReadCloser stream of the packfile layer content from an OCI manifest.
func (c *Client) FetchPackfileStream(ctx context.Context, manifest *ocispec.Manifest) (io.ReadCloser, error) {
	var packfileDesc *ocispec.Descriptor
	for i := range manifest.Layers {
		if isSnapshotLayer(manifest.Layers[i]) {
			continue
		}
		if isPackfileMediaType(baseMediaType(manifest.Layers[i].MediaType)) {
			packfileDesc = &manifest.Layers[i]
			break
		}
	}
	if packfileDesc == nil {
		return nil, fmt.Errorf("no valid git packfile layer found in manifest")
	}

	rc, err := c.Repo.Fetch(ctx, *packfileDesc)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch packfile layer blob: %w", err)
	}
	return DecompressStream(rc, packfileDesc.MediaType)
}

// PushLFSLayer uploads a Git LFS object as a layer.
//
// An LFS object id *is* the SHA-256 of its content, so the descriptor is built
// from the id and the pointer's recorded size rather than by reading the object
// to measure it. The content then streams to the registry through a verifying
// reader.
//
// That verification is the point. The previous implementation digested whatever
// it happened to read and published the blob under *that* digest, so a local
// object that did not match the id it is filed under was uploaded anyway, under
// a digest disagreeing with its own OID annotation. Now the push fails.
//
// It also avoids holding the object in memory, though measurement showed that
// is not what dominates peak usage on a push - go-git's packfile encoder is.
func (c *Client) PushLFSLayer(ctx context.Context, oid string, r io.Reader, size int64) (ocispec.Descriptor, error) {
	cleanOID, err := lfs.ValidateOID(oid)
	if err != nil {
		return ocispec.Descriptor{}, err
	}
	if size < 0 {
		return ocispec.Descriptor{}, fmt.Errorf("LFS blob %s has a negative size %d", cleanOID, size)
	}

	desc := ocispec.Descriptor{
		MediaType: lfs.MediaTypeGitLFSBlob,
		Digest:    opencontainers.NewDigestFromEncoded(opencontainers.SHA256, cleanOID),
		Size:      size,
		Annotations: map[string]string{
			lfs.AnnotationLFSOID:  cleanOID,
			lfs.AnnotationLFSSize: strconv.FormatInt(size, 10),
		},
	}

	// VerifyReader fails the read if the content does not hash to desc.Digest
	// or is not exactly desc.Size bytes, so a corrupt local object cannot be
	// published under a name that misdescribes it.
	vr := content.NewVerifyReader(r, desc)
	if err := c.Repo.Blobs().Push(ctx, desc, vr); err != nil {
		if errors.Is(err, errdef.ErrAlreadyExists) {
			return desc, nil
		}
		return desc, fmt.Errorf("failed to push LFS blob layer to OCI registry: %w", err)
	}
	if err := vr.Verify(); err != nil {
		return desc, fmt.Errorf("LFS object %s does not match its object id: %w", cleanOID, err)
	}

	return desc, nil
}

// FetchLFSLayer fetches an LFS binary layer stream from the OCI registry by descriptor.
func (c *Client) FetchLFSLayer(ctx context.Context, desc ocispec.Descriptor) (io.ReadCloser, error) {
	return c.Repo.Blobs().Fetch(ctx, desc)
}
