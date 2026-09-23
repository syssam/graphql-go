#!/bin/sh
# PostToolUse(Edit|Write): gofmt the Go file Claude just wrote. CI fails on
# unformatted code, so formatting here is cheaper than finding out there.
# No jq dependency: only file_path is needed, and JSON escapes a Windows
# path's backslashes, which the second sed undoes.
f=$(sed -n 's/.*"file_path"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -n 1 | sed 's/\\\\/\\/g')
case "$f" in
  *.go) [ -f "$f" ] && gofmt -w "$f" ;;
esac
exit 0
