# Staged Copilot implementation plan

Do **not** implement everything in one PR. A narrow PR sequence keeps platform safety and API compatibility reviewable.

## PR 1 — daemon baseline and conditional desired-state retrieval

- Establish `cmd/dscregsyncd`, config loader from JSON, defaults/validation, structured logs, cancellation and foreground CLI.
- Implement machine API-key HTTP client against `GET /v1/agent/configurations` with conditional ETag, 200/304, cache policy/jitter, timeouts and retry/backoff/Retry-After.
- Read `machine.etag` from `config.d/dscregsync/` but do not write it until publication is implemented in PR 2. Model the decoded server collection and validate names/shape/size defensively.
- Add typed API client tests using `httptest`, malformed responses, cache headers, auth/privacy validation, and config parser tests.
- Add initial Go CI (Go fmt/vet/test/build plus Windows cross-build), keeping unnecessary abstractions out.

**Done when:** a foreground process reads config, authenticates, requests a collection and handles 200/304 and retries correctly, but it does not yet modify configurations.

## PR 2 — local managed publisher and revision state

**Implemented in PR 2:** foreground Registry-to-filesystem application,
private staging/permissions, cooperative platform protection, revision state
and last-step ETag commits. Runtime tests reproduce the upstream protection
contract in separate processes and exercise abrupt exit/restart boundaries.
Real deployed-service integration remains PR 4; PR 2 did not include a watcher/uploader.

- Implement `config.d/dscregsync/{name}.json`, optional `.parameters.json`, `.NAME.revision`, and committed `machine.etag` behavior as in design.
- Validate full 200 response before modifying files; install only changed/missing units, delete absent managed units only after complete response.
- Implement safe same-directory staging/renames, file permissions, path validation, stale result removal, Linux cooperative locking and Windows sharing-violation retry.
- Serialize per-unit operations; add crash-injection tests showing partial installs are retried with uncommitted collection validator. No backup/journal/transaction system.
- Create Linux/Windows filesystem tests and cross-build. Track real `dscd`-locking dependency explicitly.

**Done when:** a real or mock Registry 200 publishes/updates/deletes only the owned subtree, and ETag commits only after all updates succeed.

## PR 3 — result watcher and history API uploader

**Implemented in this stage:** native filesystem notifications with startup and
recovery scans, confined per-unit snapshots, immutable history-API requests,
documented deterministic UUIDv5 identity, latest-only retries and independent
poll/upload cancellation. Tests cover exact bytes and envelope variants,
restart deduplication, HTTP failure policy, missing/replaced directories,
watcher failures, stable snapshots and publication/polling during blocked uploads.
No service packaging, persistent upload state or real DSC execution is included.

- Watch `results.d/dscregsync/` for complete result publications, coalescing notifications; startup and watcher-recovery one-time scans.
- Snapshot result bytes and corresponding `.NAME.revision` under the service's per-unit synchronization; never associate old result with new revision.
- Implement `POST /v1/agent/reports` payload, deterministic UUID upload identity, repeatable byte-identical retries, latest-only behavior and request timeouts/backoff. Use 201/200 as success; distinguish 409 upload conflict from transient errors.
- Exercise a real `dscd` envelope including optional/null fields and no `inputHash`. Add mock server tests for 201/200, historical accepted reports, 400/401/409/429/503, watcher event coalescing, restart/dedupe and offline service operation.

**Done when:** a new complete result is uploaded with the correct name/revision exactly once in server history after retries; no report queue or persistent hash ledger.

## PR 4 — production packaging, cross-service integration and documentation

**Implemented in this stage:** native SCM lifecycle with Event Log output and
bounded shutdown, systemd hardening, non-destructive DEB/RPM/MSI sources,
disposable-host repair/upgrade/uninstall checks, release gating, operations
documentation and an opt-in real Registry/reconciler/DSC roundtrip with a
controlled resource barrier proving protection interoperability.
Windows unit/vet and real peer runtime checks passed locally. Native Linux and
Windows checks, real-peer roundtrips and package lifecycle jobs now pass in CI.
Target-deployment and real machine reboot acceptance remain outstanding. See [deployment.md](deployment.md)
for exact evidence, commands and release gates; do not declare v1 ready yet.

- Add Linux `.deb`/`.rpm` with systemd unit and appropriate permissions, native Windows service/MSI following `dsc-reconciler` conventions. Service installation must not overwrite existing machine keys or managed input data.
- Integration smoke tests against real `dscregd` and lock-enabled `dscd`: initial 200, 304, changed parameters, delete, result generation, historical upload and reboot/restart.
- Verify upstream Registry path-validation fix and cross-platform runtime locking tests; report missing prerequisites instead of claiming success.
- Harden protected filesystem permissions, log redaction, graceful shutdown, restart and watcher recovery; document provisioning, key rotation, TLS, diagnostics and uninstall behavior.
- Verify on Linux and Windows, and update CI/release workflows.

**Done when:** both services run natively, lock interop has OS-level test evidence, and a complete configuration/report roundtrip passes with real Registry.

## Deferred — do not opportunistically add

Autoregistration, machine-name discovery, history backlog/queue, full snapshots database, outbox, credential/token exchange, certificate auto-enrollment, UI, hot config reload, telemetry exporters, peer-to-peer coordination, Kubernetes daemonset, report compression, cloud-specific hosting, automatic `dscd` installation, and file content integrity hashes.
