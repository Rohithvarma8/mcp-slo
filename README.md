# mcp-slo

[![ci](https://github.com/Rohithvarma8/mcp-slo/actions/workflows/ci.yml/badge.svg)](https://github.com/Rohithvarma8/mcp-slo/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-green.svg)](LICENSE)

**Reliability tests for MCP servers, in CI.** `mcp-slo` is a Go CLI and GitHub Action that puts any
[Streamable HTTP](https://modelcontextprotocol.io/specification/2025-11-25/basic/transports) MCP server
under concurrent load, measures latency per JSON-RPC method, and fails the build when you blow an SLO.

```sh
mcp-slo run --url https://example.com/mcp --tool search --args '{"q":"test"}' \
  --slo 'p95<500ms' --slo 'errors<1%'
```

## Why

Correctness checkers like [MCP Inspector](https://github.com/modelcontextprotocol/inspector) tell you whether
a server speaks the protocol. They don't tell you whether it holds up when real clients use it:

- **Latency budgets are product requirements.** Voice assistants are unforgiving. Alexa+ requires "a
  round-trip query response latency of less than 500 ms"
  ([Alexa+ MCP Quickstart](https://developer.amazon.com/docs/alexaplus/add-ons/mcp-toolkit-quickstart.html)). A server that passes every schema check can still miss that budget at p95, or only on a
  cold start.
- **Infra fails in ways unit tests don't catch.** API Gateway throttling, Lambda cold starts, session handling
  under concurrency, and SSE streams that never close only show up when you send real traffic.
- **Clients differ.** Servers have to negotiate protocol versions they've never seen, and client platforms
  add their own rules. Alexa+ wants unauthenticated requests to get a `401 Unauthorized` without a
  `WWW-Authenticate` header ([Alexa+ MCP Quickstart](https://developer.amazon.com/docs/alexaplus/add-ons/mcp-toolkit-quickstart.html), authentication checklist).

`mcp-slo` complements correctness tools by checking performance and resilience on every push.

## What it checks

| Check | What happens |
|---|---|
| **Load + latency** | N workers loop `initialize` → `notifications/initialized` → `tools/list` → `tools/call` for a set duration. Reports count, error rate, p50/p95/p99/max per method and overall, plus the first error. |
| **SLOs** | `p50`/`p90`/`p95`/`p99`/`max`/`mean` latency and `errors` rate, for all requests or one method (`tools/call:p99<1s`). Any breach exits non-zero. |
| **Protocol negotiation** | Initializes at `2025-03-26`, `2025-06-18`, `2025-11-25` and `2026-07-28`, and reports which versions the server accepts, which it downgrades, and which it rejects. The load test uses the newest accepted version. |
| **Cold vs. warm** | `--cold-wait 10m` idles, then measures the first session on its own and compares it with steady-state p50. |
| **Auth probes** | Missing, malformed, and expired bearer tokens: reports the status codes and whether `WWW-Authenticate` is present. Skipped with "no auth required" if the server accepts unauthenticated requests. |
| **`--profile alexa`** | A `401` that carries `WWW-Authenticate` fails, per the [Alexa+ MCP Quickstart](https://developer.amazon.com/docs/alexaplus/add-ons/mcp-toolkit-quickstart.html). A `GET` on the endpoint must also not hang as an open SSE stream. This is a defensive check, not a documented Alexa+ requirement. |

Transport details: every request is a `POST` with `Accept: application/json, text/event-stream`. Responses
can be plain JSON or SSE, and SSE streams are matched by request id, so interleaved notifications are handled.
`Mcp-Session-Id` and `MCP-Protocol-Version` are sent back as the spec requires, and sessions are closed with
`DELETE`.

## Quick start

```sh
go install github.com/Rohithvarma8/mcp-slo@latest

mcp-slo run --url https://your-server.example.com/mcp \
  --concurrency 3 --duration 30s \
  --tool get_active_incidents --args '{}' \
  --slo 'p95<500ms' --slo 'errors<1%' \
  --profile alexa --json report.json --junit junit.xml
```

Sample output from a real run against Night Watch, a Python MCP server on AWS Lambda behind API Gateway:

```text
mcp-slo v0.1.0  https://2wj4gc9lz8.execute-api.us-west-2.amazonaws.com/mcp
3 workers × 30s ≤ 15 req/s · protocol 2025-11-25 · server nightwatch 0.1.0 · profile alexa

PROTOCOL NEGOTIATION
  requested   http  negotiated  result
  2025-03-26  200   2025-03-26  accepted
  2025-06-18  200   2025-06-18  accepted
  2025-11-25  200   2025-11-25  accepted
  2026-07-28  200   2025-11-25  downgraded

AUTH
  no auth required (initialize without a token succeeded); auth probes skipped

LATENCY (steady state, successful requests; 456 requests in 30.4s, 15.0 req/s)
    method                     count  errors   p50    p95    p99    max
    initialize                   114   0.00%  61ms  131ms  142ms  144ms
    notifications/initialized    114   0.00%  57ms  137ms  143ms  148ms
    tools/list                   114   0.00%  63ms  149ms  195ms  418ms
    tools/call                   114   0.00%  66ms  144ms  161ms  2.41s
    all                          456   0.00%  63ms  137ms  164ms  2.41s

CHECKS
  ✓ alexa: GET does not hang: GET returned 405 and closed in 66ms
  ✓ load: server answered requests: 456 of 456 requests succeeded

SLOs
  ✓ p95<500ms  all requests  actual 137ms
  ✓ errors<1%  all requests  actual 0.00%

PASS
```

(That run used `--rate 15`; the other flags are the same as in the quick start above.)

### What it catches

Here is the same server without `--rate`. The run found Night Watch's API Gateway throttle (20 req/s): three
eager clients were enough to trigger `429`s.

```text
LATENCY (steady state, successful requests; 1732 requests in 30.1s, 57.6 req/s)
    method                     count  errors   p50    p95    p99    max
    initialize                   826  63.44%  53ms  123ms  139ms  151ms
    notifications/initialized    302  57.95%  52ms  128ms  144ms  148ms
    tools/list                   302  55.96%  61ms  139ms  155ms  158ms
    tools/call                   302  59.93%  65ms  137ms  158ms  159ms
    all                         1732  60.57%  56ms  129ms  148ms  159ms
  first error: notifications/initialized: HTTP 429: {"message":"Too Many Requests"}

SLOs
  ✓ p95<500ms  all requests  actual 129ms
  ✗ errors<1%  all requests  actual 60.57%  1049 of 1732 requests failed

FAIL: 1 check(s) failed
```

Latency percentiles only count successful requests, so fast rejections can't make a server look faster than
it is.

### Flags

| Flag | Default | |
|---|---|---|
| `--url` | | Streamable HTTP MCP endpoint (required) |
| `--concurrency` | `5` | concurrent workers |
| `--duration` | `30s` | steady-state load duration |
| `--rate` | `0` | max requests/s across all workers (`0` = as fast as possible) |
| `--tool`, `--args` | | tool to call each iteration and its JSON arguments |
| `--slo` | | repeatable: `p95<500ms`, `errors<1%`, `tools/call:p99<=1s` |
| `--profile` | | `alexa` |
| `--cold-wait` | | idle time before the cold-start measurement |
| `--header` | | repeatable: `'Authorization: Bearer …'` |
| `--timeout` | `10s` | per-request timeout |
| `--get-timeout` | `3s` | Alexa GET hang threshold |
| `--protocol-version` | newest accepted | pin the load-test protocol version |
| `--json`, `--junit` | | write reports to files |

If `$GITHUB_STEP_SUMMARY` is set, a Markdown summary is appended to it. Exit codes: `0` everything passed,
`1` an SLO or check failed, `2` usage error.

## GitHub Action

```yaml
name: mcp-slo
on: [push, pull_request]

jobs:
  slo:
    runs-on: ubuntu-latest
    steps:
      - uses: Rohithvarma8/mcp-slo@v0.1.0
        with:
          url: https://your-server.example.com/mcp
          concurrency: 3
          duration: 30s
          tool: get_active_incidents
          args: '{}'
          profile: alexa
          slo: |
            p95<500ms
            errors<1%
          # headers: |
          #   Authorization: Bearer ${{ secrets.MCP_TOKEN }}
```

The action installs the CLI with `actions/setup-go` and `go install` at the action's own ref. It runs the test,
writes the step summary, and uploads `junit.xml` and `report.json` as the `mcp-slo-report` artifact. Other
inputs: `rate`, `cold-wait`, `headers`, `artifact-name`, `go-version`. Output: `report` (path to the JSON
report).

## Non-goals

- **No schema validation.** Use MCP Inspector or another correctness checker to validate tool schemas and
  message shapes.
- **No UI rendering.** `mcp-slo` doesn't render or check tool output, MCP Apps, or other UI resources.
- **No certification checks.** Passing `--profile alexa` doesn't mean a server is Alexa-certified. It checks
  a few transport behaviors that are known to break.
- Not a general-purpose load generator. Stdio servers and the legacy HTTP+SSE transport aren't supported.

## Development

```sh
go vet ./...
go test -race ./...
```

Tests run against an in-process fake MCP server (`internal/mcptest`) that can answer with JSON or SSE,
require auth, return tool errors, or hang a GET stream. CI also runs the action itself against that server.

## License

[MIT](LICENSE)
