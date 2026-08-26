#!/usr/bin/env bash
#
# Runs every quality gate and records what actually happened.
#
# The point is a record that can be read later and trusted: each gate's full
# output, its exit code, and a summary that says FAIL when something failed.
# A report that only ever says PASS is not evidence of anything.
#
#   make evidence
#
# Gates that need a running stack are skipped, and recorded as skipped, when
# one is not up — a skipped check must never look like a passed one.

set -uo pipefail   # deliberately not -e: a failing gate is data, not a reason to stop

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

# Captured before anything is written.
#
# The script writes into the working tree as it runs, so reading the commit
# afterwards reports the tree as dirty and, if the run is later amended into a
# commit, records a hash that does not exist. Provenance has to be read while
# the tree is still what the gates are about to run against.
RUN_COMMIT="$(git rev-parse HEAD 2>/dev/null || echo unknown)"
RUN_SHORT="$(git rev-parse --short HEAD 2>/dev/null || echo unknown)"
RUN_DESCRIBE="$(git describe --tags --always --dirty 2>/dev/null || echo unknown)"
RUN_BRANCH="$(git rev-parse --abbrev-ref HEAD 2>/dev/null || echo unknown)"
RUN_DIRTY="$(git status --porcelain 2>/dev/null | wc -l | tr -d ' ')"

RUN_ID="$(date -u +%Y-%m-%dT%H-%M-%SZ)"
OUT="evidence/$RUN_ID"
mkdir -p "$OUT"

GATEWAY_URL="${GATEWAY_URL:-http://localhost:8080}"
COMPOSE="docker compose -f deploy/docker-compose.yml"

# Gate results, in the order they ran.
declare -a NAMES=() STATUSES=() FILES=() NOTES=()

blue()  { printf '\033[34m%s\033[0m\n' "$1"; }
green() { printf '\033[32m%s\033[0m\n' "$1"; }
red()   { printf '\033[31m%s\033[0m\n' "$1"; }

# record <name> <status> <file> <note>
record() {
  NAMES+=("$1"); STATUSES+=("$2"); FILES+=("$3"); NOTES+=("${4:-}")
  case "$2" in
    PASS) green "  PASS  $1" ;;
    FAIL) red   "  FAIL  $1" ;;
    *)    blue  "  SKIP  $1${4:+ — $4}" ;;
  esac
}

# gate <name> <output-file> <command...>
gate() {
  local name="$1" file="$2"; shift 2
  blue "→ $name"
  {
    echo "\$ $*"
    echo
  } > "$OUT/$file"

  if "$@" >> "$OUT/$file" 2>&1; then
    record "$name" PASS "$file"
  else
    local code=$?
    echo >> "$OUT/$file"
    echo "exit code: $code" >> "$OUT/$file"
    record "$name" FAIL "$file" "exit $code"
  fi
}

skip() { record "$1" SKIP "$2" "$3"; }

stack_is_up() { curl -fsS --max-time 3 "$GATEWAY_URL/health" >/dev/null 2>&1; }

have() { command -v "$1" >/dev/null 2>&1; }

printf '\n\033[1mCollecting evidence into %s\033[0m\n\n' "$OUT"

# ── Environment ──────────────────────────────────────────────────────────────
{
  echo "run id:      $RUN_ID"
  echo "commit:      $RUN_COMMIT"
  echo "describe:    $RUN_DESCRIBE"
  echo "branch:      $RUN_BRANCH"
  echo "dirty files: $RUN_DIRTY"
  echo
  echo "go:          $(go version)"
  echo "docker:      $(docker --version 2>/dev/null || echo 'not installed')"
  echo "os:          $(uname -srm)"
  echo "cpus:        $(getconf _NPROCESSORS_ONLN 2>/dev/null || echo unknown)"
  echo
  echo "── module ──"
  cat go.mod
  echo
  echo "── source size ──"
  printf 'go files:  %s\n' "$(find . -name '*.go' -not -path './.git/*' | wc -l | tr -d ' ')"
  printf 'go lines:  %s\n' "$(find . -name '*.go' -not -path './.git/*' -exec cat {} + | wc -l | tr -d ' ')"
  printf 'tests:     %s\n' "$(grep -rh '^func Test' --include='*_test.go' . | wc -l | tr -d ' ')"
  printf 'services:  %s\n' "$(find cmd -mindepth 1 -maxdepth 1 -type d | wc -l | tr -d ' ')"
} > "$OUT/00-environment.txt" 2>&1
record "environment" PASS "00-environment.txt"

# ── Static analysis ──────────────────────────────────────────────────────────
gate "go mod tidy is current" "01-tidy.txt" bash -c \
  'go mod tidy && git diff --exit-code -- go.mod go.sum'

gate "go vet" "02-vet.txt" go vet ./...

if have golangci-lint; then
  gate "golangci-lint" "03-lint.txt" golangci-lint run ./...
else
  skip "golangci-lint" "03-lint.txt" "not installed"
fi

if have govulncheck; then
  gate "govulncheck" "04-vulncheck.txt" govulncheck ./...
else
  skip "govulncheck" "04-vulncheck.txt" "not installed"
fi

# ── Tests ────────────────────────────────────────────────────────────────────
gate "unit tests (verbose)" "05-tests.txt" go test ./... -count=1 -v

# The race detector is the point of this one: much of the system is concurrent,
# and a data race there passes every deterministic test and then corrupts
# inventory under load.
gate "tests with the race detector" "06-tests-race.txt" go test ./... -race -count=1

gate "coverage" "07-coverage.txt" bash -c \
  "go test ./... -count=1 -coverprofile='$OUT/coverage.out' -covermode=atomic >/dev/null && go tool cover -func='$OUT/coverage.out'"

if [ -f "$OUT/coverage.out" ]; then
  go tool cover -html="$OUT/coverage.out" -o "$OUT/08-coverage.html" 2>/dev/null \
    && record "coverage report" PASS "08-coverage.html"
fi

# ── Build ────────────────────────────────────────────────────────────────────
# Listed by name and size rather than with 'ls -l', whose owner and group
# columns leak whoever happened to run the build into a file that is committed.
gate "build every service for linux/amd64" "09-build.txt" bash -c \
  'GOOS=linux GOARCH=amd64 go build -trimpath -o /tmp/evidence-bin/ ./cmd/... \
     && for f in /tmp/evidence-bin/*; do printf "%-18s %10d bytes\n" "$(basename "$f")" "$(wc -c < "$f")"; done \
     && rm -rf /tmp/evidence-bin'

# ── Infrastructure ───────────────────────────────────────────────────────────
if have terraform || have tofu; then
  TF="$(command -v terraform || command -v tofu)"
  gate "terraform validate" "10-terraform.txt" bash -c \
    "cd terraform && '$TF' fmt -recursive -check && '$TF' init -backend=false >/dev/null && '$TF' validate && rm -rf .terraform .terraform.lock.hcl"
else
  skip "terraform validate" "10-terraform.txt" "neither terraform nor tofu installed"
fi

gate "compose configuration" "11-compose.txt" bash -c \
  "$COMPOSE --profile observability --profile seed config >/dev/null && $COMPOSE config --services | sort"

# ── Live stack ───────────────────────────────────────────────────────────────
if stack_is_up; then
  {
    echo "── containers ──"
    $COMPOSE --profile observability ps --format '{{.Name}}\t{{.Status}}' 2>/dev/null | sort
  } > "$OUT/12-stack.txt" 2>&1
  record "running stack" PASS "12-stack.txt"

  gate "smoke: commerce path" "13-smoke-core.txt" ./scripts/smoke.sh
  gate "smoke: platform" "14-smoke-platform.txt" ./scripts/smoke-platform.sh

  # ── Snapshots: what the system was actually doing ──────────────────────────
  {
    for entry in \
      "gateway 9217" "product 9210" "cart 9213" "warehouse 9212" \
      "cca 9211" "identity 9214" "order 9215" "notification 9216"; do
      set -- $entry
      echo "── $1 (:$2) ──"
      curl -fsS --max-time 3 "http://localhost:$2/metrics" 2>/dev/null \
        | grep -E '^quorum_' | grep -v '_bucket' || echo "(unreachable)"
      echo
    done
  } > "$OUT/15-metrics.txt" 2>&1
  record "metrics snapshot" PASS "15-metrics.txt"

  {
    echo "── readiness ──"
    for entry in \
      "gateway 9217" "product 9210" "cart 9213" "warehouse 9212" \
      "cca 9211" "identity 9214" "order 9215" "notification 9216"; do
      set -- $entry
      printf '%-14s ' "$1"
      curl -fsS --max-time 3 "http://localhost:$2/readyz" 2>/dev/null || echo "(unreachable)"
      echo
    done

    echo
    echo "── database clusters ──"
    for port in 9080 9081 9082 9083 9084 9090 9091 9092 9093 9094 9095 9096 9097; do
      printf ':%s ' "$port"
      curl -fsS --max-time 3 "http://localhost:$port/kv/stats" 2>/dev/null || echo "(unreachable)"
      echo
    done

    echo
    echo "── warehouse ledger ──"
    curl -fsS --max-time 3 http://localhost:8084/warehouse/stats 2>/dev/null
    echo

    echo
    echo "── redis keyspace ──"
    docker exec redis redis-cli info keyspace 2>/dev/null || echo "(unreachable)"
    docker exec redis redis-cli --scan --pattern '*' 2>/dev/null \
      | sed 's/:[^:]*$/:*/' | sort | uniq -c | sort -rn | head -20 || true

    echo
    echo "── queues ──"
    curl -fsS --max-time 3 -u guest:guest \
      'http://localhost:15672/api/queues/%2F?columns=name,messages,messages_ready,consumers' 2>/dev/null \
      | python3 -m json.tool 2>/dev/null || echo "(unreachable)"
  } > "$OUT/16-system-state.txt" 2>&1
  record "system state" PASS "16-system-state.txt"

  if have locust; then
    gate "load test (60s)" "17-loadtest.txt" bash -c \
      "locust -f loadtest/locustfile.py --host '$GATEWAY_URL' --headless \
        --users '${LOAD_USERS:-20}' --spawn-rate 5 --run-time '${LOAD_TIME:-60s}' --only-summary"
  else
    skip "load test" "17-loadtest.txt" "locust not installed (pip install -r loadtest/requirements.txt)"
  fi
else
  for entry in \
    "running stack|12-stack.txt" \
    "smoke: commerce path|13-smoke-core.txt" \
    "smoke: platform|14-smoke-platform.txt" \
    "metrics snapshot|15-metrics.txt" \
    "system state|16-system-state.txt" \
    "load test|17-loadtest.txt"; do
    skip "${entry%%|*}" "${entry##*|}" "no running stack (run 'make up && make seed')"
  done
fi

rm -f "$OUT/coverage.out"

# ── Scrub local identity ─────────────────────────────────────────────────────
#
# These files are committed, so the machine that produced them should not be
# identifiable from them. Tool output picks up the running user and the
# hostname in places that are not worth predicting one at a time — Locust
# stamps the hostname on every log line, `ls -l` prints the file owner, and
# temporary paths carry the home directory.
blue "→ scrubbing local identity"
python3 - "$OUT" "$(id -un 2>/dev/null || echo '')" "$(hostname -s 2>/dev/null || echo '')" "$HOME" <<'SCRUB'
import pathlib, sys

out, user, host, home = sys.argv[1], sys.argv[2], sys.argv[3], sys.argv[4]

# Longest first, so a hostname containing the username is replaced as a whole
# rather than leaving a fragment behind.
replacements = sorted(
    [(v, tag) for v, tag in ((home, "$HOME"), (host, "HOST"), (user, "USER")) if v],
    key=lambda pair: len(pair[0]),
    reverse=True,
)

scrubbed = 0
for path in pathlib.Path(out).iterdir():
    if not path.is_file():
        continue
    try:
        text = path.read_text(errors="ignore")
    except OSError:
        continue

    original = text
    for value, tag in replacements:
        # Case-insensitively, because tools capitalise hostnames differently
        # from how the shell reports them.
        lowered, cursor, pieces = text.lower(), 0, []
        needle = value.lower()
        while (idx := lowered.find(needle, cursor)) != -1:
            pieces.append(text[cursor:idx])
            pieces.append(tag)
            cursor = idx + len(needle)
        if pieces:
            pieces.append(text[cursor:])
            text = "".join(pieces)
            lowered = text.lower()

    if text != original:
        path.write_text(text)
        scrubbed += 1

print(f"  rewrote {scrubbed} file(s)")
SCRUB
record "local identity scrubbed" PASS ""

# ── Summary ──────────────────────────────────────────────────────────────────
passed=0; failed=0; skipped=0
for status in "${STATUSES[@]}"; do
  case "$status" in
    PASS) passed=$((passed + 1)) ;;
    FAIL) failed=$((failed + 1)) ;;
    *)    skipped=$((skipped + 1)) ;;
  esac
done

overall="PASS"
[ "$failed" -gt 0 ] && overall="FAIL"

{
  echo "# Test evidence — $RUN_ID"
  echo
  echo "| | |"
  echo "|---|---|"
  echo "| Result | **$overall** |"
  echo "| Commit | \`$RUN_SHORT\` ($RUN_DESCRIBE) |"
  echo "| Collected | $RUN_ID |"
  echo "| Gates | $passed passed, $failed failed, $skipped skipped |"
  echo "| Host | $(uname -srm), $(go version | awk '{print $3}') |"
  echo
  echo "## Gates"
  echo
  echo "| Gate | Result | Output |"
  echo "|---|---|---|"
  for i in "${!NAMES[@]}"; do
    note=""
    [ -n "${NOTES[$i]}" ] && note=" <br><sub>${NOTES[$i]}</sub>"
    if [ -f "$OUT/${FILES[$i]}" ]; then
      echo "| ${NAMES[$i]} | ${STATUSES[$i]}$note | [\`${FILES[$i]}\`](${FILES[$i]}) |"
    else
      echo "| ${NAMES[$i]} | ${STATUSES[$i]}$note | — |"
    fi
  done
  echo

  if [ -f "$OUT/07-coverage.txt" ]; then
    echo "## Coverage"
    echo
    echo '```'
    tail -1 "$OUT/07-coverage.txt"
    echo '```'
    echo
    echo "Per-package, highest first:"
    echo
    echo '```'
    python3 - "$OUT/07-coverage.txt" <<'PY'
import re, sys, collections
totals = collections.defaultdict(lambda: [0.0, 0])
for line in open(sys.argv[1]):
    m = re.match(r'(\S+\.go):\d+:\s+\S+\s+([\d.]+)%', line)
    if not m:
        continue
    pkg = m.group(1).rsplit('/', 1)[0].replace('github.com/JDinSeattle/quorum-market/', '')
    totals[pkg][0] += float(m.group(2))
    totals[pkg][1] += 1
rows = sorted(((s / n, p) for p, (s, n) in totals.items() if n), reverse=True)
for pct, pkg in rows:
    print(f'{pkg:36} {pct:5.1f}%')
PY
    echo '```'
    echo
  fi

  if [ -f "$OUT/13-smoke-core.txt" ] || [ -f "$OUT/14-smoke-platform.txt" ]; then
    echo "## What the smoke tests proved"
    echo
    echo '```'
    grep -hE '^  (ok|FAIL)' "$OUT/13-smoke-core.txt" "$OUT/14-smoke-platform.txt" 2>/dev/null \
      | sed 's/\x1b\[[0-9;]*m//g'
    echo '```'
    echo
  fi

  echo "## Reproducing this"
  echo
  echo '```bash'
  echo "make up && make seed   # bring the stack up and fill the catalogue"
  echo "make evidence          # rerun every gate and write a new directory here"
  echo '```'
} > "$OUT/SUMMARY.md"

# A stable path to the most recent run, so links and scripts do not chase
# timestamps.
rm -rf evidence/latest
cp -R "$OUT" evidence/latest

printf '\n'
if [ "$failed" -gt 0 ]; then
  red "$failed gate(s) failed — see $OUT/SUMMARY.md"
else
  green "all $passed gate(s) passed — see $OUT/SUMMARY.md"
fi
[ "$skipped" -gt 0 ] && blue "$skipped skipped"
printf '\n'

exit "$failed"
