# Security

## Reporting a vulnerability

Report privately through
[GitHub's advisory form](https://github.com/mrueg/git-remote-oci/security/advisories/new).
Please do not open a public issue for anything exploitable.

Include what you did, what happened, and what you expected. A failing test or a
short reproduction against a local `registry:3` is the fastest route to a fix.

This is a personal, pre-1.0 project with no service-level commitment. Expect a
first response within a couple of weeks.

## What is in scope

The interesting attack surface is **content fetched from a registry**, because a
registry is not a trust boundary: anyone who can push to a repository controls
every byte a client later reads.

In scope:

- Registry-supplied data leading to code execution, path traversal, or writes
  outside `$GIT_DIR` — manifests, annotations, packfiles, Git LFS pointers and
  object ids all arrive this way.
- Resource exhaustion from hostile content, such as decompression bombs.
- Credentials leaking into logs, error messages, or the registry.
- A fetch that silently produces an incomplete or wrong object graph. Git's
  correctness depends on it, so we treat it as a security-adjacent bug even
  though it is not exploitable.

Out of scope:

- Advisory locking being bypassed. It is documented as advisory: registries
  offer no compare-and-swap, so it narrows races without closing them. See
  [FORMAT.md §9](FORMAT.md#9-locks).
- `org.opencontainers.image.signature` not being a signature. It records the
  `pushcert` option value and is documented as **not** provenance.
- Anything requiring push access to the repository being attacked. Someone who
  can push can already rewrite history.
- Vulnerabilities in registries, in `git` itself, or in dependencies without a
  demonstrated path through this code.

## Notes for reviewers

Two invariants carry most of the weight, both in
[AGENTS.md](AGENTS.md) and [FORMAT.md](FORMAT.md):

- **Every identifier from a registry is validated before use.** Commit ids, LFS
  object ids and ref names become tag names and filesystem paths.
- **Data-loss failures are propagated, never downgraded to a warning.** A
  missing pack base is a hard error precisely because nothing on the registry
  side verifies reachability.
