# AGENTS.md — Copilot implementation contract

Before editing code, read `README.md`, `docs/design.md`, `docs/protocol.md`, `docs/implementation-plan.md` and the current API contract in [`Bpoe/dsc-registry`](https://github.com/Bpoe/dsc-registry/blob/main/docs/api.md). Consult [`Bpoe/dsc-reconciler`](https://github.com/Bpoe/dsc-reconciler) for actual result envelope, writer paths, and OS-specific lock behavior. If upstream behavior contradicts this design, report it instead of silently inventing a protocol.

## Project boundaries

- Repository `dsc-registry-sync`; binary/service `dscregsyncd`. `dscregd` is Registry; `dscd` is the independent local DSC reconciler.
- Go executable, one module, pragmatic small packages. Standard library first; use external packages only where necessary (for example, filesystem event watching or Windows system calls). No generic DI framework, Clean/Onion layers, ORM, message broker or shared library invented prematurely.
- Do **not** run DSC from this process. No dependency on `dscd` Go internals: integrate via stable files and HTTP.
- The machine key scopes all API calls. Never provide machine UUID/name as a caller-controlled authorization identity.
- Configuration files are `config.d/dscregsync/{name}.json` with optional `{name}.parameters.json`. Per-unit local revision is `.NAME.revision`. Machine collection cache validator is `config.d/dscregsync/machine.etag`.
- Use current `POST /v1/agent/reports` contract; Registry stores historical revisions, even after config deletion. The older named report endpoint exists for compatibility but should not drive new design.
- Do not compare `dscd`'s `inputHash` to Registry content revisions; no persistent hashes, transaction files, backups, upload queues or state databases in v1.
- The only owner of `config.d/dscregsync/` should be `dscregsyncd`; never edit other configurations under `config.d/`.

## Critical correctness

- Validate all server-controlled names as single safe lowercase configuration stems before forming paths. Never follow untrusted symlinks or redirect writes outside managed directory.
- For existing configurations, respect dscd's per-config protection before deleting the old file; Linux cooperatively acquire an exclusive advisory lock and verify opened-file identity, Windows retry sharing violations. Do not add lock files.
- Stage and safely publish parameters and `.NAME.revision` before atomically publishing a new configuration last. Remove the old result before that publication; serialize update and upload snapshot work per unit within this process. An incomplete update can be retried; no rollback system.
- Only persist the new `machine.etag` after a complete successful 200 collection application. A 304 is not an instruction to delete anything. Errors and transient unavailability must never wipe cached desired state.
- Handle result watcher events via coalescing and read complete atomically published result files. Take startup/recovery scans to cover events that happened before the watcher was established; no periodic results polling loop is required.
- Generate stable upload IDs from exact result content + associated configuration revision + unit name, and retry without changing the `result` JSON bytes. Distinct execution results should yield distinct IDs. Do not fabricate a hash-to-revision mapping.
- Treat report files, parameters and credentials as potentially secret. No values, request bodies, tokens or full results in routine logs.

## Workflow and quality

- Work on **one stage / one PR at a time**. Keep docs and tests updated in each PR. Do not implement deferred features.
- Unit tests plus mock HTTP-server tests; integration tests against running `dscregd` and `dscd` where appropriate. Linux and Windows runtime tests are both required before declaring v1 ready. A cross-build is not equivalent to Windows runtime validation.
- `gofmt`, `go test ./...`, `go vet ./...`, `go test -race ./...` where supported, Linux build and Windows cross-build. CI should check formatting, tests, vet, and builds once Go code exists.
- Give a concrete summary of changes, validation performed, and unresolved upstream dependencies.
