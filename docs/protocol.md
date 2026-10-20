# HTTP and filesystem integration contract

**Authority:** https://github.com/Bpoe/dsc-registry/blob/main/docs/api.md and https://github.com/Bpoe/dsc-reconciler/blob/main/docs/design.md . This is a consumer-oriented reference, not a fork of the server schema. Verify contract changes against the current code during each implementation PR.

## Machine GET

Request (when a locally committed validator exists, including normal startup):

```http
GET /v1/agent/configurations HTTP/1.1
Authorization: ApiKey dscm_<keyId>.<secret>
If-None-Match: "machine-<uuid>-config-<revision>"
```

- Store/send the **exact quoted HTTP ETag** from Registry; example formatting above is illustrative.
- `200` JSON: `configurationRevision` integer and `configurations` array with `name` (safe lowercase stem), `revision` (opaque UUID), `template` (JSON object), `parameters` (optional object or null). ETag and Cache-Control headers supplied.
- `304`: no JSON payload; Cache-Control may still define next polling interval; do not change local configuration files.
- Only trust a complete valid `200` as the authoritative set of desired names. Never delete because of error/partial JSON/304.

### Client validation and poll policy

- Read `configDir/dscregsync/machine.etag` once at startup without creating it.
  Preserve the validator bytes, including quotes and a weak `W/` prefix, trimming
  only one terminal LF or CRLF. The tag limit is 4096 bytes (excluding the newline).
  Missing root/directory/file means no validator; malformed, unreadable or
  non-regular metadata and managed symlinks fail startup.
- A 200 requires a single valid ETag and UTF-8 `application/json`. Bound the
  uncompressed collection to 16 MiB and each template plus parameters to 1 MiB.
  Require a nonnegative integer collection revision and a non-null array;
  `[]` is valid. Require unique safe names, canonical lowercase UUID revisions
  and object-valued documents. Absent/null parameters differ from `{}`.
  Reject duplicate JSON properties, invalid UTF-8, excessive nesting, trailing
  data and legacy `{format,content}` document wrappers. Ignore additive response
  fields and retain opaque document bytes/numbers without DSC interpretation.
- Names use Registry's 1-64-byte lowercase ASCII stem grammar. Windows device
  stems (including dot extensions) are also rejected on Linux and Windows.
  Registry currently permits these; upstream alignment is needed. The client
  fails the entire response, never sanitizes a name or skips a bad unit.
- A 304 has no collection and cannot replace the committed validator. An
  unsolicited 304 without a request validator is an error. A validated 200
  advances the validator only after successful complete publication and the
  final on-disk ETag rename.
- On successful 200/304, use `Cache-Control: max-age` (including quoted integers),
  falling back to 300 seconds or `pollFallbackSeconds` if missing or malformed.
  Normal delays are bounded to 5 seconds-1 hour. `no-cache`/`no-store` select
  the 5-second lower bound; duplicated max-age directives use the fallback.
- Failures use exponential backoff starting at 5 seconds and doubling to
  5 minutes, reset by a successful 200/304. `Retry-After` delta-seconds or
  HTTP-date can extend a failure delay, up to 24 hours. Add 0-10% positive jitter
  without exceeding the applicable cap, never reducing the server delay within
  those bounds. Requests and waits are cancellable.
- HTTPS uses OS trust roots and normal certificate verification. Never follow
  redirects or send a machine identity as a path/query parameter. Routine logs
  contain status, counts and delays, not keys, URLs, validators or response bodies.

Example 200 body:

```json
{
  "configurationRevision": 7,
  "configurations": [
    {
      "name": "docker",
      "revision": "77701b57-ddb9-4e7b-b235-b04ed87e3468",
      "template": {"$schema":"https://aka.ms/dsc/schemas/v3/bundled/config/document.json","resources":[]},
      "parameters": {"Runtime":"containerd"}
    }
  ]
}
```

## Filesystem handshake

With a fully installed `docker` unit:

```text
config.d/dscregsync/docker.json
config.d/dscregsync/docker.parameters.json   # absent if server parameters null/missing
config.d/dscregsync/.docker.revision          # UUID (not inputHash)
config.d/dscregsync/machine.etag              # complete collection HTTP validator
results.d/dscregsync/docker.json.result.json # dscd-generated JSON envelope
```

`dscd` recursively discovers `.json` templates, ignores dot-prefixed names and does not execute `.parameters.json` alone. Its result envelope contains `configuration: "dscregsync/docker.json"` and potentially `parameters: "dscregsync/docker.parameters.json"`; `inputHash` may be absent for failures. `dscd` must hold its per-unit protection until the result is fully published.

Current reconciler source implements that protection: Linux opens without
following symlinks, acquires exclusive `flock` and checks pathname identity;
Windows opens with only `FILE_SHARE_READ`. The reconciler closes the protected
handle after result publication. The writer mirrors the relative configuration
path and appends `.result.json`, using same-directory temporary replacement.
This source check is not a substitute for later native interoperability tests.
PR 2 rechecked reconciler source at
`d6010bf737dbbbfd56d69d98d1fc20132022c8dd` and tests the same protection in
separate helper processes on both platforms, without invoking DSC.

On update/removal `dscregsyncd` removes the **old template first**, only after respecting OS-specific protection. It then removes the old result and parameters; for an update it publishes the new parameters if present, writes `.docker.revision`, then atomically publishes `docker.json` last. Complete the whole 200 collection before writing `machine.etag`.

The publisher stages compact, sorted-key JSON using exact JSON numbers (no
floating-point conversion), with same-directory hidden private files. Absent
or null parameters produce no sidecar; `{}` produces a sidecar. Unchanged
revisions skip publication only if the template/revision are regular files and
parameter presence matches. Existing file contents are not hashed.

Flock and sharing contention retry with cancellable 25-250 ms delays. On Linux,
the opened configuration must still identify the pathname while its exclusive
lock is held. On Windows, ordinary delete-sharing violations retry; other errors
(including ACL denial and access-denied metadata replacements) fail the
collection and retry through normal polling backoff. The old ETag remains.
Per-unit serialization covers template removal through final publication and
report snapshot reads. `Publisher.SnapshotResult` reuses the confined directory
and no-follow file helpers, checks opened/path identities and file size/mtime
before/after reading, and requires a regular installed template and valid revision.
No HTTP operation runs inside this gate.

Failure/process exit at an intermediate boundary leaves at most a partial
installation and hidden staging files. The next successful 200 cleans abandoned
agent staging files and repairs missing units without undoing completed units.
Successful units and their new results survive collection retries.
The ETag rename is the final commit point. No persistent marker, saved hash,
backup, lock file or rollback protocol is used.

## Machine report POST

Prefer the current history API:

```http
POST /v1/agent/reports HTTP/1.1
Authorization: ApiKey dscm_<keyId>.<secret>
Content-Type: application/json

{
  "configurationName": "docker",
  "configurationRevision": "77701b57-ddb9-4e7b-b235-b04ed87e3468",
  "uploadId": "95951a72-7b8f-43e2-b602-1802d091c91d",
  "result": {
    "schemaVersion": 1,
    "configuration": "dscregsync/docker.json",
    "operation": "test",
    "startedAt": "2026-10-18T12:00:00Z",
    "finishedAt": "2026-10-18T12:00:00.005Z",
    "durationMs": 5,
    "outcome": "failed",
    "exitCode": null,
    "dscResult": null,
    "error": {"kind":"start","message":"DSC unavailable"},
    "stderr": ""
  }
}
```

- Do not synthesize or reinterpret `result`; send the exact bytes of the `dscd` envelope as a nested JSON object, not JSON-encoded string. Copy bytes into wrapper without changing their encoding/whitespace across retries. No `inputHash` validation to infer revision.
- `201 Created`: accepted new historical report; `200 OK`: exact duplicate of an existing accepted upload ID, both success. `409`: upload ID reused with different content/revision — a fatal identity error, not an obsolete-revision signal. `401/403` credential/role problems; transient 429/503/5xx are retryable with reasonable backoff.
- Unlike the original API, the current report-history API **accepts old or removed configuration content revisions**. The server's current status view filters reports by current desired revision.
- The legacy `POST /v1/agent/configurations/{name}/report` exists with a slightly different wrapper and `204` on success. Use only if explicitly required by server compatibility; do not silently fall back on new-route errors.

### Upload identity and exact bytes

The fixed UUIDv5 namespace is `4e70e5f8-87c8-5e72-90bb-645fb9c79a04`.
Its RFC 9562 name input is this concatenation, in order:

1. Unsigned 32-bit big-endian byte length of the validated ASCII configuration name, followed by those name bytes.
2. Unsigned 32-bit big-endian byte length of the canonical lowercase Registry revision UUID, followed by its ASCII bytes.
3. The 32 raw bytes of SHA-256 over the **entire exact result file**, including any leading/trailing JSON whitespace or terminal newline.

Apply standard UUIDv5: SHA-1 over the namespace's 16 network-order bytes and the
above input, take the first 16 digest bytes, set version 5 and RFC variant bits,
and format lowercase canonical hyphenated UUID text. This is identity generation,
not authentication or an `inputHash` mapping. The namespace and encoding must
remain fixed across releases. Golden vector: name `docker`, revision
`77701b57-ddb9-4e7b-b235-b04ed87e3468`, file bytes `{"schemaVersion":1}` followed
by one LF produce `8d9330d3-1b7e-5980-9256-dac0bfe18b18`.

The wrapper is constructed once by directly appending the original file bytes as
the `result` value; never pass it through `json.Marshal`, even as `RawMessage`
(which would compact/re-escape it). The client owns this immutable request body.
Registry trims whitespace outside the nested JSON value but preserves everything
inside it. File whitespace changes intentionally produce a different upload ID.
Exact retries/restarts of an unchanged file produce the same ID/body; different
execution payloads produce different IDs without a persistent ledger.

Results are bounded to 16 MiB minus 512 bytes of reserved wrapper space.
The complete wrapper must also fit 16 MiB, Registry's default report limit.
Validate UTF-8, complete object-shaped JSON, duplicate properties and nesting
including the wrapper level, but leave envelope semantics and resource payloads
to Registry. Never require an `inputHash`; input failures legitimately omit it.
A stricter server limit can return `413`, which is not retried unchanged.

### Report retry and watcher policy

Only `201` and `200` succeed. `400` (invalid report), `409` (identity conflict),
`413`, `415`, other nonretryable 4xx and unexpected 2xx/3xx are terminal for that
unchanged upload ID in this process. Log only safe unit/status metadata; do not
alter the body, generate another ID, or fall back to the legacy route.
Startup may retry that file again because suppression is not persisted.

Transport errors, timeouts, `401`/`403`, `408`, `429` and 5xx retry per unit using
the same failure schedule as polling: 5 seconds doubling to 5 minutes, positive
0-10% jitter, and `Retry-After` seconds/HTTP-date extending the wait up to 24 hours.
Repeated notifications never shorten that deadline. At the next attempt,
snapshot the latest file; newer bytes supersede older work and missing
destinations discard it. Network calls never hold the publisher gate.
An old in-flight snapshot retains its old revision even if inputs are removed.
Receipts/error bodies are bounded-drained, never persisted or logged.

`fsnotify` supplies native create/write/rename/remove hints, coalesced by name
for 100 ms. Hidden temporary files and unrelated subtrees are ignored. Attach
existing ancestor directories before inspecting children to handle absent roots
and fast mkdir/publication; event paths use native comparison rules, including
Windows case insensitivity. Scan once after attachment on startup, watcher
failure/overflow/closure recovery, and directory creation/replacement. Failed
watch setup retries with cancellable 1-30 second backoff. Stable invalid local
JSON or missing/invalid revision metadata waits for another hint/scan; transient
file/identity failures back off. There is no healthy periodic result scan.
Neither observation nor upload modifies results, input files or ETags.
An in-memory name/ID/deadline map and one in-flight request are not an outbox;
missed intermediate executions cannot be recovered from the latest-result file.

## Compatibility check before integration

Registry `5a4ddb543afc103de3ad933922ed5f603f3ff656` documents and implements
client-defined envelope paths in `docs/api.md` and `internal/report/report.go`.
`configuration` must be a nonempty string; optional/null `parameters` may be any
string, including empty. The old hardcoded `managed/` restriction is no longer
an upstream source blocker. Verify deployment of that change before production
integration; the client must never forge envelope paths for an older server.
PR 3 also rechecked reconciler `d6010bf737dbbbfd56d69d98d1fc20132022c8dd`:
`internal/dsc/result.go`, `internal/results/writer.go`, platform file helpers,
and the protection retained through `writer.Write` in the reconciler. Tests
use those envelope shapes and native filesystem behavior with mock HTTP; they
do not execute DSC or establish deployed cross-service compatibility.
