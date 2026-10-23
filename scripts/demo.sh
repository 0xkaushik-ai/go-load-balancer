#!/usr/bin/env bash
# Starts three demo backends and the load balancer in front of them.
# Press Ctrl+C to stop everything.
#
# Usage: ./scripts/demo.sh [round-robin|least-conn]
set -euo pipefail

cd "$(dirname "$0")/.."
STRATEGY="${1:-round-robin}"

go build -o bin/backend ./cmd/backend
go build -o bin/lb ./cmd/lb

pids=()
cleanup() {
  echo
  echo "stopping..."
  kill "${pids[@]}" 2>/dev/null || true
  wait 2>/dev/null || true
}
trap cleanup EXIT INT TERM

./bin/backend -addr 127.0.0.1:9101 -name one   & pids+=($!)
./bin/backend -addr 127.0.0.1:9102 -name two   & pids+=($!)
./bin/backend -addr 127.0.0.1:9103 -name three -delay 500ms & pids+=($!)

./bin/lb -addr 127.0.0.1:8000 -strategy "$STRATEGY" -health-interval 2s \
  -backends http://127.0.0.1:9101,http://127.0.0.1:9102,http://127.0.0.1:9103 &
pids+=($!)

cat <<'EOF'

Load balancer running on http://127.0.0.1:8000 (backend "three" is slow).
Try, in another terminal:
  curl -s 127.0.0.1:8000/hello
  for i in $(seq 6); do curl -s 127.0.0.1:8000/; done
  curl -s 127.0.0.1:8000/lb/status
Then kill one backend (e.g. kill the "two" process) and watch failover.

EOF
wait
