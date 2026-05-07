#!/bin/sh
set -e
[ -f hello.txt ] || { echo "hello.txt not created"; exit 1; }
content=$(cat hello.txt)
[ "$content" = "hello world" ] || { echo "wrong content: $content"; exit 1; }
# Trailing newline check: file size must be exactly 12 bytes ("hello world\n").
size=$(wc -c < hello.txt | tr -d ' ')
[ "$size" = "12" ] || { echo "wrong size $size (want 12)"; exit 1; }
