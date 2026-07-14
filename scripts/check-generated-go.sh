#!/usr/bin/env sh
set -eu

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
GENERATED=$(mktemp -d)
trap 'rm -rf "$GENERATED"' EXIT HUP INT TERM

GO_PROTO_OUT="$GENERATED" "$ROOT/scripts/generate-protocol.sh"

if ! diff -qr "$ROOT/protocol/generated/go" "$GENERATED"; then
  echo "checked-in Go protocol bindings are stale; run make generate-go" >&2
  exit 1
fi

go test "$ROOT/protocol/generated/go/..."
