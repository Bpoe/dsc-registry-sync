# `dscregsyncd` — v1 architecture and behavior

**Status:** PRs 1-3 implement foreground synchronization; stage 4 adds native
service/packaging sources and real peer integration coverage. Native checks,
real-peer roundtrips and package lifecycle jobs pass in Linux/Windows CI.
Target-deployment and reboot acceptance remain readiness gates.
See [deployment.md](deployment.md). **Date:** 2026-10-19.

`dscregsyncd` is an independent, small Go machine-side synchronization service for DSC Registry. It is neither an execution engine nor a replacement for [`dscd`](https://github.com/Bpoe/dsc-reconciler). It runs on Linux and Windows and never invokes the DSC executable or imports Registry's server internals.

## 1. Responsibilities and non-goals

```text
               GET desired state (conditional ETag)
               POST execution reports (API key)
  dscregd <----------------------------------------> dscregsyncd
                                                       |
                                         config.d/dscregsync/*.json
                                         results.d/dscregsync/*.result.json
                                                       |
                                                      dscd
                                           reads / reconciles / publishes
```

The sync service polls assigned configurations, installs them as local JSON files, observes latest result files produced by `dscd`, and uploads results with correct Registry revision association. Its separate one-shot `register` command enrolls a new machine with a time-limited Registry token and provisions protected local service configuration/key files. Registry administrators create registration tokens; the server issues the machine key.

Not in v1: remote commands, installing DSC itself, administering `dscd`, cloud discovery, templates/assignment compilation, central policy logic, registration token creation, automatic re-registration/credential rotation, server push/websockets, exactly-once execution, queueing every result while offline, compliance evaluation, UI, automatic rollback, and shared Go modules with the other projects.

## 2. Runtime model

- One process/service per managed machine. One writer/owner of the managed directory; no multi-agent leader election.
- Go service, standard `net/http` client and `log/slog`. Small packages by concrete concern, for example `cmd/dscregsyncd`, `internal/config`, `internal/registry`, `internal/sync`, `internal/results`, and platform helpers only as needed. Do not create empty packages.
- Foreground mode for testing and a native systemd/Windows SCM service for deployment.
- Two independent loops: desired-state HTTP polling and local results watching/upload. Neither should block the other during outages; synchronize operations on one unit *within* the process so a report snapshot cannot be mislabeled during a local update.
- Server is authoritative for desired configuration collections. `dscd` is authoritative for completed local results. **Neither** service tries to infer DSC compliance from raw resources.

## 3. Directory and file contract

| File | Linux default | Windows default |
| --- | --- | --- |
| Service config | `/etc/dsc/dscregsyncd.json` | `%ProgramData%\dsc\dscregsyncd.json` |
| Machine key | `/etc/dsc/dscregsyncd.key` | `%ProgramData%\dsc\dscregsyncd.key` |
| Template | `/etc/dsc/config.d/dscregsync/docker.json` | `%ProgramData%\dsc\config.d\dscregsync\docker.json` |
| Parameters | `.../dscregsync/docker.parameters.json` | `...\dscregsync\docker.parameters.json` |
| Per-unit revision | `.../dscregsync/.docker.revision` | `...\dscregsync\.docker.revision` |
| Machine HTTP ETag | `/etc/dsc/config.d/dscregsync/machine.etag` | `%ProgramData%\dsc\config.d\dscregsync\machine.etag` |
| `dscd` result | `/var/lib/dsc/results.d/dscregsync/docker.json.result.json` | `%ProgramData%\dsc\results.d\dscregsync\docker.json.result.json` |

`configDir` and `resultsDir` identify `dscd` root directories; the service always derives the private `dscregsync` subdirectories beneath them. The Registry exposes one flat list of configuration names, not arbitrary subdirectories. Each name is a lowercase safe stem validated before forming file paths.

- `template` is an embedded JSON *object* and becomes `{name}.json`. It is not a JSON string, YAML payload or a `{format,content}` wrapper. Serialize deterministically and do not interpret DSC resources.
- `parameters`: absent or `null` means **no** sidecar; `{}` means a present, empty JSON sidecar. File contents are a direct JSON object with no wrapper.
- `.NAME.revision` is the server's opaque content-revision UUID corresponding to the published local unit. Do not confuse this with `dscd`'s `inputHash` or the collection revision number.
- `machine.etag` is the raw HTTP validator string, including its quotes and any weak prefix. It is not a filename-derived hash and not the integer collection revision. Store/read it as one bounded file, trimming only the terminating newline if one is written.
- Dot-prefixed metadata is ignored by `dscd` discovery. The `.etag` extension is not a DSC template and is likewise ignored. Only this service owns contents of its managed input subdirectory. No modification of locally owned sibling directories.

No local state DB, extra lock file, saved `inputHash`, template/parameters hashes, transaction marker, backup copies, permanent last-upload ledger, or persistent upload queue in v1. Hidden temporary staging files during an update are permitted and cleaned up opportunistically.

## 4. Conditional desired-state sync

`GET /v1/agent/configurations` authenticates via machine key. On every poll, use `If-None-Match` equal to the locally committed `machine.etag` when present. **Normal startup uses this conditional request; it does not force an unconditional GET.** No machine ID is passed to the endpoint. Respect `Cache-Control: private, max-age=N`, jitter and reasonable bounds/fallback (initial fallback 300 seconds); use exponential backoff + `Retry-After` on failure.

- **304:** no desired collection change. Do not alter local files or the committed machine validator. Result watching continues.
- **200:** decode and validate the full collection before applying anything: unique safe names, opaque valid revision UUIDs, object-shaped templates, optional object/null parameters, size limits and ETag. Do not interpret resource syntax or execute DSC. Determine additions/updates from `.NAME.revision` and expected files; only delete local managed names absent from the authoritative collection.
- For **unchanged** revision with existing expected files, skip republishing. The presence/absence of the sidecar is known from the 200 response; verify accordingly. No content hashes are persisted or required. Because the managed directory has one trusted owner, out-of-band edits with intact files are outside v1 integrity detection.
- For **changed or missing** units, stage the new pair in dot-prefixed temporary names in the same directory. Publish using the protocol in section 5.
- After *all* additions, changes and removals in that 200 response succeed, atomically commit `machine.etag` **last**. On any partial failure, retain the old ETag so the next poll will normally get 200 for the changed collection and retry. If a crash occurs while repairing an already-current collection, a subsequent 304 alone cannot discover arbitrary local loss; v1 deliberately does not promise repair of out-of-band deletion without an invalidated cache or later desired-state change.
- Do **not** interpret an HTTP error, invalid/partial response, authentication failure, watcher error, or 304 as an empty desired-state collection. A valid 200 with `configurations: []` is an authoritative empty collection and removes only agent-owned units.

The Registry's monotonically increasing `configurationRevision` integer appears in the response, but no separate persisted copy is needed: ETag carries the collection validator. Individual `revision` UUIDs are persisted in per-unit files.

**Publication boundary:** the publisher applies complete validated collections and
commits the ETag before the poller promotes it in memory. HTTP failures and 304s
never invoke the publisher. Result watching/upload runs independently; service
packaging is described in [deployment.md](deployment.md). See [protocol.md](protocol.md#client-validation-and-poll-policy) for
concrete validation limits and retry bounds.

## 5. Update/delete and `dscd` coordination

The configuration file itself is the synchronization object. `dscd` holds protection for a unit from input capture through the end of result publication, including failure paths. This is an upstream dependency; it is **not implemented inside `dscd` by this repository**.

- **Linux:** `dscd` holds an exclusive advisory `flock` on the configuration. Before deleting an existing unit, `dscregsyncd` opens its configuration and obtains a nonblocking exclusive `flock` on the same file; on contention, back off and retry. Verify opened-file identity still matches the pathname after acquiring the lock. Advisory locks **do not prevent unlink**; both parties must cooperate. Do not merely check lock state then delete. Do not add separate lock files.
- **Windows:** `dscd` opens the config for reconciliation without `FILE_SHARE_DELETE`, retaining its handle through result publication. `dscregsyncd` deletes the old config using ordinary OS operations and retries on sharing violations. Do not introduce a client-side byte-range lock protocol. Protect from writes via appropriate dscd sharing mode.
- Configuration deletion is **first** in an update, while it remains protected. With the old template removed, no new reconciliation of that pathname should begin. Then remove the old sidecar and old result (and other owned stale sidecars), install the new sidecar if present, atomically write `.NAME.revision`, and atomically rename the new config into place **last**. Keep critical per-unit operations serialized with result upload snapshot reads within `dscregsyncd`.
- On authoritative removal, remove the config first using the same safety rules, then its sidecar, result, and `.NAME.revision`; do not undo previously applied machine state.
- On Linux, a lock on a just-unlinked inode does not protect the new pathname: publication order, `dscd`'s post-lock file-identity check and the protected-result contract are the guarantees. Never rely on old-inode locking to serialize reads of a replacement file.
- Missing files or partially applied updates are acceptable; the next full collection response can republish them. No rollback, journal or transaction-marker protocol. Do not claim strict two-file crash atomicity. Stage in same filesystem; publish each individual file by same-directory atomic rename.
- Do not perform unsafe path traversal, operate through symlinks, or alter directories outside `dscregsync/`.

The implementation confines operations with `os.Root`, rejects nonregular
managed entries, and verifies managed-directory identity. Administrator-supplied
roots/ancestors must be trusted and writable by only the intended owners.
Unexpected entries in the exclusively owned input directory fail application;
only recognized unit artifacts and agent staging files are removed. Orphan
parameters, revision files and results participate in authoritative removal.
The result directory is reopened after protected configuration deletion because
an in-flight `dscd` may have created it while the publisher waited for protection.

New managed directories/files have private permissions (Linux 0700/0600;
Windows protected ACLs for the service identity, SYSTEM and Administrators).
Windows applies ACLs at handle-relative creation, not through a later path-based
permission update. Existing roots and directories retain their permissions.
Run both processes with compatible identities. Staged files are flushed and
closed before rename. Linux flushes changed result directories and the input
directory before the ETag commit point. Windows has no portable directory
fsync; neither platform promises a multi-file power-loss transaction.
`Publisher.SnapshotResult` uses `Publisher.WithUnit`, the same synchronization
entry point as publication, and the same confined directory/file helpers.
It verifies file identity, length and modification time around the bounded read.
Network requests run only after releasing the gate.

**Invariant:** A result of the previous installed unit must not be uploaded under the new content revision. `dscd` must finish result publication before unlocking, the client removes old results before publishing the new configuration, and local update/read handlers must be serialized by unit.

## 6. Results: filesystem watcher and upload

- Use a real filesystem watcher on `results.d/dscregsync/` (not an endless results-polling timer). Handle create/rename/write notifications and coalesce duplicate events. `dscd` publishes complete `NAME.json.result.json` via temporary-file replacement, so inspect the destination, not temporary filenames. Watcher setup should handle the managed result directory not existing yet.
- On startup and after a watcher failure/recreation, make **one reconciliation scan** of the current result directory so events during downtime/startup aren't silently missed. This does not make result polling the primary mechanism.
- Validate unit name/path and read the stable result JSON bytes. Under the sync service's per-unit serialization, read `.NAME.revision` and snapshot the raw result together; never infer a Registry revision from the envelope's `inputHash`. Preserve the nested result bytes exactly through retries. The original result `configuration` field is informational and typically `dscregsync/NAME.json`.
- Upload through `POST /v1/agent/reports`, passing `configurationName`, `configurationRevision`, `uploadId`, `result`. The new API accepts historical/removed revisions, so a snapshot of a completed old result already in flight may be uploaded with its **old** revision; never relabel it as new. The Registry decides what counts as current-revision status.
- Generate a stable canonical UUID upload ID deterministically from unambiguous `(configurationName, configurationRevision, SHA-256(exact result JSON bytes))` using a documented fixed UUID namespace/derivation. This is the same across process restarts and HTTP retries; different result payloads create different IDs. The server scopes deduplication to authenticated machine + name + upload ID.
- This is **latest-result best effort**, not delivery of all executions. When multiple watcher notifications or offline reports coalesce, prefer the latest atomically visible result. Do not add an outbox or report history queue. On every startup scan, a previously uploaded latest result may be submitted again; Registry deduplicates it (`200` same receipt, `201` first receipt).
- If a result is replaced while being read, verify a stable snapshot or retry. Never upload partially written JSON, or upload a result that can only be associated with an unknown/missing revision. Input failures may legitimately omit `inputHash`; preserve that fact without trying to calculate a value.
- Network failures/429/503/5xx: back off/retry if the snapshot remains relevant, honoring `Retry-After`. Unauthorized key: surface error and back off without deleting input. A stable invalid-report `400` is not cured by blind fast retry; log minimal metadata. `409` now means upload-ID conflict, **not** stale desired revision; do not blindly generate a new ID for an unchanged payload. The report-history API accepts old revisions.
- Keep result upload separate from polling so Registry outage doesn't block local `dscd`. No periodic heartbeat traffic, historical backlog or exactly-once guarantee in v1.

PR 3 uses `fsnotify` subscriptions on the managed result directory and its existing
ancestors, attaching parents before checking children. Directory creation/moves,
watcher overflow/errors and closed channels cause reattachment and a scan;
failed setup retries with cancellable 1-30 second exponential delay. Notifications
coalesce by safe unit name for 100 ms. One upload worker retains only per-name
latest IDs and retry deadlines, not historical payloads. A slow upload can delay
other uploads, but never holds a publication gate or blocks desired-state polling.
On retry it snapshots the then-current destination; if it disappeared, pending
work is dropped. A completed old request may still succeed historically.
Invalid local JSON/missing association waits for another event or recovery scan.
See [protocol.md](protocol.md#upload-identity-and-exact-bytes) for the fixed UUID
derivation, bounds and permanent/retryable status policy.

## 7. Service config and security

`dscregsyncd register <machine-name> --server <origin>` is a one-shot foreground
operation, not a daemon/service mode. It reads a token without terminal echo
(`golang.org/x/term`) or reads one line with `--token-stdin`. No credential is
accepted as an argument. Existing URL policy applies; HTTP loopback requires
explicit `--allow-insecure-loopback`. The Registry registration API uses Bearer
authentication; subsequent daemon calls continue to use the machine API key.

Before HTTP, registration verifies privilege and all credential-path ancestors,
refuses existing destinations and preflights private staging/no-replace publication.
Linux requires root-owned protected directories and creates files mode 0600.
Windows requires elevation, rejects reparse points and creates files/directories
with SYSTEM/Administrators-only protected ACLs. It stages and flushes both files,
publishes the key without replacement, then the complete JSON config last.
Failure cleanup checks file identity and only removes this attempt's files.
No persistent ID, rollback journal or credential queue is introduced.
`--config` places the JSON and its sibling `dscregsyncd.key` at an alternate
protected destination; other platform defaults are unchanged.

There is exactly one registration HTTP attempt, no redirects and a bounded,
validated response. If the remote outcome is uncertain or local persistence
fails after registration, explicit administrator recovery is required rather
than re-registration. Service installation/startup remains a packaging/operator
task. See [deployment.md](deployment.md) for recovery and permissions.

Prefer JSON for the service config to avoid adding YAML purely for service configuration. See `examples/dscregsyncd.json`. Initial fields:

```json
{
  "registryUrl": "https://registry.example.internal",
  "apiKeyFile": "/etc/dsc/dscregsyncd.key",
  "configDir": "/etc/dsc/config.d",
  "resultsDir": "/var/lib/dsc/results.d",
  "requestTimeoutSeconds": 30,
  "pollFallbackSeconds": 300
}
```

The API key is stored **outside** this JSON file and never logged. Linux: root-owned restrictive key file (0600) and protected parent directory. Windows: ACL-restricted to service identity/SYSTEM/Administrators; a stronger DPAPI-backed provider can be revisited if needed, but is not a prerequisite for v1. HTTPS is required except explicitly configured loopback development. Use OS trust roots and normal certificate validation; no `insecureSkipVerify` option. Fail clearly on invalid URL/path/configuration, malicious names, permission failures or inability to establish exclusive ownership of the managed tree.

The loopback development opt-in is `allowInsecureLoopback` (default `false`).
It permits HTTP only for `localhost` or literal loopback addresses. The foreground
publisher assumes one daemon owns the managed input tree; it does not implement
multi-process leader election or lock files. Private new-file permissions are
implemented; installation/root ownership and deployment hardening are covered by
the stage 4 packages and provisioning instructions. Existing ancestor safety
remains an administrator responsibility.

Log state transitions, poll/HTTP status codes, retry/backoff, per-unit publication success/failure, and report-upload outcome via structured logging. Do not log API keys, JSON template/parameter bodies, raw `dscResult`, stderr or any other potentially secret material. Respect cancellation and timeouts. Independent restarts are supported. Separate Windows service and Linux systemd packaging follow `dsc-reconciler`'s platform patterns, not its binary/package identity. Windows SCM logs go to its own Application Event Log source; cancellation has a 30-second cleanup deadline, with nonzero exit on timeout. Packages preserve provisioned data and never install DSC or `dscd`.

## 8. Limitations and acceptance scenarios

1. New machine with zero desired configs receives a 200 and commits `machine.etag`; subsequent 304 leaves files untouched.
2. New/changed unit with a parameter sidecar installs `.NAME.revision` and parameters before template rename; `dscd` never reconciles a mixed pair during a cooperating update.
3. Changed unit with removed parameters deletes the old sidecar before exposing the new template; empty `{}` still creates an empty JSON object file.
4. Active Linux `flock` or Windows delete-sharing denial causes backoff, not forced deletion. Old result published before the lock is released is not mislabeled.
5. Removal of Registry config deletes only service-owned local inputs and result, and doesn't undo machine state. An HTTP failure never triggers deletion.
6. Interrupted update leaves the ETag uncommitted for retry on subsequent 200. Missing/invalid local files cannot be assumed repaired solely because a 304 was received.
7. Watcher processes published complete result files, including failure envelopes with no `inputHash`. Upload identity is deterministic and idempotent across restart; no duplicate history records.
8. Historical/removed-revision upload is accepted by new Registry API and does not replace current-revision status.
9. Local admins' other `config.d/` subtrees are unchanged. No secrets in logs. Linux/Windows runtime integration tests pass against real `dscd` lock behavior.

## 9. Upstream compatibility blockers

- Current `dsc-reconciler` source implements the locking contract above in `internal/reconcile/protection.go`, `files_linux.go` and `files_windows.go`, with the handle retained through `writer.Write`. Release/deployment availability and native sync-agent interoperability still require verification in later stages.
- Rechecked at Registry `5a4ddb543afc103de3ad933922ed5f603f3ff656`: `docs/api.md` and `internal/report/report.go` accept client-defined configuration/parameter paths. The former hardcoded `managed/` restriction is fixed upstream, but deployed-version verification remains required before production integration. Never falsify the result envelope for older servers.
- New server report-history behavior overrides older discussions that presumed 409 on an old content revision. `POST /v1/agent/reports` accepts old/deleted revisions and de-duplicates exact retries; see live Registry API documentation.
- Registry's `internal/desired/json.go` currently permits Windows device stems. The sync client additionally rejects `con`, `prn`, `aux`, `nul`, `com1`-`com9` and `lpt1`-`lpt9`, including names with dot extensions, on both platforms. This stricter safety check is an explicit upstream contract discrepancy, not a rewritten or normalized server name.
