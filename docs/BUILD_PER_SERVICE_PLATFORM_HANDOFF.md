# build-agent: per-service platform (handoff)

**Status:** branch `feat/per-service-platform`; merged/tagged/deployed state is in the orchestrator ledger, not here.

**What shipped.** `services[].platform` (`{mode, value}`, same type as the project field) overrides the project platform for one service; nil inherits. `platformArgs(d, svc)` is the only resolver and `buildxArgs` its only caller. Consumer: `price-monitor-browser` (Chrome is amd64-only) via its `build.json`.

**New validation (trigger, 400).** `Descriptor.validate()`: mode in `""|auto|fixed` (project) or `auto|fixed` (service, an empty mode is ambiguous there), a `fixed` value must match `linux/<arch>[/vN][,...]`. Before this, a typo or a `fixed` with no value silently built multi-arch.

**Measured gate.** Validation run against every `build.json` under `/docker_container_volumes`: **1,101 files (15 distinct contents), all pass**. Platforms in use: `auto` 562, `fixed linux/arm64` 509, `fixed linux/amd64` 30. Re-run with `BUILD_JSON_LIST=<file of paths> go test -run TestEveryDeployedBuildJSONStillValidates -v`.

**Verified by.** Negative controls, each red: precedence removed, fixed-value check off, mode typo accepted, service mode not required, services not validated, handler not gated.

**Not covered / traps.** `build_orchestrator.py` (the non-agent path) ignores per-service platform and stays multi-arch. The ansible trigger passes `build.json` verbatim, so the field reaches the agent; `control-api/frigate_build.go` builds a typed descriptor of its own and would DROP the field (irrelevant for Frigate; a trap if the browser sidecar is ever triggered from there).
