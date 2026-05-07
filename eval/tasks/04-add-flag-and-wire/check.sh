#!/bin/sh
set -e
go build ./... 2>&1
go test ./... 2>&1

# Smoke: --verbose prints the marker; default does not.
out_v=$(./flagcli --verbose --greeting hi 2>&1)
out_d=$(./flagcli --greeting hi 2>&1)

echo "$out_v" | grep -q '^verbose: on$' || { echo "missing 'verbose: on' with --verbose"; echo "got: $out_v"; exit 1; }
echo "$out_v" | grep -q '^hi$'           || { echo "missing greeting in verbose output"; exit 1; }
if echo "$out_d" | grep -q 'verbose'; then echo "default run leaks 'verbose'"; exit 1; fi
echo "$out_d" | grep -q '^hi$'           || { echo "missing greeting in default output"; exit 1; }
