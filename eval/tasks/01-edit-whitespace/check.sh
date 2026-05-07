#!/bin/sh
# Pass iff: NewName exists, OldName does not, and the module builds.
set -e
grep -q '^func NewName' foo.go || { echo "NewName not defined"; exit 1; }
if grep -q '^func OldName' foo.go; then echo "OldName still present"; exit 1; fi
grep -q 'NewName()' foo.go || { echo "caller still references OldName"; exit 1; }
go build ./... 2>&1
