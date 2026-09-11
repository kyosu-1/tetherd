#!/usr/bin/env bash
# Root-required macOS e2e. Proves pf capture + natlook + yamux dial end to end
# without AWS. Run: make e2e-local   (asks for sudo once)
set -euo pipefail
cd "$(dirname "$0")/.."

SOCK=/tmp/tetherd-e2e.sock
AGENT=127.0.0.1:9900
URL=http://192.0.2.11:8081/
EXEC=/usr/local/libexec/tetherd/tetherd-exec
RUN=(./bin/tetherd run --transport direct --agent-addr "$AGENT" --remote-cidr 192.0.2.0/24
     --helper-socket "$SOCK" --exec-path "$EXEC" --user e2e --)

pass=0; fail=0
check() { # check <name> <expected-substring> <cmd...>
  local name=$1 want=$2; shift 2
  local out
  if out=$("$@" 2>&1) && [[ "$out" == *"$want"* ]]; then
    echo "  ✓ $name"; pass=$((pass+1))
  else
    echo "  ✗ $name"; echo "$out" | sed 's/^/      /'; fail=$((fail+1))
  fi
}

cleanup() {
  set +e
  if [[ -n "${HELPER_PID:-}" ]]; then sudo kill "$HELPER_PID" 2>/dev/null; wait "$SUDO_PID" 2>/dev/null; fi
  docker compose -f hack/docker-compose.yml down -v >/dev/null 2>&1
}
trap cleanup EXIT

echo "== build"
make build >/dev/null
echo "== containers"
docker compose -f hack/docker-compose.yml up -d --build --wait
for _ in $(seq 1 50); do nc -z 127.0.0.1 9900 2>/dev/null && break; sleep 0.1; done
echo "== helper (sudo)"
sudo -v
sudo ./bin/tetherd-helper --socket "$SOCK" --exec-src "$PWD/bin/tetherd-exec" &
SUDO_PID=$!
for _ in $(seq 1 50); do [[ -S "$SOCK" ]] && break; sleep 0.1; done
[[ -S "$SOCK" ]] || { echo "helper socket did not appear"; exit 1; }
HELPER_PID=$(pgrep -n -f "bin/tetherd-helper --socket $SOCK" || true)
[[ -n "$HELPER_PID" ]] || HELPER_PID=$SUDO_PID

echo "== checks"
if curl -s --max-time 2 "$URL" >/dev/null 2>&1; then
  echo "  ! $URL is reachable without tetherd on this Docker; relying on 'from 192.0.2.10' checks"
fi
check "curl via agent"        "from 192.0.2.10" "${RUN[@]}" curl -s --max-time 5 "$URL"
check "bash child inherits"   "from 192.0.2.10" "${RUN[@]}" bash -c "curl -s --max-time 5 $URL"
check "zsh child inherits"    "from 192.0.2.10" "${RUN[@]}" zsh -c "curl -s --max-time 5 $URL"
check "go child (go run)"     "from 192.0.2.10" "${RUN[@]}" go run ./hack/e2echeck "$URL"
if command -v node >/dev/null; then
  check "node child" "from 192.0.2.10" "${RUN[@]}" node -e "fetch('$URL').then(r=>r.text()).then(t=>process.stdout.write(t))"
fi
if command -v psql >/dev/null; then
  check "psql to postgres" "1 row" "${RUN[@]}" psql "postgres://postgres:tetherd@192.0.2.12/postgres" -c 'select 1'
fi
rc=0; "${RUN[@]}" sh -c 'exit 7' || rc=$?
if [[ $rc -eq 7 ]]; then echo "  ✓ exit code propagates"; pass=$((pass+1)); else echo "  ✗ exit code propagates (rc=$rc)"; fail=$((fail+1)); fi
check "no-network still runs" "hi" ./bin/tetherd run --transport direct --agent-addr "$AGENT" --no-network --user e2e -- echo hi

echo "== pf state after sessions (must be empty)"
sudo pfctl -a com.apple/900.tetherd -sr 2>/dev/null | sed 's/^/  /' || true

echo
echo "passed=$pass failed=$fail"
[[ $fail -eq 0 ]]
