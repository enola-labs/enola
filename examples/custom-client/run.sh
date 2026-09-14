#!/bin/sh
# Teach enola an in-house HTTP client through config, and watch the dependency it hid
# appear in the graph. Runs in a temporary copy, so the example files stay unchanged.
set -e
cd "$(dirname "$0")"

ENOLA="${ENOLA:-enola}"
case "$ENOLA" in
  */*) ENOLA="$(cd "$(dirname "$ENOLA")" && pwd)/$(basename "$ENOLA")" ;;
esac
if ! command -v "$ENOLA" >/dev/null 2>&1; then
  echo "enola not found on PATH. Build it first:" >&2
  echo "    go build -o enola ./cmd/enola   # from the repository root" >&2
  echo "then re-run with:  ENOLA=../../enola ./run.sh" >&2
  exit 1
fi

DEMO="$(mktemp -d)"
trap 'rm -rf "$DEMO"' EXIT
cp -R . "$DEMO/custom-client"
cd "$DEMO/custom-client"

# The cross-repo edges of the last snapshot. A cluster writes its linked graph into the
# last repository it lists, and facts.jsonl escapes the ">" in an edge name as a
# unicode escape.
edges() {
  grep -o '"kind":"dependency","name":"[a-z-]* -\\u003e [a-z-]*"' backend/.enola/facts.jsonl \
    | sed -e 's/.*"name":"//' -e 's/"$//' -e 's/\\u003e/>/' | sort -u | sed 's/^/    /'
}

echo "########## 1. No client declared"
"$ENOLA" --generate cluster.yaml >/dev/null 2>&1
echo "Cross-repo edges:"
edges
echo
"$ENOLA" coverage cluster.yaml 2>/dev/null

echo
echo "########## 2. The client declared (cluster-with-client.yaml)"
"$ENOLA" --generate cluster-with-client.yaml >/dev/null 2>&1
echo "Cross-repo edges:"
edges
echo
"$ENOLA" coverage cluster-with-client.yaml 2>/dev/null

echo
echo "########## 3. Plus literal-against-parameter matching (cluster-with-params.yaml)"
"$ENOLA" --generate cluster-with-params.yaml >/dev/null 2>&1
echo "Cross-repo edges:"
edges
echo
"$ENOLA" coverage cluster-with-params.yaml 2>/dev/null

echo
echo "########## 4. Who calls the gateway's resource route"
"$ENOLA" endpoint 'GET /v1/resources' cluster-with-params.yaml 2>/dev/null

echo
echo "==> See README.md for what each step shows and what enola still cannot read."
