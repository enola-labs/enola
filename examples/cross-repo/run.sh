#!/bin/sh
# Indexes both services into one graph and shows what enola resolved between them.
#
# The point to watch for: api/server.go registers "/orders/{id}", but the route is
# STORED at "/api/v2/orders/{id}" — the prefix was composed across a function
# boundary. That composed path is why the web service's call resolves at all.
set -e
cd "$(dirname "$0")"

ENOLA="${ENOLA:-enola}"
if ! command -v "$ENOLA" >/dev/null 2>&1; then
  echo "enola not found on PATH. Build it first:" >&2
  echo "    go build -o enola ./cmd/enola   # from the repository root" >&2
  echo "then re-run with:  ENOLA=../../enola ./run.sh" >&2
  exit 1
fi

echo "==> Indexing both services into one graph"
"$ENOLA" --generate cluster.yaml >/dev/null 2>&1

echo
echo "==> Routes the api service actually serves"
echo "    (registered as \"/orders/{id}\" — stored at the composed runtime path)"
grep -h '"file":"api/' api/.enola/facts.jsonl | grep -o '"name":"/api/v2[^"]*"' | sort -u | sed 's/"name":/    /'

echo
echo "==> Cross-repo edge coverage"
"$ENOLA" coverage cluster.yaml 2>/dev/null

echo "==> Read README.md for why the unresolved one is deliberate."
