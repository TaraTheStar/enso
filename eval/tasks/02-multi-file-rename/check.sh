#!/bin/sh
set -e
# No file may still contain Greet; SayHello must appear in all three.
if grep -rn '\bGreet\b' --include='*.go' . ; then
  echo "Greet still present somewhere"; exit 1
fi
grep -q 'SayHello' greeter/greeter.go || { echo "definition not renamed"; exit 1; }
grep -q 'SayHello' server/handler.go  || { echo "server caller not renamed"; exit 1; }
grep -q 'SayHello' cmd/main.go        || { echo "main caller not renamed"; exit 1; }
go build ./... 2>&1
