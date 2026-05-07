#!/bin/sh
set -e
go build -o swaptest . 2>&1
out_default=$(./swaptest 2>&1)
out_named=$(./swaptest --name Sam 2>&1)
echo "$out_default" | grep -q '^Hello, world!$' || { echo "default greeting wrong: $out_default"; exit 1; }
echo "$out_named"   | grep -q '^Hello, Sam!$'   || { echo "named greeting wrong: $out_named"; exit 1; }
