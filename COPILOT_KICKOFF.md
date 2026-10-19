# First Copilot task — PR 1 only

Please implement **PR 1 — daemon baseline and conditional desired-state retrieval** in this repository.

1. Before coding, read `AGENTS.md`, `docs/design.md`, `docs/protocol.md`, `docs/implementation-plan.md`, and current `Bpoe/dsc-registry/docs/api.md`.
2. Implement only the scope of PR 1. This includes the Go executable, JSON configuration validation, structured logging, service lifecycle suitable for foreground testing, a testable HTTP client for machine-scoped conditional configuration polling, ETag read behavior, caching/jitter/backoff, and focused tests.
3. Do **not** publish local configurations, write `machine.etag`, implement locks, watch results, POST reports, or add systemd/MSI packaging in this PR. Those belong to PRs 2–4.
4. Follow standard Go conventions, standard library first, shallow packages, and avoid frameworks. Keep credentials out of logs and tests.
5. Create the initial GitHub Actions Go CI checks as appropriate and update README if commands change.
6. Run `gofmt`, `go test ./...`, `go vet ./...`, `go test -race ./...` where supported, Linux build and Windows cross-build. Report what actually ran and any blockers.
7. Return a list of changed files, behavior implemented, test evidence, and anything that conflicts with the server contract. Do not silently change the product design or execute later stages.

Make the PR small enough for a direct human review and do not expand its scope to solve unrelated cross-repository issues.
