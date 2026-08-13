# build-agent

A single-binary Go HTTP service that turns container image builds into trackable jobs.

You `POST` a build request describing a repo/ref (or an inline base64 tar context) plus a
build descriptor, and the agent does the rest: it clones the source, ensures a persistent
`docker buildx` builder, validates and busts corrupted registry build caches, builds each
service under fast/slow lane concurrency limits against the host's Docker socket, streams
the merged build log over a WebSocket, pushes multi-arch images to a registry, applies
`latest`/`stable`/timestamp tag rotation, prunes old tags to a retention limit, and reports
the whole job lifecycle back to a central controller over a durable, restart-surviving
event outbox. It runs one agent per build host; the controller sees a fleet of them.

---

## ⚠ Security — read this before deploying anything

**This service has no inbound authentication whatsoever.** There is no bearer check, no
API key, no mTLS termination, no allow-list — nothing. Every route on `:8080` is open to
anyone who can open a TCP connection to it.

That matters more here than it would for most services, because of what the agent is:

- **It requires a mounted Docker socket.** Anything that can reach `POST /v1/builds` can
  make the agent run an arbitrary `Dockerfile`, which means arbitrary code with the
  privileges of the Docker daemon. That is **root-equivalent on the host**, full stop.
  Container isolation does not help you here; the docker socket *is* the escape hatch.
- **`pre_build` in the descriptor is literally a list of shell commands**, executed via
  `sh -c` in the build workdir. It is arbitrary code execution by design, taken straight
  from the request body.
- **The inline context upload (`context_b64`) lets a caller write files onto the host**
  inside the job workdir. The extractor is hardened against traversal, symlink escape,
  special files and decompression bombs — but it is still attacker-supplied bytes landing
  on disk if the endpoint is exposed.
- **The WebSocket upgraders accept any `Origin`** (`CheckOrigin` returns `true`
  unconditionally), so any web page in any browser that can route to the agent can open
  `/ws/fleet` and `/ws/jobs/:id/logs` and read your build logs.
- **The health endpoints always return 200.** They confirm the process is alive, not that
  it is healthy or that its controller is reachable.

The agent is designed to run **on a trusted network, behind a reverse proxy that terminates
mTLS (or equivalent strong client authentication)**. The proxy is the security boundary;
the agent assumes it exists. Bind the container's port to a private interface, never to a
public one, and never to `0.0.0.0` on a host with a public IP.

**Anyone who exposes `:8080` directly is handing out host takeover.** If you need
authentication in the agent itself, add it — do not assume it is there.

The one credential the agent *does* hold is outbound-only: a per-host bearer token read
from `BEARER_TOKEN_FILE` and attached to every call it makes *to* the controller. It
authenticates the agent to the controller, and does nothing to protect the agent's own API.

---

## Architecture

Flat `package main`, one file per concern:

| File | Responsibility |
|---|---|
| `main.go` | Process entry: logging, config load, boot-gap report, app construction, orphan sweep, gin route table, `:8080` listener, graceful shutdown on SIGINT/SIGTERM. |
| `app.go` | Dependency wiring — pooled controller HTTP client, event buffer, build registry, fleet hub — plus the background workers (outbox drain, fleet broadcaster, reconcile loop). |
| `config.go` | Env-var config struct and loader. Missing required vars or an unreadable bearer token exit the process at startup. |
| `types.go` | The whole wire + domain model: `BuildRequest`, `Descriptor`, `ServiceSpec`, the `Build` aggregate with its mutex-guarded state transitions, progress rollup and log ring buffer. |
| `handlers.go` | Gin HTTP handlers: health, trigger, list/get/log/cancel, and the job-log WebSocket. |
| `jobs.go` | `buildRegistry` — creates job workdirs, starts builds in the background, tracks in-flight builds, cancels them, evicts old ones, and serves log subscriptions. |
| `engine.go` | The build itself: clone, builder bootstrap, `pre_build` hooks, cache validation, per-service `docker buildx` invocation under lane semaphores, buildx step parsing, tagging, retention, bounded GC. |
| `context.go` | Hardened extractor for the inline `context_b64` upload (traversal, symlink, special-file and decompression-bomb defences). |
| `registry.go` | Direct Registry v2 HTTP API client: build-cache validation and busting, tag listing, digest resolution, and timestamp-tag retention pruning. |
| `events.go` | The `events.sqlite` store: a local `builds` table for restart survival plus the kit's durable outbox that POSTs every lifecycle event to the controller. |
| `fleet.go` | Coalescing snapshot hub and the `/ws/fleet` handler that streams `{host, builds}` to dashboards. |
| `orphan_sweep.go` | Boot-time recovery: flips builds stranded `running`/`pending` by a prior crash to `failed` (exit 137) and enqueues synthetic terminal events. |
| `heartbeat.go` | Writes a timestamp to `<CONFIG_DIR>/heartbeat.last` every 30s and logs, at the next boot, how long the agent was down and whether it stopped cleanly. |
| `network.go` | The pooled outbound HTTP client (bearer round-tripper, optional proxy dial rewrite) and the GitHub installation-token vend with retry/backoff. |
| `logging_setup.go` | Optional rotating on-disk log file (lumberjack) tee'd alongside stderr when `LOG_DIR` is set. |

Shared operational scaffolding — the durable event outbox, the fleet hub, the job store and
the reconcile loop — lives in [`agent-kit-go`](https://github.com/Daniel-dev22/agent-kit-go)
so sibling agents share one implementation.

---

## HTTP API

Nine routes, all on `:8080`. **All unauthenticated** — see the security section.

### `GET /health/live`
Liveness. Always `200 {"status":"ok"}`. Touches no dependency.

### `GET /health/ready`
Readiness. Always `200 {"status":"ready"}`. It does **not** reflect controller
reachability, Docker socket availability or outbox backlog — it means the process is
serving.

### `POST /v1/builds`
Starts a build. The build runs detached from the request; the call returns immediately.

Request body (`BuildRequest`):

```json
{
  "descriptor": { "...": "see The build descriptor below" },
  "repo": "git@github.com:example-org/example-repo.git",
  "ref": "v1.4.2",
  "context_b64": "",
  "registry": "registry.example.com",
  "version": "",
  "services": ["api", "web"],
  "private": null,
  "trigger_key": "ci"
}
```

| Field | Required | Meaning |
|---|---|---|
| `descriptor` | yes | The target repo's `build.json`, verbatim. Must have `project` and a non-empty `services`. |
| `repo` | exactly one of `repo` / `context_b64` | Git URL, SSH (`git@github.com:owner/repo.git`) or HTTPS form. |
| `ref` | no | Tag or branch. Passed to `git clone --branch`; omitted ⇒ default branch. Cloning is always `--depth 1`, so a raw commit SHA will not work. |
| `context_b64` | exactly one of `repo` / `context_b64` | `base64(gzip(tar))` of the build context, for sources that are not a standalone cloneable repo. |
| `registry` | yes | Registry host. Overrides anything in the descriptor. Images are pushed to `<registry>/<image_prefix><service>`. |
| `version` | only when `descriptor.version.scheme == "external"` | The version tag to apply. |
| `services` | no | Subset of the descriptor's services to build. Empty ⇒ all, in descriptor order. |
| `private` | no | `true`/`false` forces the GitHub-token decision; omitted ⇒ inferred from `PRIVATE_REPO_OWNERS`. |
| `trigger_key` | no | Free-form provenance label carried through to the controller. Defaults to `"api"`. |

**`202 Accepted`** → `{"job_id":"<uuid>","state":"pending"}` (the state may already read
`running`; the engine starts concurrently with the response).

**`400 Bad Request`** with `{"error":"..."}` when the JSON does not bind, when
`descriptor.project` or `descriptor.services` is missing, when `registry` is missing, when
neither or both of `repo`/`context_b64` are supplied, or when the version scheme is
`external` and `version` is empty.

There is no 5xx path — a build that fails after acceptance is reported through the job
state, not the HTTP response.

### `GET /v1/jobs`
`200` with a JSON array of job objects: every in-memory build merged with the most recent
50 persisted builds (in-memory wins on id collision).

**The array is not globally sorted.** The in-memory half comes out in Go map order and is
prepended to the newest-first persisted half. Sort by `started_at` client-side.

### `GET /v1/jobs/:id`
`200` with one job object — from memory if the build is still tracked, otherwise from
`events.sqlite`. `404 {"error":"job not found"}` if neither has it.

Job object shape:

```json
{
  "id": "0f2c…", "project": "example", "ref": "v1.4.2",
  "version_tag": "20260813-184501", "registry": "registry.example.com",
  "state": "running",
  "services": [{
    "name": "api", "lane": "fast", "state": "building", "attempts": 1,
    "started_at": "2026-08-13T18:45:01Z", "ended_at": null, "duration_ms": 0,
    "image": "", "tag": "", "error": "",
    "current_step": 4, "total_steps": 9, "step_name": "RUN go build ./..."
  }],
  "progress": {
    "total_services": 2, "done_services": 0, "failed_services": 0,
    "current_service": "api", "percent": 0, "updated_at": "2026-08-13T18:45:12Z"
  },
  "started_at": "2026-08-13T18:45:01Z", "completed_at": "0001-01-01T00:00:00Z",
  "exit_code": 0, "error": "", "line_count": 812, "trigger_key": "ci"
}
```

`current_step`/`total_steps`/`step_name` are a **liveness indicator only**, parsed from
`buildx --progress=plain` `#N [stage n/m]` lines. The counter is per BuildKit stage, not
per build, and it is cleared when the service finishes. `step_name` is capped at 80 chars.

### `GET /v1/jobs/:id/log`
`200` → `{"id":"<uuid>","lines":["[git] Cloning into…", "…"]}` — the job's in-memory ring
buffer, capped at the last **2000** lines. Each line is prefixed with its producer
(`[git]`, `[builder]`, `[prebuild]`, `[<service-name>]`).

`404 {"error":"job not found or no live log"}` for any job not currently in memory —
including jobs that completed and were evicted, or that survive only in sqlite. **Build
logs are not persisted**; only the job record is.

### `POST /v1/jobs/:id/cancel`
Cancels the job's context, which kills the in-flight `git`/`docker buildx` child processes.

`200 {"cancelled":true}` on success. `409 {"error":"job not cancellable (unknown or already
terminal)"}` if the id is unknown or the job already reached a terminal state.

### `GET /ws/jobs/:id/logs`
WebSocket log stream. **Frames are raw newline-terminated text, not JSON** — each frame is
one log line plus `\n`. On connect the server replays the ring-buffer backlog, then streams
live lines. The server pings every 20s; the client should read continuously and needs to
send nothing.

If the job has no live log the server writes the plain text `job not found or no live log`
and closes. Because the upgrade already succeeded, this arrives as a WebSocket message, not
an HTTP status.

A slow consumer does not stall the build: the per-subscriber channel (256 deep) drops its
oldest line to make room, so a lagging viewer loses lines rather than blocking the engine.

### `GET /ws/fleet`
WebSocket snapshot feed. Frames are JSON:

```json
{
  "type": "snapshot",
  "data": {
    "host": { "name": "builder01", "site": "site-a", "num_cpu": 16 },
    "builds": [ /* same shape as GET /v1/jobs */ ]
  }
}
```

One snapshot is sent immediately on connect, then a fresh one on every lifecycle change
(including live buildx sub-step transitions). The hub is **purely event-driven** — there is
no periodic tick — and it coalesces: bursts collapse into one snapshot per build window,
and slow subscribers drop frames rather than stalling the broadcaster.

---

## The build descriptor

The `descriptor` field is the target repo's `build.json`, checked in beside the services it
describes. This repo's own `build.json` is a minimal example.

```json
{
  "project": "example",
  "image_prefix": "example-",
  "platform": { "mode": "auto" },
  "version": { "scheme": "timestamp" },
  "lanes": { "fast": 2, "slow": 1 },
  "tag_rotation": true,
  "retention_keep": 15,
  "build_args": { "APK_CACHE_BUST": "${APK_CACHE_BUST}", "SERVICE_DIR": "${SERVICE_DIR}" },
  "pre_build": ["make version"],
  "services": [
    { "name": "api",  "lane": "fast" },
    { "name": "web",  "lane": "slow", "dockerfile": "web/Dockerfile", "context": "." },
    { "name": "tool", "dockerfile": "docker/main/Dockerfile", "target": "runtime",
      "build_args": { "EXTRA": "v=${VERSION}" } }
  ]
}
```

| Field | Default | Meaning |
|---|---|---|
| `project` | — (required) | Project name, echoed onto every job. |
| `image_prefix` | `""` | Prepended to each service name to form the image repo: `<registry>/<image_prefix><service>`. |
| `platform.mode` | `"auto"` | `"auto"` ⇒ `--platform linux/amd64,linux/arm64`. `"fixed"` ⇒ use `platform.value` verbatim. |
| `platform.value` | `""` | e.g. `"linux/arm64"`. Only read when `mode` is `"fixed"`. |
| `version.scheme` | `"timestamp"` | `"timestamp"` ⇒ the tag is `YYYYMMDD-HHMMSS` stamped once at build start and shared by every service in the job. `"external"` ⇒ the request's `version` field is the tag (and becomes required). |
| `lanes.fast` | `BUILD_FAST_LANE` (2) | Max concurrent fast-lane service builds. Values ≤ 0 fall back to the env default. |
| `lanes.slow` | `BUILD_SLOW_LANE` (1) | Max concurrent slow-lane service builds. Same fallback. |
| `tag_rotation` | `true` | Before building, if `<image>:latest` exists it is copied to `<image>:stable`. Set `false` to disable. |
| `retention_keep` | `REGISTRY_RETENTION_KEEP` (15) | How many `YYYYMMDD-HHMMSS` tags to keep per image after each successful build. `latest`, `stable` and `buildcache` are always protected, as is any tag not matching the timestamp pattern. `0` or less disables pruning. |
| `build_args` | `{}` | Project-level build args, inherited by every service. |
| `pre_build` | `[]` | Shell commands run via `sh -c` in the freshly populated workdir **before any service builds**, each bounded by `GIT_TIMEOUT`. Output is streamed under the `[prebuild]` prefix. The first failure fails the whole job. **Arbitrary code — see the security section.** |
| `services[]` | — (required, non-empty) | The services to build. |

Per service:

| Field | Default | Meaning |
|---|---|---|
| `name` | — | Service name; also the image suffix and the log-line prefix. |
| `context` | `"."` | buildx context path, relative to the workdir. |
| `dockerfile` | `"<name>/Dockerfile"` | Dockerfile path, relative to the workdir. Missing ⇒ that service fails immediately. |
| `target` | `""` | buildx `--target` stage. Empty ⇒ final stage. |
| `lane` | `"fast"` | `"slow"` puts the service in the slow-lane semaphore; anything else is fast. |
| `build_args` | `{}` | Merged over the project-level `build_args` (service wins on conflict). |

### Build-arg templating

Values in `build_args` (project and service level alike) are substituted before being passed
as `--build-arg`:

| Token | Expands to |
|---|---|
| `${VERSION}` | The job's version tag — the `YYYYMMDD-HHMMSS` stamp, or the external version. |
| `${APK_CACHE_BUST}` | A coarse `YYYY-MM` bucket, so package-index layers are re-fetched about monthly instead of every build. |
| `${SERVICE_DIR}` | The directory of the service's Dockerfile (`filepath.Dir`), or `"."` if empty. Useful when one Dockerfile serves many services. |

### Assembled buildx invocation

```
docker buildx build --builder <BUILDX_BUILDER> --progress=plain \
  --platform <…> \
  --cache-from type=registry,ref=<image>:buildcache \
  --cache-to   type=registry,ref=<image>:buildcache,mode=max \
  [--build-arg K=V …] [--target <stage>] [--secret id=ghtoken,src=<file>] \
  -t <image>:latest -f <dockerfile> --push <context>
```

Then `<image>:latest` is copied to `<image>:<version-tag>` with
`docker buildx imagetools create`, and retention pruning runs.

> **Note:** `buildkitd_config` is accepted in the descriptor JSON but is **not used** — the
> builder's BuildKit config comes only from the `BUILDKITD_CONFIG` env var. Likewise `ssh`
> in this repo's own `build.json` is a hint for whatever external system builds *this*
> image; the agent does not read it.

---

## Job model

**Build states:** `pending` → `running` → one of `completed` / `failed` / `cancelled`.

**Per-service states:** `pending` → `building` → `done` / `failed`.

**Terminal rollup**, applied once every service goroutine has returned:

- Already `cancelled` ⇒ stays `cancelled`; nothing is recomputed.
- Any service `failed` ⇒ build `failed`, `exit_code 1`, error `"one or more services failed
  to build"` unless a more specific message was already set.
- Otherwise ⇒ `completed`, `exit_code 0`, and a bounded `docker buildx prune` runs
  (`--reserved-space 10GB --max-used-space 40GB --min-free-space 20GB --filter until=168h`).

Failures *before* the per-service fan-out — context extraction, GitHub token vend, token
file write, `git clone`, builder creation, `pre_build` — fail the whole job immediately with
`exit_code 1` and a specific error message. No services are attempted.

`progress.percent` is `(done + failed) / total × 100`, so it reaches 100% even when
services failed; read `failed_services` for the outcome.

**Lifecycle events** emitted to the controller: `started`, `progress` (per service state
transition), and one of `completed` / `failed` / `cancelled`.

### Logs

Each build keeps a **2000-line ring buffer** in memory, holding merged stdout+stderr from
every child process, line-prefixed by producer. `line_count` is the total ever appended, so
`line_count > 2000` means the head has rolled off. **Logs are never persisted** — they exist
only while the build is in memory.

### Retention

- **In memory:** at most **50** builds. When the map exceeds that, the oldest *terminal*
  builds are evicted by completion time. Running builds are never evicted.
- **On disk:** `<CONFIG_DIR>/events.sqlite` (WAL mode) holds a `builds` table upserted on
  every lifecycle event, plus the kit's `pending_events` outbox table. `GET /v1/jobs` reads
  back the 50 most recently updated rows. Nothing prunes the `builds` table — it grows
  without bound; add a janitor if that matters for your retention window.

### Boot orphan sweep

A build interrupted by a container exit would otherwise sit `running` forever, both locally
and in the controller. So **before the HTTP listener starts**, the agent flips every
`running`/`pending` row in the local `builds` table to `failed` with:

- `exit_code` **137** (SIGKILL, the usual cause),
- error `"agent restart orphan (boot sweep): build interrupted by container exit"`,

and enqueues a synthetic terminal event per swept job on the durable outbox, so the
controller converges as soon as it is reachable again.

Separately, `heartbeat.go` writes `<CONFIG_DIR>/heartbeat.last` every 30s and `0` on clean
shutdown, so the next boot can log whether the last stop was graceful and how long the gap
was.

---

## Configuration

All configuration is environment variables, read once at startup.

### Required — the process exits if these are missing

| Variable | Meaning |
|---|---|
| `NODE_NAME` | Short hostname identifying this agent. Reported on every event and in the fleet snapshot. |
| `SITE_ID` | Logical site/region this node belongs to (e.g. `site-a`). Reported alongside `NODE_NAME`. |
| `CONTROL_CENTER_URL` | Base URL of the controller, e.g. `https://controller.example.com:1443`. All outbound calls are built from it. |

The bearer token is **effectively required too**: `BEARER_TOKEN_FILE` has a default path,
but if that file is missing, unreadable or empty the agent logs an error and exits 1. To run
without a controller you must still place a non-empty file there.

### Optional

| Variable | Default | Meaning |
|---|---|---|
| `CONFIG_DIR` | `/var/lib/build-agent` | Persistent state: `events.sqlite`, `heartbeat.last`. Created at startup with mode 0700. |
| `WORK_DIR` | `/var/lib/build-agent/work` | Scratch root. Each job gets `<WORK_DIR>/<uuid>/`, with the build context in `.../src/`; the whole job directory is removed when the build ends. |
| `BEARER_TOKEN_FILE` | `/etc/build-agent/bearer-token` | File holding the per-host bearer token (trailing whitespace trimmed). Mount it 0600. |
| `TRAEFIK_DOCKER_DNS` | `""` | If set, every controller connection is dialled to this host instead of the URL's own host, while the Host header and TLS SNI keep the original name — the shape needed when a local reverse-proxy container attaches the client certificate. Empty ⇒ dial the URL host directly. |
| `TRAEFIK_DIAL_PORT` | `1443` | Port used with `TRAEFIK_DOCKER_DNS`. Ignored when that is empty. |
| `PRIVATE_REPO_OWNERS` | `""` | Comma-separated GitHub owners/orgs (e.g. `acme,example-org`) whose repos are treated as private when `private` is not set on the request, triggering an installation-token vend. Empty ⇒ **nothing** is inferred private. Matched as `"<owner>/"` against the repo URL. |
| `BUILDX_BUILDER` | `build-agent-builder` | Name of the persistent `docker-container` buildx builder. Reused across builds so BuildKit cache mounts survive. |
| `BUILDKITD_CONFIG` | `""` | Path to a `buildkitd.toml` passed as `--buildkitd-config` when the builder is created. Silently skipped if the path does not exist. |
| `CC_RECREATE_BUILDER` | unset | Set to `1` to force the builder to be destroyed and recreated on the next build — needed after editing `BUILDKITD_CONFIG`, since an existing builder is otherwise reused as-is. |
| `BUILD_FAST_LANE` | `2` | Default max concurrent fast-lane service builds. A descriptor's `lanes.fast` overrides it. |
| `BUILD_SLOW_LANE` | `1` | Default max concurrent slow-lane service builds. A descriptor's `lanes.slow` overrides it. |
| `REGISTRY_RETENTION_KEEP` | `15` | Default timestamp-tag retention. A descriptor's `retention_keep` overrides it. |
| `GIT_TIMEOUT` | `5m` | Per-`git clone` timeout, and also the per-command timeout for each `pre_build` step. Go duration string. |
| `BUILD_TIMEOUT` | `60m` | Hard ceiling on a whole build job, across all its services. Go duration string. |
| `LOG_LEVEL` | `info` | `debug` \| `info` \| `warn` \| `error`. Anything else is treated as `info`. |
| `LOG_DIR` | `""` | If set, logs are tee'd to a rotating `<LOG_DIR>/agent.log` (20 MB × 5 backups, 14 days, compressed) alongside stderr. Empty ⇒ stderr only. |
| `LOGUTIL_FORMAT` | `json` | Set to `text` for slog's text handler instead of JSON. |

Invalid integer or duration values log a warning and fall back to the default rather than
failing startup.

### Runtime requirements

The container needs the **Docker socket** (`/var/run/docker.sock`) bind-mounted, plus the
`docker` CLI, the `buildx` plugin and `git` on `PATH` — the provided image installs all
three. `CONFIG_DIR` should be a persistent volume; losing it loses job history and any
undelivered events.

---

## Controller contract

The agent is a satellite. It makes three kinds of outbound call, all to
`CONTROL_CENTER_URL`, all carrying `Authorization: Bearer <token>`:

| Call | When | Body | Expected response |
|---|---|---|---|
| `POST /api/build/github-token` | Before cloning a repo judged private | `{"repositories":["owner/repo"]}` | `{"token":"…","expires_at":"…"}` — a short-lived GitHub App installation token. Up to 4 attempts, backing off 500ms → 1s → 2s, retrying only network errors and `500`/`502`/`503`/`504`; `401`/`403` and every other status fail immediately. |
| `POST /api/build/jobs/{id}/event` | Every lifecycle transition | The full job payload (job id, site, node, project, ref, version tag, registry, state, event, per-service progress, timestamps, exit code, error, line count, trigger key, emitted-at) | Any 2xx. Non-2xx leaves the event queued. |
| `POST /api/build/reconcile` | On boot and every 5 minutes | `{"site","node","snapshot_at","jobs":[{id,state,exit_code}…]}` — every non-terminal job plus everything terminal in the last 24h | Any 2xx. The controller is expected to make its rows for this `(site,node)` match. |

Event delivery is **durable**: events are written to the `pending_events` table before the
push is attempted, an immediate best-effort POST follows, and a drain loop retries the
backlog oldest-first every 30s. A controller outage delays events; it does not lose them.
The reconcile loop is the level-triggered safety net for events that are lost or arrive out
of order — the failure it exists to prevent is a controller row stranded `running` forever.

**Be honest about the dependency: the agent is not useful without a controller that
implements these three endpoints.** You can start it against a stub — builds run, logs
stream, `/v1/jobs` works — but token vending fails (so private repos cannot be cloned) and
the outbox grows unboundedly until something accepts its events. There is no reference
controller in this repo.

---

## Build & run

With the Go toolchain — the normal path:

```bash
go build ./...
go build -o build-agent .
```

```bash
NODE_NAME=builder01 \
SITE_ID=site-a \
CONTROL_CENTER_URL=https://controller.example.com:1443 \
BEARER_TOKEN_FILE=./bearer-token \
CONFIG_DIR=./state WORK_DIR=./state/work \
./build-agent
```

### Docker image — one honest caveat

`docker build` on this repo **currently requires access to the private module
`github.com/Daniel-dev22/agent-kit-go`.** The Dockerfile sets `GOPRIVATE`, rewrites the
GitHub URL to SSH and runs `go mod download` under `--mount=type=ssh`, so it needs a
forwarded SSH agent holding a key with access to that repo:

```bash
docker buildx build --ssh default -t build-agent:dev .
```

An outside contributor without that access should **build with the Go toolchain instead** —
`go build ./...` needs nothing special once the module is in the local module cache. When
`agent-kit-go` is published, the `GOPRIVATE` / url-rewrite / `--mount=type=ssh` lines can
simply be deleted and the image will build anywhere.

No SSH key ever lands in an image layer, and this is unrelated to how the agent authenticates
at runtime — runtime builds of *other* private repos use the controller-vended GitHub App
token passed to buildx as `--secret id=ghtoken`, never SSH.

---

## Testing

```bash
go test ./...
```

Covered today:

**`engine_test.go`** — build-arg templating (`${VERSION}`, `${APK_CACHE_BUST}`,
`${SERVICE_DIR}`); service selection (empty subset ⇒ all, subsets preserve descriptor
order); platform args (auto ⇒ multi-arch, fixed wins); buildx argument assembly and ordering
(`--target` present when set, absent when not, always before the trailing `-t`/`-f`/`--push`
tail); version tags (external scheme uses the request version, timestamp scheme matches
`YYYYMMDD-HHMMSS`); repo-slug extraction from SSH and HTTPS URLs and the token-bearing clone
URL; buildx `--progress=plain` step parsing, including the non-matching cases (`DONE`,
`CACHED`, `[internal]`, output lines) and the 80-char step-name bound; and private-repo
detection across explicit `private`, configured owners, an unlisted owner, an empty owner
list and a substring near-miss.

**`context_test.go`** — inline context extraction: the happy path with nested directories,
plus rejection of path traversal (`../`, `../../etc/passwd`, absolute paths, verified to
write nothing outside the destination), symlink entries, empty archives and malformed
base64.

**Not covered.** `handlers.go`, `jobs.go`, `events.go`, `registry.go`, `fleet.go`,
`orphan_sweep.go`, `network.go` and `config.go` have **no tests at all**. That means the HTTP
surface, WebSocket behaviour, job lifecycle and eviction, sqlite persistence, registry v2
interactions, fleet snapshots, the orphan sweep, the token-vend retry ladder and env parsing
are all unverified by the suite. The tested parts are the pure functions; the I/O is not.

---

## Related projects

- **[agent-kit-go](https://github.com/Daniel-dev22/agent-kit-go)** — the shared Go module
  this is built on: durable event outbox, fleet snapshot hub, job store, reconcile loop and
  logging setup.

Sibling agents built on the same kit:

- **[docker-agent](https://github.com/Daniel-dev22/docker-agent)**
- **[duplicacy-agent-api](https://github.com/Daniel-dev22/duplicacy-agent-api)**
- **[gdrive-agent](https://github.com/Daniel-dev22/gdrive-agent)**
- **[filemesh-agent](https://github.com/Daniel-dev22/filemesh-agent)**

---

## License

[MIT](LICENSE) © 2026 Daniel-dev22
