#!/bin/sh
set -e
grep -q '^func NewName' foo.go || { echo "NewName not defined"; exit 1; }
if grep -q '^func OldName' foo.go; then echo "OldName still present"; exit 1; fi
go build ./... 2>&1
