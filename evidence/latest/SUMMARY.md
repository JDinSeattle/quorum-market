# Test evidence — 2026-08-26T15-52-35Z

| | |
|---|---|
| Result | **PASS** |
| Commit | `3869d39` (3869d39) |
| Collected | 2026-08-26T15-52-35Z |
| Gates | 19 passed, 0 failed, 0 skipped |
| Host | Darwin 25.5.0 arm64, go1.26.6 |

## Gates

| Gate | Result | Output |
|---|---|---|
| environment | PASS | [`00-environment.txt`](00-environment.txt) |
| go mod tidy is current | PASS | [`01-tidy.txt`](01-tidy.txt) |
| go vet | PASS | [`02-vet.txt`](02-vet.txt) |
| golangci-lint | PASS | [`03-lint.txt`](03-lint.txt) |
| govulncheck | PASS | [`04-vulncheck.txt`](04-vulncheck.txt) |
| unit tests (verbose) | PASS | [`05-tests.txt`](05-tests.txt) |
| tests with the race detector | PASS | [`06-tests-race.txt`](06-tests-race.txt) |
| coverage | PASS | [`07-coverage.txt`](07-coverage.txt) |
| coverage report | PASS | [`08-coverage.html`](08-coverage.html) |
| build every service for linux/amd64 | PASS | [`09-build.txt`](09-build.txt) |
| terraform validate | PASS | [`10-terraform.txt`](10-terraform.txt) |
| compose configuration | PASS | [`11-compose.txt`](11-compose.txt) |
| running stack | PASS | [`12-stack.txt`](12-stack.txt) |
| smoke: commerce path | PASS | [`13-smoke-core.txt`](13-smoke-core.txt) |
| smoke: platform | PASS | [`14-smoke-platform.txt`](14-smoke-platform.txt) |
| metrics snapshot | PASS | [`15-metrics.txt`](15-metrics.txt) |
| system state | PASS | [`16-system-state.txt`](16-system-state.txt) |
| load test (60s) | PASS | [`17-loadtest.txt`](17-loadtest.txt) |
| local identity scrubbed | PASS | — |

## Coverage

```
total:										(statements)		51.0%
```

Per-package, highest first:

```
internal/gateway                      90.8%
internal/ratelimit                    86.6%
internal/cache                        86.1%
internal/cart                         82.2%
internal/httpx                        74.6%
internal/auth                         73.2%
internal/events                       70.9%
internal/product                      69.4%
internal/identity                     66.1%
internal/busywait                     66.0%
internal/order                        54.9%
internal/notification                 52.1%
internal/kv                           49.5%
internal/warehouse                    49.1%
internal/obs                          39.1%
internal/rmq                          18.6%
internal/cca                          16.7%
internal/redisx                        0.0%
internal/envx                          0.0%
cmd/warehousesvc                       0.0%
cmd/productsvc                         0.0%
cmd/productloader                      0.0%
cmd/ordersvc                           0.0%
cmd/notificationsvc                    0.0%
cmd/kvnode                             0.0%
cmd/identitysvc                        0.0%
cmd/healthcheck                        0.0%
cmd/gatewaysvc                         0.0%
cmd/ccasvc                             0.0%
cmd/cartsvc                            0.0%
```

## What the smoke tests proved

```
```

## Reproducing this

```bash
make up && make seed   # bring the stack up and fill the catalogue
make evidence          # rerun every gate and write a new directory here
```
