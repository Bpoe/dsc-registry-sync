# DSC Registry Sync (`dscregsyncd`)

`dscregsyncd` is a small, independent Go service that synchronizes DSC Registry desired-state configurations to an existing local `dscd` reconciler and uploads `dscd` results to DSC Registry.

**Status:** PR 1 read-only foreground baseline. The daemon validates configuration, authenticates and conditionally polls desired state. It does **not** yet publish configurations, write ETags, watch/upload results or install a native service. The responsibilities below describe the eventual v1 service, not completed functionality.

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

## Build and run the read-only baseline

Use Go 1.27 or newer. No external Go modules are required.

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
the key at startup; restart after key rotation. It does not change permissions.
`-config` defaults to the platform service-config path above. `-help` prints
usage. Ctrl+C, and SIGTERM on Linux, cancel requests and waits. Logs are JSON on
stderr; startup/validation failures exit nonzero, while HTTP failures back off.

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
**A received ETag is never promoted, even in memory, in this stage.** Until PR 2
can commit an applied collection, subsequent polls keep using the startup
validator (or remain unconditional). No directories or files are created.
An empty 200 is distinct from a 304 or failure, but none changes local state yet.

```sh
go test ./...
go vet ./...
go test -race ./...
```

Run `gofmt` on changed Go sources. Race tests require a supported C toolchain
and cgo enabled. CI checks formatting, tests, vet, race tests and native builds
on Linux and Windows, plus a Windows cross-build from Linux. This is not yet
cross-service integration or evidence that v1 is production-ready.

## Design and implementation

- [Architecture, state, lifecycle and failure rules](docs/design.md)
- [HTTP and filesystem integration contracts](docs/protocol.md)
- [Small, reviewable Copilot PR sequence](docs/implementation-plan.md)
- [First Copilot prompt](COPILOT_KICKOFF.md)
- [Coding agent instructions](AGENTS.md)
- [Example configuration](examples/dscregsyncd.json)

## Cross-repository prerequisites

1. `dsc-reconciler`: current upstream source implements per-configuration protection through result publication (Linux cooperative exclusive `flock`, Windows denial of write/delete sharing). PRs 2-4 must still verify the deployed version and native cross-service behavior.
2. `dsc-registry`: remove the hardcoded `managed/{name}.json` validation from incoming result envelopes. As checked on 2026-10-19, both `docs/api.md` and `internal/report/report.go` still enforce `managed/` paths; verify the fix has landed before integration tests.
3. Treat current `dsc-registry/docs/api.md` and actual server behavior as the authority for machine GET and the newer `POST /v1/agent/reports` API, including history and deduplication.
4. Registry's current name validator accepts Windows device stems such as `con` and `nul.txt`. This client rejects them on **both** platforms, in addition to the documented Registry stem grammar, rather than accepting a collection that cannot safely be published on Windows. Such a response fails validation in its entirety; upstream name alignment remains separate work.
