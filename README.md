# DSC Registry Sync (`dscregsyncd`)

`dscregsyncd` is a small, independent Go service that synchronizes DSC Registry desired-state configurations to an existing local `dscd` reconciler and uploads `dscd` results to DSC Registry.

**Status:** design and Copilot implementation scaffold, not yet an implemented daemon. Do not install or run this repository as a working service until the implementation PRs land.

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
/etc/dsc/machine.etag                           # exact HTTP ETag including quotes
/etc/dsc/config.d/dscregsync/
    docker.json                                 # managed DSC template
    docker.parameters.json                      # optional direct parameter mapping
    .docker.revision                            # Registry content revision UUID
/var/lib/dsc/results.d/dscregsync/
    docker.json.result.json                     # produced by dscd
```

Windows counterparts live under `%ProgramData%\dsc\` (including `config.d\dscregsync\` and `results.d\dscregsync\`). The config/API key file uses restricted ACLs. The service account must have permissions appropriate for managing the directory and result snapshots.

`machine.etag` is an HTTP cache validator, **not** the integer machine configuration revision. Per-unit `.NAME.revision` contains the opaque per-configuration content revision; neither is a `dscd` execution hash.

## Design and implementation

- [Architecture, state, lifecycle and failure rules](docs/design.md)
- [HTTP and filesystem integration contracts](docs/protocol.md)
- [Small, reviewable Copilot PR sequence](docs/implementation-plan.md)
- [First Copilot prompt](COPILOT_KICKOFF.md)
- [Coding agent instructions](AGENTS.md)
- [Example configuration](examples/dscregsyncd.json)

## Cross-repository prerequisites

1. `dsc-reconciler`: complete and test per-configuration protection from input capture through result publication (Linux cooperative exclusive `flock`, Windows denial of delete sharing).
2. `dsc-registry`: remove the hardcoded `managed/{name}.json` validation from incoming result envelopes. As checked on 2026-10-19, both `docs/api.md` and `internal/report/report.go` still enforce `managed/` paths; verify the fix has landed before integration tests.
3. Treat current `dsc-registry/docs/api.md` and actual server behavior as the authority for machine GET and the newer `POST /v1/agent/reports` API, including history and deduplication.
