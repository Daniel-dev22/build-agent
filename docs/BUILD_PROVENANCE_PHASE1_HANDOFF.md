# Phase 1 — build-agent stamps image provenance

Plan: `control-center/docs/BUILD_PROVENANCE_PLAN.md`. Phase 0's answers are in that
document; this is the first phase that ships code.

## Status

| | |
|---|---|
| Built | yes — `provenance.go` (new), `engine.go` (wiring), `provenance_test.go` (new) |
| Reviewed | **self-review only.** Four independent lenses were NOT run — see *What was not done* |
| Merged | see the branch `feat/image-provenance`; check `git log main` for whether it landed |
| Tagged | **no** |
| Deployed | **no.** A deploy/release/tag hold was in force; nothing has run on kd-nas01 |

**No image has been built with this code.** Every claim below that needed a real
build is listed as unverified, not assumed true.

## What ships

Every `docker buildx build` the agent runs now carries up to four labels:

| label | value | when |
|---|---|---|
| `org.opencontainers.image.source` | clone URL, userinfo stripped | git path only |
| `org.opencontainers.image.revision` | `git rev-parse HEAD` in the clone | git path, best effort |
| `com.controlcenter.build.ref` | `req.Ref` as requested | git path, when a ref was given |
| `com.controlcenter.build.context` | `git` or `upload` | **always** |

An unknown value omits its label entirely. Nothing is ever emitted as `key=`.

## The plan was wrong about the uploaded-context path — read this before phase 2

The plan's phase 1 negative control said *"an uploaded-context build must carry the
ref label but no revision label"*. It cannot carry a ref either. The only
uploaded-context caller in the estate is
`ansible/genmon/include_tasks/build_genmon_image.yaml`, which posts `context_b64`,
`descriptor`, `registry`, `version`, `trigger_key`, `services` — **no `repo`, no
`ref`**. So such a build can populate none of the first three labels, and without a
fourth its image is indistinguishable from an upstream image the agent never
touched.

`com.controlcenter.build.context` is that fourth label, and it is the correction:

🔴 **Phase 2 must derive `built_from_source` from `com.controlcenter.build.context`,
NOT from `com.controlcenter.build.ref`.** The plan says `.ref`; that would mark every
genmon image as "not built by us", which is exactly backwards.

`provenanceFor` also deliberately **discards** any repo/ref a request carries
alongside `context_b64`. The tar did not come from them, and a label naming a source
the image was not built from is worse than no label — it is a confident wrong answer.
There is a test pinning that discard.

## What was verified, and how

**Measured:**
- **20 mutations applied to the working tree, 20 caught.** Not deletions only — the
  four shapes that survive most often: *polarity* (the control-character guard made
  never to fire), *position* (labels emitted after the `-t/-f/--push` tail),
  *boundary* (the length caps moved by one, in both directions), and *arguments*
  (`run` handing `buildService` a fresh `provenance{}` instead of the derived value).
- **The canary distinguishes INVALID from RED.** A deliberate compile break is
  reported as invalid, so "caught" means a test failed rather than that Go could not
  parse the file. One mutation was initially misreported as a survivor for exactly
  this reason (`declared and not used`) and was re-run in a compilable form.
- `go test -race ./...` clean. `prov` is a value struct fully populated before any
  service goroutine starts, so the per-service closures read a frozen copy.
- **The wiring is pinned by reading `run`'s own AST**, not by grepping the file: it
  asserts `provenanceFor` is called, `prov.Revision` is assigned, there is **exactly
  one** `buildService` call site, and that site is passed the identifier `prov`. A
  string in a comment cannot satisfy any of it. `engine.run` needs docker, git, a
  builder and a registry, so this is the honest floor — without it the whole phase
  ships inert with every unit test green.
- `gitRevision` is driven against a **real git repository** created in `t.TempDir()`
  (hermetic identity via `GIT_AUTHOR_*` env, RFC 2606 address — deliberately not
  `git -c user.email`, which is forbidden in this estate and would also make the
  fixture depend on the host's global config).

**Assumed, NOT measured — every one of these needs a real build:**
- That buildx accepts the flags as assembled. No `docker buildx build` has run.
- That `--label` does not invalidate the registry buildcache. The reasoning is that
  labels are applied to the final image config rather than entering the LLB the
  solver keys on — plausible, unverified. **Build the same source twice and confirm
  the second run reports CACHED.**
- That a CLI `--label` overrides a `LABEL` of the same key in a project's Dockerfile.
  Believed yes (CLI applied last). Matters if any built project sets
  `org.opencontainers.image.source` itself.
- That labels land on **each child image of a multi-arch manifest**, not only the
  manifest list. Phase 2 reads labels off the running container's resolved image, so
  a manifest-only label would make phase 2 silently empty on any `platform: auto`
  project. Frigate is `fixed linux/amd64` and will not exercise this.

## Decisions, and what was rejected

- **Best effort revision.** A failed `rev-parse` omits the label, logs, and the build
  continues. Failing a build because metadata could not be read would let a
  bookkeeping step veto the thing it is bookkeeping for.
- **Drop, never repair.** A value with a control character is dropped rather than
  stripped; one over 512 bytes is dropped rather than truncated. Half a URL still
  reads as an answer. Being trusted is this label's only purpose.
- **`req.Repo` is scrubbed of URL userinfo.** Nothing reaches that path with a secret
  today — the token-bearing URL is built locally inside `gitClone` and `req.Repo` is
  never rewritten — so this is defence in depth. It is worth the twelve lines because
  a token baked into a pushed image cannot be retracted by deleting the tag.
- **scp-syntax (`git@github.com:o/r.git`) is left alone.** Its `git@` is an ssh user,
  not a credential, and stripping it would break the label as a clone target. A
  mutation that "tidied" this into stripping both forms is in the canary set.
- **A subprocess for the revision, rejecting an in-process `.git` parse.** One fork
  per build, against a build measured in minutes. Parsing `.git/HEAD` then
  `refs/heads/*` or `packed-refs` is more code and more failure modes for an
  immeasurable saving. This is the `syscalls-over-subprocesses` carve-out: the cost
  is not per-cycle, and git is already the tool in this path.

## Deferred

| # | Item | Reason |
|---|---|---|
| P1-D1 | `gitClone` streams the **token-bearing** URL into the build log's argv path; if git echoes the remote on failure, a vended installation token could reach the job log | Pre-existing, not introduced here, and out of this phase's scope. Worth its own change: the log is served over the fleet API. Token is short-lived, which bounds but does not remove it |
| P1-D2 | No test proves buildx accepts the argv | Only a real build can; it is the first thing the rollout checks |

## Next phase's first concrete step

Phase 2, in `agent-repos/docker-agent`: add `SourceRepo`, `SourceRef`,
`SourceRevision`, `BuiltFromSource` to `ComposeProject` (`dockerclient.go:67-86`),
all `omitempty`, populated from the running container's `Config.Labels`. The router
and discovery-api need no change — phase 0 verified the whole path after the agent
is verbatim pass-through.

🔴 `BuiltFromSource` must serialise as the **string** `"true"`, not a JSON boolean:
the form field is a string enum and `handlePopulateFields` assigns without coercion.

## Traps

- **Tag before you deploy, or you rebuild the old code.** `kubernetes/build-deploy.yml`
  checks out the repo's latest tag, not `main`. Current tag is `0.1.5`; this is `0.1.6`.
- **Two playbooks, and the first is not enough.** `build-deploy.yml` builds and pushes
  but patches no k8s deployment (build-agent has none); `build-agent/deploy.yml` is
  what actually rolls the container. Add `-e firewatch_lifecycle=oneshot` to both.
- **The old agent builds the new one.** Confirm the new binary is running before
  trusting anything about its behaviour — and confirm the **binary**, not the tag:
  `docker exec build-agent sh -c 'strings /usr/local/bin/build-agent | grep -c "com.controlcenter.build.context"'`
  with a deliberately wrong string as the negative control.
- **Do not background the playbook with `nohup … &`** — the wrapper returns
  immediately and reports success while ansible is still running.
- **Watch the STATE, not the log.** The rollout check is
  `docker inspect --format '{{json .Config.Labels}}'` on a freshly built image, and a
  second build of the same source to confirm the cache still hits. "No errors in the
  build log" is not evidence for either.

## What was not done

**The four independent review lenses were not run.** This session is configured not
to spawn subagents, so the merge gate was a self-review plus the mutation campaign
above. That is a materially weaker instrument and the gap is measured: on comparable
phases a self-review found 4–5 findings where four lenses found 28–50, and an
author's own mutation campaign scored 20/20 while an independent lens found 36
survivors on the same diff — because the author mutates the lines he wrote and the
lens mutates the properties he claimed.

So read the 20/20 above as *"the author's list is covered"*, not as *"this diff is
clean"*. If lenses can be run before this reaches a host, run them.
