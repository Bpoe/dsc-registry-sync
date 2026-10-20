# DSC Registry Sync (`dscregsyncd`)

`dscregsyncd` is a small, independent Go service that synchronizes DSC Registry desired-state configurations to an existing local `dscd` reconciler and uploads `dscd` results to DSC Registry.

**Status:** Stage 4 adds native Windows service support, systemd/DEB/RPM/MSI
packaging, disposable-host installation checks and real cross-service integration
coverage. The complete roundtrip, including real `dscd` protection, has run on
Windows. Linux runtime, installed-service lifecycle and actual reboot acceptance
remain release gates; this is **not yet a v1 production-readiness declaration**.

## Responsibilities

- Authenticate to [`dscregd`](https://github.com/Bpoe/dsc-registry) with an already-provisioned **machine API key** over HTTPS.
- Poll `GET /v1/agent/configurations` conditionally with the machine ETag and respect `Cache-Control`.
- Publish `.json` DSC templates and optional `.parameters.json` sidecars into the dedicated `config.d/dscregsync/` directory.
- Observe `dscd`'s atomic result-file publications using a filesystem watcher and upload the latest available result through the Registry machine API.
- Persist minimal revision/ETag state; allow `dscd` to continue offline.

**Not responsible for:** executing DSC, registering machines, issuing keys, interpreting resource compliance, managing local non-Registry configurations, or guaranteeing delivery of every historical execution report.

## Components

```text
DSC Registry (`dscregd`)
       ^ HTTPS machine API
       |
  `dscregsyncd`
       | file publication / result observation
       v
`dscd` (independent, preexisting local reconciler)
```

## Linux default paths

```text
/etc/dsc/dscregsyncd.json                       # service configuration
/etc/dsc/dscregsyncd.key                        # machine API key (root-owned, 0600)
/etc/dsc/config.d/dscregsync/
    machine.etag                                # exact committed HTTP ETag including quotes
    docker.json                                 # managed DSC template
    docker.parameters.json                      # optional direct parameter mapping
    .docker.revision                            # Registry content revision UUID
/var/lib/dsc/results.d/dscregsync/
    docker.json.result.json                     # produced by dscd
```

Windows counterparts live under `%ProgramData%\dsc\` (including `config.d\dscregsync\` and `results.d\dscregsync\`). The config/API key file uses restricted ACLs. The service account must have permissions appropriate for managing the directory and result snapshots.

`machine.etag` is an HTTP cache validator, **not** the integer machine configuration revision. Per-unit `.NAME.revision` contains the opaque per-configuration content revision; neither is a `dscd` execution hash.

## Build and run the synchronizer

Use Go 1.27 or newer. External Go modules are `golang.org/x/sys` for Windows
handle-relative creation/private ACLs and `github.com/fsnotify/fsnotify` for
native filesystem notifications (inotify on Linux, ReadDirectoryChangesW on Windows).

```sh
go build -o dscregsyncd ./cmd/dscregsyncd
./dscregsyncd -config /etc/dsc/dscregsyncd.json
```

On Windows, from PowerShell:

```powershell
go build -o dscregsyncd.exe .\cmd\dscregsyncd
.\dscregsyncd.exe -config "$env:ProgramData\dsc\dscregsyncd.json"
```

Provision a machine key separately in the protected `apiKeyFile`; never put the
key in JSON, command-line arguments or source control. Restrict the key file to
the service identity (Linux mode 0600, Windows restricted ACLs). This stage reads
the key at startup; restart after key rotation. It does not change key permissions.
`-config` defaults to the platform service-config path above. `-help` prints
usage; `-version` identifies the build. Ctrl+C, and SIGTERM on Linux, cancel requests and waits. Logs are JSON on
stderr; startup/validation failures exit nonzero, while HTTP failures back off.
Windows SCM stop/shutdown cancels both loops; installed-service JSON logs go to
the Application Event Log (`dscregsyncd`, event ID 1). Foreground and service
shutdown allow 30 seconds for cleanup before exiting nonzero.

See [deployment.md](docs/deployment.md) for package builds, installation,
protected credential provisioning, upgrade/uninstall behavior and acceptance
checks. Neither package family installs DSC or `dscd`, registers a machine, or
ships a key or active configuration file.

`registryUrl` is required and must be an HTTPS origin, with no credentials,
non-root path, query or fragment. Other fields default as shown in the
[example](examples/dscregsyncd.json), with platform-specific paths. All paths
must be absolute; input/result directories may be absent. For explicit local
development only, set `"allowInsecureLoopback": true` with an HTTP URL whose
host is `localhost` or a loopback IP literal. TLS verification stays enabled
for HTTPS, and redirects are never followed.

`requestTimeoutSeconds` is 1-300 (default 30); `pollFallbackSeconds` is 5-3600
(default 300). Unknown, duplicate, null and incorrectly typed config fields
are rejected. Config files are limited to 64 KiB and key files to 8 KiB.
Cache/retry policy and response limits are documented in [protocol.md](docs/protocol.md).

The first poll uses `config.d/dscregsync/machine.etag` if present. Missing cache
means an unconditional GET; malformed/unreadable cache is a startup error.
A fully validated 200 publishes additions/updates and removes absent managed
units. Only after the whole collection succeeds does `machine.etag` advance,
on disk and in memory. A partial failure leaves the previous validator and
retries without rollback. A valid empty 200 removes all managed units; a 304,
invalid response or HTTP failure never deletes cached desired state.

Only `dscregsyncd` may write `configDir/dscregsync`. The configured roots and
their ancestors are trusted administrator-provisioned paths. Managed entries
must be regular files/directories, not symlinks or reparse-point redirects.
Unexpected input entries fail application rather than being deleted.
Abandoned agent staging files are cleaned on the next 200. Sibling input and
result subtrees are untouched.

New managed directories use Linux mode 0700; templates, parameters, revisions
and ETags use 0600. On Windows, newly created managed directories and files have
protected ACLs allowing only the service identity, SYSTEM and Administrators.
Existing directory ACLs/modes and configured roots are not rewritten.
Run `dscd` with the same identity (or appropriate privileged access) and protect
the configured roots from other writers.

Updates stage and flush private files, remove the protected old template,
remove its parameters/result, then publish parameters and revision before the
new template. Linux uses cooperative exclusive flock with opened-file identity
verification; Windows respects `dscd`'s denied write/delete sharing. Contention
uses cancellable 25-250 ms backoff. Other filesystem failures use the poll
loop's failure backoff. This is retryable ordered publication, not multi-file
crash atomicity or a sudden-power-loss guarantee. After local out-of-band file
loss, a 304 alone cannot repair the collection: invalidate the cache while the
daemon is stopped or wait for a desired-state change.

Results use an independent notification-driven loop. Startup and watcher-recovery
scans cover files published before subscriptions were established. Missing result
directories are observed through ancestor watches until they appear; no results
directory or file is created by the watcher, and there is no periodic results
polling loop. Duplicate create/write/rename notifications coalesce for 100 ms;
only complete, confined `NAME.json.result.json` destinations are read.

Snapshot reads share the publisher's per-unit gate, capturing exact result bytes
with `.NAME.revision`. Upload HTTP never holds that gate. An already captured old
report may finish after an update or removal, with its original revision.
`inputHash`, optional/null fields, embedded paths and resource results are never
rewritten or used to infer revision/compliance.

The history API receives an immutable JSON wrapper containing the unmodified
result. A documented [UUIDv5 derivation](docs/protocol.md#upload-identity-and-exact-bytes)
keeps retries and process restarts idempotent. `201` and duplicate `200` both
succeed. Invalid/conflicting reports (`400`/`409`) stop retrying unchanged content
until restart; authentication, transport, rate-limit and server failures back off
with `Retry-After`. A newer local result supersedes failed work at the next
attempt. Only notification names, latest IDs and retry deadlines live in memory:
there is no outbox, upload ledger or report-history queue. While offline, `dscd`
continues locally, and intermediate executions may be lost. See the protocol for
size limits, retry bounds and other HTTP statuses.

```sh
go test ./...
go vet ./...
go test -race ./...
```

Run `gofmt` on changed Go sources. Race tests require a supported C toolchain
and cgo enabled. CI checks formatting, tests, vet, race tests and native builds
on Linux and Windows, plus a Windows cross-build from Linux. Deployment CI also
builds pinned real Registry/reconciler peers and runs native roundtrips with DSC
3.3.0, then exercises package install/repair/upgrade/uninstall on disposable
hosts. Tagged releases require all these jobs before publishing packages.
RPM lifecycle checks run in Fedora without systemd; Debian checks exercise real
systemd. A green cross-build alone is never native runtime evidence.

Publisher tests include separate-process OS protection, abrupt process exits
at publication boundaries, restart/retry, incomplete collections, private
permissions, path confinement, stale-inode rejection on Linux and Registry
200/304/error-to-filesystem coverage. Windows symlink tests require the OS
privilege to create symlinks. Tests using default paths can require an isolated
`ProgramData` environment value if the real installation is inaccessible.
Result tests cover real envelope shapes, null/absent fields, exact-byte uploads,
stable UUIDs, restart deduplication, native notifications, coalescing, directory
creation/replacement, watcher overflow/closure recovery, offline latest-only
behavior, HTTP response policy, cancellation, concurrent atomic snapshots, and
polling/publication while an old report upload is blocked. Fixtures under
`testdata/results` follow the upstream schema; the null-optionals fixture also
exercises Registry-permitted optional fields and an additive field.

## Design and implementation

- [Architecture, state, lifecycle and failure rules](docs/design.md)
- [HTTP and filesystem integration contracts](docs/protocol.md)
- [Small, reviewable Copilot PR sequence](docs/implementation-plan.md)
- [First Copilot prompt](COPILOT_KICKOFF.md)
- [Coding agent instructions](AGENTS.md)
- [Example configuration](examples/dscregsyncd.json)
- [Deployment, operations and integration acceptance](docs/deployment.md)

## Cross-repository prerequisites

1. `dsc-reconciler`: current upstream source implements per-configuration protection through result publication (Linux cooperative exclusive `flock`, Windows denial of write/delete sharing). PR 2 tests reproduce that OS handshake in separate processes. Stage 4's opt-in test also holds a real DSC resource request inside real `dscd` while publication waits. Windows has passed; Linux and compatible deployed versions remain required before v1 readiness.
2. `dsc-registry`: the former hardcoded `managed/{name}.json` report validation is removed in upstream `main` (rechecked at `5a4ddb5` and exercised with real relative-path reports on Windows). Verify the deployed Registry includes the history API and path fix; never falsify an envelope to work around older servers.
3. Treat current `dsc-registry/docs/api.md` and actual server behavior as the authority for machine GET and the newer `POST /v1/agent/reports` API, including history and deduplication.
4. Registry's current name validator accepts Windows device stems such as `con` and `nul.txt`. This client rejects them on **both** platforms, in addition to the documented Registry stem grammar, rather than accepting a collection that cannot safely be published on Windows. Such a response fails validation in its entirety; upstream name alignment remains separate work.
