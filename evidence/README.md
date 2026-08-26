# Test evidence

Recorded output from every quality gate, so that "it works" is a claim someone
else can check rather than one they have to take on trust.

Each run writes a timestamped directory containing the full output of every
gate — not a summary of it — plus its exit code. [`latest/`](latest/) is a copy
of the most recent run.

```bash
make up && make seed   # gates that need a running stack are skipped without one
make evidence
```

Start with `SUMMARY.md` in any run directory.

## What is recorded

| File | Gate |
|---|---|
| `SUMMARY.md` | Pass/fail table, coverage, and what the smoke tests proved |
| `00-environment.txt` | Commit, toolchain, host, module graph, source size |
| `01-tidy.txt` | `go.mod` and `go.sum` are current |
| `02-vet.txt` | `go vet` |
| `03-lint.txt` | `golangci-lint` |
| `04-vulncheck.txt` | `govulncheck` — known vulnerabilities in reachable code |
| `05-tests.txt` | Every test, verbose, so each assertion is named |
| `06-tests-race.txt` | The same suite under the race detector |
| `07-coverage.txt` | Per-function coverage |
| `08-coverage.html` | Annotated source. Generated locally, not committed — GitHub will not render it and it is the largest file here |
| `09-build.txt` | Every service cross-compiled for `linux/amd64` |
| `10-terraform.txt` | Infrastructure formatting and validation |
| `11-compose.txt` | Compose configuration and service list |
| `12-stack.txt` | Container health at collection time |
| `13-smoke-core.txt` | Commerce path, end to end, against the running stack |
| `14-smoke-platform.txt` | Gateway, auth, cache, events, order history, notifications |
| `15-metrics.txt` | Every service's domain metrics at collection time |
| `16-system-state.txt` | Readiness, database clusters, ledger, Redis keyspace, queues |
| `17-loadtest.txt` | Locust run, when Locust is installed |

## How to read it

**A skip is not a pass.** Gates that need something absent — a running stack, a
linter that is not installed — are recorded as `SKIP` with the reason. They are
counted separately in the summary and never fold into the pass count.

**A failing gate does not stop the run.** Every gate runs regardless, so one
failure produces a complete picture instead of hiding whatever came after it.
The summary says `FAIL` and the script exits non-zero.

**The interesting files are the smoke tests.** Unit tests prove the pieces;
`13-` and `14-` prove the assembled system does what it claims — that stock is
deducted exactly once, that a retried checkout does not charge twice, that one
customer cannot read another's cart, and that an order reaches the order
service and the notification service purely through events.

## Why runs are kept

The timestamped directories are committed rather than ignored. A green CI badge
says the tests passed at some point on someone's machine; a recorded run says
which commit, on what, with what output, and what was skipped. When something
regresses, the last good run is the fastest way to see what changed.
