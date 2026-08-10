# The git-remote-oci on-registry format

**Version 1.** Recorded on the `_refs` index and the `_index` image index as
`io.git-remote-oci.format-version`.

This describes how a git repository is laid out inside an OCI registry, so that the layout can be
inspected, reimplemented, or argued with independently of the Go code that happens to write it
today.

> **Not stable.** This is pre-1.0 software. The format changes in place, under the same version
> number, for the whole 0.x series. There is no compatibility path and no negotiation: a reader that
> meets a version it does not implement **refuses the repository** rather than guessing.
>
> The version is a tripwire against a layout this build does not understand, not a changelog. This
> document is the changelog; the version starts moving at 1.0.
>
> If the code and this document disagree, the code is the truth and this document is a bug.

---

## 1. Why the layout looks like this

A registry is not a filesystem. It offers three things: content-addressed blobs, manifests that
reference blobs, and mutable tags that name manifests. It offers no directories, no transactions,
and no compare-and-swap. Everything below is a consequence.

Two constraints shape the design more than anything else:

- **A tag is the only mutable name**, so every mutable thing — a ref, an index, a lock — is a tag,
  and every update to one is a read-modify-write with no atomicity.
- **A packfile is opaque to the registry.** The registry cannot tell that one packfile depends on
  objects in another, so if a packfile omits objects, something in the format has to say what it
  omitted and where to find them. That is what §4.2 exists for.

---

## 2. Object model

A repository occupies one OCI repository, addressed as `oci://<registry>/<namespace>/<name>`.
Everything lives under tags in that one repository.

| Tag | Holds | Mutable |
| :--- | :--- | :--- |
| `<40-hex commit id>` | commit manifest — a packfile and what it depends on | no, written once |
| `<encoded ref name>` | ref manifest — where a ref points, plus display metadata | yes |
| `_refs` | the ref index: the authoritative list of refs | yes |
| `_index` | an OCI image index over the same refs, for generic tooling | yes |
| `_lfs_locks` | Git LFS lock records | yes |
| `lock-<encoded ref name>` | advisory ref lock | yes |

Tags beginning with `_` are reserved. The ref-name encoding in §3 can never produce a bare `_`
prefix followed by anything other than `_`, `t_`, `r_`, `x_` or `h_`, so a ref can never collide
with a reserved tag.

### 2.1 Media types

| Media type | Used for |
| :--- | :--- |
| `application/vnd.git.repository.packfile.v1` | packfile layer, stored raw |
| `application/vnd.git.repository.packfile.v1+gzip` | packfile layer, gzip |
| `application/vnd.git.repository.packfile.v1+zstd` | packfile layer, zstd |
| `application/vnd.git.repository.index.v1+json` | the `_refs` index blob |
| `application/vnd.git.lfs.v1+blob` | a Git LFS object |
| `application/vnd.oci.image.config.v1+json` | config blob on every manifest |
| `application/vnd.oci.empty.v1+json` | the placeholder layer on manifests with no payload |

The `vnd.git.*` types are specific to this project and are not registered with IANA.

Readers select a decompressor from the layer's media type. A manifest that labels a layer `+gzip`
and stores zstd is malformed; the buffered read path additionally sniffs magic bytes, but nothing
should rely on that.

---

## 3. Ref names to tags

An OCI tag must match `[a-zA-Z0-9_][a-zA-Z0-9._-]{0,127}`. A git ref name may contain `/` and much
else, and the same short name may exist as both a branch and a tag. The mapping must therefore be
**injective**: two distinct refs must never share a tag, because sharing a tag means one silently
overwrites the other's manifest.

```
refs/heads/<name>  ->           escape(<name>)
refs/tags/<name>   ->  "_t_" +  escape(<name>)
refs/<rest>        ->  "_r_" +  escape(<rest>)
<anything else>    ->  "_x_" +  escape(<name>)
```

`escape()` passes `[a-zA-Z0-9.-]` through unchanged, doubles `_` to `__`, and renders every other
byte as `_` followed by two lowercase hex digits.

Because an encoded branch name escapes a leading `_` to `__`, an encoded name can never be mistaken
for the `_t_` / `_r_` / `_x_` markers. The common case stays readable: `refs/heads/main` is the tag
`main`.

| Ref | Tag |
| :--- | :--- |
| `refs/heads/main` | `main` |
| `refs/heads/release-1.2` | `release-1.2` |
| `refs/heads/feature/login` | `feature_2flogin` |
| `refs/heads/my_branch` | `my__branch` |
| `refs/tags/v1.0.0` | `_t_v1.0.0` |
| `refs/notes/commits` | `_r_notes_2fcommits` |
| `HEAD` | `_x_HEAD` |

### 3.1 Over-long refs

A ref whose encoding exceeds 128 bytes is stored as:

```
"_h_" + <encoded prefix> + "-" + <first 8 hex digits of sha256(ref name)>
```

This is **lossy but still injective**: the tag cannot be decoded back to a ref name, so such refs are
discoverable only through `_refs`, but two distinct long refs still get distinct tags.

The `_h_` marker is load-bearing. A truncated tag ends in `-<hex>`, which is also perfectly ordinary
content, so without a reserved marker a truncated long ref and a short ref spelled exactly like the
truncation would produce the same tag. `escape()` can never emit `_h`, because a lone `_` is always
followed by `_` or two hex digits and `h` is not hex.

### 3.2 Refs that look like commit ids

A ref whose encoded tag is 40 hex characters would collide with the commit-manifest namespace. Such a
ref manifest is published under `ref-<tag>` instead.

---

## 4. Commit manifests

Tagged with the commit id. Written once and never rewritten: the same commit always produces the same
tag, and re-pushing it is a no-op.

**A manifest is published only for the tip of each pushed refspec.** Commits between one push and the
next have no manifest of their own — they are inside the packfile. Do not assume an arbitrary commit
is addressable.

```jsonc
{
  "schemaVersion": 2,
  "mediaType": "application/vnd.oci.image.manifest.v1+json",
  "config": { "mediaType": "application/vnd.oci.image.config.v1+json", ... },
  "layers": [
    { "mediaType": "application/vnd.git.repository.packfile.v1", ... },
    { "mediaType": "application/vnd.git.lfs.v1+blob", ... }   // zero or more
  ],
  "annotations": {
    "org.opencontainers.image.revision": "<commit id>",
    "org.opencontainers.image.title":    "<commit id>",
    "org.opencontainers.image.vendor":   "git-remote-oci",
    "io.git-remote-oci.pack-bases":      "none",               // see 4.2
    "io.git-remote-oci.parents":         "<id>,<id>"           // optional, metadata
  }
}
```

The config blob is a minimal OCI image config with platform `unknown/unknown`, present so that
generic registry tooling can render the manifest. Its `rootfs.diff_ids` holds the digest of the
**uncompressed** packfile, which differs from the layer digest whenever compression is on.

### 4.1 The packfile layer

Layer 0 is always the packfile: a standard git packfile as produced by `git pack-objects`, optionally
compressed as a whole. It is incremental — it contains the objects reachable from this commit minus
those reachable from the commits named in `pack-bases`.

It is also **thin**: objects may be stored as deltas whose base is *not* in the pack, but in one of
the packs named by `pack-bases`. This is only sound because §4.2 requires a reader to fetch and
import those bases first; by the time this pack is indexed, its bases are on disk and
`git index-pack --fix-thin` can complete it. A reader that skips the bases will not merely lose
objects, it will fail to index the pack at all — which is the loud failure the invariant is meant to
produce.

### 4.2 Pack bases: the one thing to get right

**Mandatory on every commit and ref manifest.** Value is either the literal `none`, or a
comma-separated list of 40-hex commit ids.

It names the commits the packfile was cut against. It is the **packfile dependency graph**, and it is
what a fetcher must follow.

This is *not* the same as `io.git-remote-oci.parents`, which is the **commit graph** and is metadata
only. The two coincide only when a push carried exactly one commit. Consider pushing `A`, then
committing `B` and `C` and pushing again:

```
commits:   A ─── B ─── C          parents(C) = B
manifests: A             C        B has no manifest
packfiles: [everything]  [C,B minus A]     pack-bases(C) = A
```

A reader following `parents` looks for `B`, does not find it, and stops — never reaching `A`, whose
objects it needs. `git index-pack` accepts the truncated result, git updates the ref, and the
repository is quietly missing objects until a checkout fails. Following `pack-bases` reaches `A`
directly.

The invariant a writer must maintain:

> A manifest, together with the transitive closure of the manifests named in its `pack-bases`,
> contains every object reachable from that manifest's commit.

Which means a base may be named **only** if all three hold:

1. a ref on the registry points at it, so it is published rather than merely local;
2. it is an ancestor of the commit being pushed, so a reader walking back from that commit reaches
   it — this is why a force push that rewrites history must not name the replaced tip;
3. the registry actually serves a manifest for it.

**The graph must be acyclic.** Condition 2 already implies it — ancestry is a partial order, so a
commit cannot be packed against itself or against anything downstream of it — but it is stated
separately because it is the property a reader has to enforce, and enforcing it is not optional. A
cycle has no valid import order: every manifest in it waits for another, so a reader that follows
the ordering rule below without checking for one does not fail, it hangs. Readers must therefore
detect a cycle and report it, never block on it. A manifest naming itself is the degenerate case and
is equally invalid.

Rules for readers:

- Absent, empty, or malformed is an **error**. Do not treat it as `none`.
- A named base that cannot be fetched is an **error**. Do not warn and continue.
- Fetch every base, and finish importing it, **before** importing the packfile that depends on it.
- A cycle among the bases is an **error**. Resolve the graph before importing, so that a cycle is
  found by inspecting it rather than by deadlocking on it.
- A reader may bound how large a graph it will resolve, and fail rather than run unbounded on
  registry-supplied data. This implementation stops at 50 000 manifests and tells the user to run
  `git-remote-oci gc`, which is well beyond any repository that has been compacted even once.

**Corollary:** commit-id tags are load-bearing. They are the pack bases later pushes were cut
against, and cannot be pruned without first rewriting the dependent packfiles as self-contained ones.

### 4.3 Shallow snapshot layer (optional)

A manifest may carry one extra layer annotated `io.git-remote-oci.snapshot: "true"`. It is a
packfile — same media types as §4.1, compression included — holding exactly the objects reachable
from the manifest's commit, with **no ancestry**: the commit, its tree, its sub-trees and its blobs.
Unlike the packfile in §4.1 it is never thin and names no bases; it stands entirely on its own.

It exists for `git clone --depth 1`. A shallow clone needs the boundary commit's complete tree, and
the §4.1 packfiles are incremental, so reconstructing that tree means walking the whole `pack-bases`
chain — which is the entire history, the thing `--depth` was asked to leave out. Compaction does not
help: it collapses the chain to one packfile that still contains every commit.

Rules:

- It is **optional**. A writer may omit it, and this implementation does unless
  `ociremote.shallowSnapshot` is set, which it is not by default. A reader that does not find one
  falls back to §4.2.
- It is **not** the ref's packfile. It carries a packfile media type, so a reader looking for "the
  packfile" must exclude snapshot-annotated layers by annotation rather than relying on layer order.
- A reader may use it **only** when the client asked for depth 1. It contains one commit, so serving
  any other request from it would silently under-deliver.
- Because it is optional and additive, a reader that ignores the annotation entirely still behaves
  correctly: it sees an extra layer it does not recognise and fetches the §4.1 packfile as before.

The cost is a second, undeltified copy of the tip's tree on every push that publishes one.

### 4.4 LFS layers

Git LFS objects referenced by the pushed tree are additional layers of media type
`application/vnd.git.lfs.v1+blob`, annotated:

| Annotation | Value |
| :--- | :--- |
| `org.git.lfs.oid` | the LFS object id, a 64-hex sha256 |
| `org.git.lfs.size` | size in bytes, decimal |

The layer content is the object itself, not a pointer. Readers must verify that the content hashes to
the declared oid before storing it, and must validate the oid before using it to build a path.

No LFS server is involved and there is no Batch API.

---

## 5. Ref manifests

Tagged with the encoded ref name (§3). Same layers as the commit manifest it points at, so a reader
that resolves a ref does not need the commit manifest as well.

Additional annotations:

| Annotation | Meaning |
| :--- | :--- |
| `io.git-remote-oci.ref` | full ref name, e.g. `refs/heads/main` |
| `io.git-remote-oci.tagger` | annotated tag: tagger identity |
| `io.git-remote-oci.tag-message` | annotated tag: message |
| `io.git-remote-oci.tag-signature` | annotated tag: signature |
| `io.git-remote-oci.tag-object` | annotated tag: the tag object's own id |
| `io.git-remote-oci.deleted` | `"true"` on a tombstone; see §8 |
| `org.opencontainers.image.*` | title, authors, created, description, vendor, documentation |

`org.opencontainers.image.signature` is **not written**. It used to carry two unrelated things — an
annotated tag's real GPG/SSH signature, and the value of the `pushcert` push option, which is the
string `"true"` and not a signature at all — with no way for a reader to tell which it had. Publishing
`"true"` under a standard OCI key is worse than publishing nothing, because tooling that trusts the
key trusts that. A tag's signature is in `io.git-remote-oci.tag-signature`, which only ever means
that; the `pushcert` option is accepted so git does not error out, and recorded nowhere, since there
is no server here to verify a certificate against.

For an annotated tag, `org.opencontainers.image.revision` is the commit the tag resolves to, while
`io.git-remote-oci.tag-object` is the tag object itself. The packfile is built from the tag object,
so the tag object is included.

---

## 6. `_refs` — the ref index

The authoritative list of refs, and the only place the format version is recorded.

A manifest whose single layer, of type `application/vnd.git.repository.index.v1+json`, is a JSON
object mapping ref name to entry:

```json
{
  "refs/heads/main": {
    "sha": "f0d5b61268be377529d6aa5585bd30226aab8d03",
    "author": "Alice <alice@example.com>",
    "timestamp": 1700000000,
    "message": "Initial commit"
  },
  "refs/tags/v1.0.0": {
    "sha": "f0d5b61268be377529d6aa5585bd30226aab8d03",
    "tagger": "Alice <alice@example.com>",
    "tag_message": "release 1.0.0",
    "tag_object": "9a1f2b3c4d5e6f708192a3b4c5d6e7f809a1b2c3"
  }
}
```

Every field except `sha` is optional. An entry is always a JSON object; a bare string is not accepted.

The manifest carries `io.git-remote-oci.format-version`. **A reader must check it before acting on
anything in the repository, and refuse a value it does not implement.** `_index` (§7) carries and is
checked for the same value, because it stands in when `_refs` is missing and an unchecked stand-in is
simply an unchecked way in.

It also carries `io.git-remote-oci.head`, naming the ref `HEAD` points at — see §6.1.

Listing refs reads `_refs`. If `_refs` is absent, a reader may fall back to `_index` (§7), and then to
enumerating tags and inspecting each manifest. Tag enumeration is a repair path for a damaged
repository, not a normal one — it is O(tags) round trips and cannot see refs whose tags were
truncated (§3.1).

Updating `_refs` is a read-modify-write and must be done under the `_refs_index_lock` (§9).

### 6.1 `io.git-remote-oci.head`

Names the ref the remote's `HEAD` points at, e.g. `refs/heads/main`. Absent means none is recorded.

A tag points at a manifest, never at another tag, so there is nowhere to put a symref; this
annotation is the substitute. Without it a reader has to guess, and the guess — `main`, else
`master`, else the alphabetically first branch — is wrong for any repository whose default branch is
neither, which clones onto the wrong branch entirely.

**A push never moves it.** Nothing in the remote-helper protocol tells a helper what the remote's
default branch should be, so a push adopts the branch it is pushing only when nothing is recorded
yet. First writer wins, for pushes.

It is not write-once, though: `git-remote-oci set-head` rewrites it, because first-writer-wins is the
right rule for a push and the wrong rule for a repository — it would leave whoever pushed first
having chosen permanently. A writer that changes it must republish the index with the refs otherwise
unchanged, and must refuse a ref the repository does not publish or one outside `refs/heads/`: `HEAD`
is a symbolic ref to a branch, and a reader that finds it naming a tag, or nothing at all, has no
branch to check out.

If the recorded ref is deleted the annotation is dropped rather than left dangling.

---

## 7. `_index` — the OCI image index

A standard `application/vnd.oci.image.index.v1+json` listing the same refs, so that `oras`, `crane`,
`skopeo` and registry web UIs can discover what a repository contains. It is written alongside
`_refs` and carries no information that `_refs` does not — including the format version and the
recorded `HEAD`, both of which a reader must honour here exactly as on `_refs`.

The index itself is annotated `io.git-remote-oci.type: repository-index`, which is how a tool can
tell it apart from an ordinary multi-platform image index.

Each entry points at the corresponding ref manifest, carries platform `unknown/unknown` so it can be
selected at all, and repeats the ref metadata as descriptor annotations — including
`io.git-remote-oci.ref` with the full ref name and `org.opencontainers.image.ref.name` with the
encoded tag. Readers must use `io.git-remote-oci.ref`; there is no inference from the short name.

---

## 8. Deleting a ref

Preferred: delete the ref manifest, then rewrite `_refs` without the ref.

Many hosted registries refuse manifest deletion. Simply leaving the tag would be wrong — tag
enumeration would rediscover it and the ref would come back on the next push — so the ref tag is
instead overwritten with a **tombstone**: a manifest with no packfile layer, carrying

```
"io.git-remote-oci.deleted": "true"
```

Readers must skip tombstones when enumerating tags. The tag and its blobs are not reclaimed; that is
inherent to a registry that will not delete.

A deletion that fails for any other reason must be reported as a failure, not turned into a
tombstone.

---

## 9. Locks

Registries offer no compare-and-swap, so all locking here is **advisory**. It narrows the window for
concurrent writers to clobber each other; it does not close it.

A lock is a manifest under `lock-<encoded ref name>` with no payload layer, carrying:

| Annotation | Meaning |
| :--- | :--- |
| `org.git-remote-oci.lock.ref` | the ref being locked |
| `org.git-remote-oci.lock.owner` | who holds it, or `released` for a tombstone |
| `org.git-remote-oci.lock.expires_at` | RFC 3339 expiry |
| `org.git-remote-oci.lock.id` | unique id of this acquisition |

Acquisition is check, write, then read back and confirm the id is still ours. That catches the
interleaving where another writer wrote between our check and our read, but not the one where it
writes after. Both writers can believe they hold the lock.

An expired lock is not a lock: expiry must be honoured, or a client that died mid-push wedges the ref
forever. Releasing writes a tombstone with owner `released` rather than deleting, because deletion
may be refused. Release must verify the lock id matches.

The pseudo-ref `_refs_index_lock` serialises updates to `_refs`.

`_lfs_locks` holds Git LFS lock records in its config blob. It is not reachable from `git`.

---

## 10. What the format does not do

- **No garbage collection.** Nothing is ever pruned. Commit tags accumulate one per push, and are
  load-bearing (§4.2), so they cannot simply be deleted.
- **No mixing of hash algorithms.** A repository is either SHA-1 or SHA-256 throughout, as in git. Object ids are 40 or 64 hex characters accordingly; readers derive the algorithm from the ids they find rather than from a recorded field, so the two cannot disagree.
- **No shallow or partial representation.** Every packfile carries whole objects; there is nothing a
  reader can use to fetch less.
- **No referrers.** No manifest sets `subject`, and the Referrers API is not used. Association is by
  tag alone.
- **No cross-packfile deduplication.** Identical blobs deduplicate by digest, but overlapping
  packfiles do not.

---

## 11. Changing the format

Any change to a manifest shape, an annotation, a media type, or the tag mapping is a format change.
It must be described in the section it belongs to, in the same commit that makes it: this document
specifies what is written now, not what used to be. It does **not** bump `FormatVersion` during 0.x;
see the note at the top. There is no obligation to read a previous version — and currently no code
that does — but there is an obligation to make the refusal legible, which is what the version
annotation is for.
