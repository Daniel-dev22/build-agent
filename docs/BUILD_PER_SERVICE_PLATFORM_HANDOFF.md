# build-agent: per-service platform (handoff)

**Status:** branch `feat/per-service-platform`; merged (`a1d0418`), tagged `0.1.8`, and deployed to both sites per the user (2026-10-31; this session ran no deploy). Live check from one site's router: a trigger with `platform.value` `windows/amd64` answers 400 `descriptor.service x: platform.value ... is not a linux/<arch> list`, so the validating agent is live there. The other site was not probed.

**What shipped.** `services[].platform` (`{mode, value}`, same type as the project field) overrides the project platform for one service; nil inherits. `platformArgs(d, svc)` is the only resolver and `buildxArgs` its only caller. Consumer: `price-monitor-browser` (Chrome is amd64-only) via its `build.json` is **pending**: 0 of 1,101 `build.json` files carry `services[].platform` yet.

**New validation (trigger, 400).** `Descriptor.validate()`: mode in `""|auto|fixed` (project) or `auto|fixed` (service, an empty mode is ambiguous there), a `fixed` value must match `linux/<arch>[/vN][,...]`. Before this, a typo or a `fixed` with no value silently built multi-arch.

**Measured gate.** Validation run against every `build.json` under `/docker_container_volumes`: **1,101 files (15 distinct contents), all pass**. Platforms in use: `auto` 562, `fixed linux/arm64` 509, `fixed linux/amd64` 30. Re-run with `BUILD_JSON_LIST=<file of paths> go test -run TestEveryDeployedBuildJSONStillValidates -v`.

**Verified by.** Negative controls, each red: precedence removed, fixed-value check off, mode typo accepted, service mode not required, services not validated, handler not gated.

**Not covered / traps.** `build_orchestrator.py` (the non-agent path) ignores per-service platform and stays multi-arch. The ansible trigger passes `build.json` verbatim, so the field reaches the agent; `control-api/frigate_build.go` builds a typed descriptor of its own and would DROP the field (irrelevant for Frigate; a trap if the browser sidecar is ever triggered from there).

**Deploy-ordering gate (review finding, MED).** An agent without this change silently drops `services[].platform` (the trigger binds JSON with unknown fields allowed) and builds multi-arch. For the amd64-only browser image that is not a loud failure. So: **no build.json may carry `services[].platform` until BOTH sites run an agent at or above the release tag**, and a rollback of the agent below it must be treated as a rollback of that build.json line too.

**Deferred (named).** (a) `/home/daniel/ansible/kubernetes/build/trigger-agent-build.yml` uses `wget -q` with `until "job_id" in stdout`, so any 400 (this change adds a class of them) is retried ~36 s and its message is lost; fix belongs in the ansible repo (treat 4xx as final and print the body). (b) The all-`build.json` gate is manual (`BUILD_JSON_LIST`), since the files carry real hostnames and are not copied into testdata. (c) Validation is shape-only: `linux/foo` passes and fails later in buildx. Review: `/var/tmp/scratch/pricemon3-w1b-lens.md`.
