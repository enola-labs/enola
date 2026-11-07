#!/usr/bin/env bash
# Regenerates this directory's parser from upstream tree-sitter-c-sharp plus patches/,
# or with --check verifies that doing so reproduces the committed files byte for byte.
# Needs git, node (npx) and network access. See ATTRIBUTION.md.
set -euo pipefail

UPSTREAM=https://github.com/tree-sitter/tree-sitter-c-sharp
COMMIT=8c0abe0b84a3
CLI=tree-sitter-cli@0.26.5

here=$(cd "$(dirname "$0")" && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

git clone -q "$UPSTREAM" "$work/ts"
git -C "$work/ts" checkout -q "$COMMIT"
for p in "$here"/patches/*.patch; do
  git -C "$work/ts" apply "$p"
done
(cd "$work/ts" && npx -y "$CLI" generate --abi 15 >/dev/null && npx -y "$CLI" test >/dev/null)

files=(grammar.json:src/grammar.json src/parser.c:src/parser.c src/node-types.json:src/node-types.json
  src/scanner.c:src/scanner.c src/tree_sitter/alloc.h:src/tree_sitter/alloc.h
  src/tree_sitter/array.h:src/tree_sitter/array.h src/tree_sitter/parser.h:src/tree_sitter/parser.h)

if [[ "${1:-}" == "--check" ]]; then
  status=0
  for f in "${files[@]}"; do
    if ! cmp -s "$work/ts/${f#*:}" "$here/${f%%:*}"; then
      echo "differs: ${f%%:*}"
      status=1
    fi
  done
  [[ $status == 0 ]] && echo "reproduced: upstream $COMMIT + $(ls "$here"/patches/*.patch | wc -l | tr -d ' ') patches"
  exit $status
fi

for f in "${files[@]}"; do
  cp "$work/ts/${f#*:}" "$here/${f%%:*}"
done
echo "regenerated: upstream $COMMIT + patches; build with go build -a"
