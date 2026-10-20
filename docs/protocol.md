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
will also cover report snapshot reads in PR 3.

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

## Compatibility check before integration

Registry `e443c3f925c0094d3b033e7ac9c8590297b8dbef` now documents and implements
client-defined envelope paths in `docs/api.md` and `internal/report/report.go`.
`configuration` must be a nonempty string; optional/null `parameters` may be any
string, including empty. The old hardcoded `managed/` restriction is no longer
an upstream source blocker. Verify deployment of that change before PR 3/4
integration; the client must never forge envelope paths for an older server.
