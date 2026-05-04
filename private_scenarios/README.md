# Private scenarios

Real-LLM, real-environment tests for client workflows. Everything in
this directory except this README and `*.example` templates is
gitignored — credentials, client directives, baseline snapshots, and
per-client test files all stay local.

## Quick start (no MCP, no setup)

The bundled `ping_test.go.example` is a zero-dependency smoke test:
it auto-spawns an apteva-server with an ephemeral database, sets a
directive, injects one console event, waits for the agent to think,
and reports tokens+cost. Copy it and run.

```bash
cp ping_test.go.example ping_test.go
RUN_TESTKIT_TESTS=1 go test -tags testkit -run TestPing -v -count=1
```

If this PASSes you know the harness is working end-to-end. From here,
add phases and MCPs (see below) for real client tests.

## Build tag

Every test file in this directory uses `//go:build testkit`. That
keeps these tests out of the default `go test ./...` run so they
don't fire in CI without being asked, and don't require a live server
for normal unit-test runs.

## Running against an existing server (no autostart)

If you already have a server running with MCPs configured, point
testkit at it:

```bash
export APTEVA_SERVER_URL=http://localhost:5280
export APTEVA_API_KEY=sk-...               # from dashboard → Keys
export APTEVA_TEST_INSTANCE_ID=42          # the instance to run against
RUN_TESTKIT_TESTS=1 go test -tags testkit -run TestMyWorkflow -v -count=1
```

Testkit skips the autostart flow entirely when the URL is reachable
and uses your existing API key + instance. Tests can then reference
MCPs you wired up via **Dashboard → Integrations** by name — no
credentials land in committed code.

## Iteration loop

The typical inner loop:

1. Edit your directive string in the test file.
2. Re-run `go test -run TestMyWorkflow`.
3. Read the logged `tokens=… cost=$…` summary.
4. Compare against a baseline JSON (see `Session.AssertBaseline`).
5. Commit the directive change (to your private branch/repo).

## What NOT to commit

- Real API keys (Google, Deepgram, Composio, etc.) — use env vars.
- Instance IDs that point to production data.
- Client directives with business-sensitive framing.
- Baseline JSONs for client workflows (they leak token counts → cost).

The `.gitignore` in this directory blocks everything by default so
forgetting is safe.
