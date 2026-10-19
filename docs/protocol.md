# HTTP and filesystem integration contract

**Authority:** https://github.com/Bpoe/dsc-registry/blob/main/docs/api.md and https://github.com/Bpoe/dsc-reconciler/blob/main/docs/design.md . This is a consumer-oriented reference, not a fork of the server schema. Verify contract changes against the current code during each implementation PR.

## Machine GET

Request (after first accepted 200):

```http
GET /v1/agent/configurations HTTP/1.1
Authorization: ApiKey dscm_<keyId>.<secret>
If-None-Match: "machine-<uuid>-config-<revision>"
```

- Store/send the **exact quoted HTTP ETag** from Registry; example formatting above is illustrative.
- `200` JSON: `configurationRevision` integer and `configurations` array with `name` (safe lowercase stem), `revision` (opaque UUID), `template` (JSON object), `parameters` (optional object or null). ETag and Cache-Control headers supplied.
- `304`: no JSON payload; Cache-Control may still define next polling interval; do not change local configuration files.
- Only trust a complete valid `200` as the authoritative set of desired names. Never delete because of error/partial JSON/304.

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

On update/removal `dscregsyncd` removes the **old template first**, only after respecting OS-specific protection. It then removes the old result and parameters; for an update it publishes the new parameters if present, writes `.docker.revision`, then atomically publishes `docker.json` last. Complete the whole 200 collection before writing `machine.etag`.

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

The current server documentation **and the `internal/report/report.go` implementation** still contain hardcoded `managed/` envelope path validation (verified 2026-10-19). It must be removed as requested in a separate Registry change. Accepting arbitrary valid `dscd` configuration paths is part of server/client decoupling; client must not forge a different path to work around it.
