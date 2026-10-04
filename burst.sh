#!/bin/sh
# One-command on-sale stampede. Requires Go.
# Usage: ./burst.sh BASE_URL [extra flags passed to cmd/burst]
set -eu
if [ "$#" -lt 1 ]; then
  echo "usage: ./burst.sh BASE_URL [burst flags]" >&2
  exit 2
fi
base=$1
shift
exec go run ./cmd/burst --base-url "$base" "$@"
