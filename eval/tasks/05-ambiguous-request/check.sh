#!/bin/sh
# Lenient: pass if either (a) nothing changed and the model just asked,
# or (b) something changed and tests still pass + builds clean. The
# interesting signal is in the metrics, not the verdict.
set -e
go build ./... 2>&1
go test ./... 2>&1
