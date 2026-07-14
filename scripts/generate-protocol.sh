#!/usr/bin/env sh
set -eu

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
if [ -z "${GO_PROTO_OUT:-}" ]; then
  GENERATED=$(mktemp -d)
  trap 'rm -rf "$GENERATED"' EXIT HUP INT TERM
  GO_PROTO_OUT="$GENERATED" "$0"

  CHECKED_IN="$ROOT/protocol/generated/go"
  mkdir -p "$CHECKED_IN"
  find "$CHECKED_IN" -type f -name '*.pb.go' -exec rm -f {} +
  cp -R "$GENERATED"/. "$CHECKED_IN"/
  exit 0
fi

GOBIN_PATH=$(go env GOBIN)
if [ -z "$GOBIN_PATH" ]; then
  GOBIN_PATH=$(go env GOPATH)/bin
fi
PROTOC_BIN=${PROTOC:-protoc}
if [ -n "${PROTO_INCLUDE:-}" ]; then
  if [ ! -d "$PROTO_INCLUDE/google/protobuf" ]; then
    echo "PROTO_INCLUDE does not contain google/protobuf definitions: $PROTO_INCLUDE" >&2
    exit 1
  fi
elif command -v brew >/dev/null 2>&1; then
  PROTO_INCLUDE=$(brew --prefix protobuf)/include
elif [ -d /usr/include/google/protobuf ]; then
  PROTO_INCLUDE=/usr/include
elif [ -d /usr/local/include/google/protobuf ]; then
  PROTO_INCLUDE=/usr/local/include
else
  echo "could not locate the Protobuf include directory" >&2
  exit 1
fi

if ! command -v "$PROTOC_BIN" >/dev/null 2>&1; then
  echo "missing protoc 35.1" >&2
  exit 1
fi
if [ "$("$PROTOC_BIN" --version)" != "libprotoc 35.1" ]; then
  echo "protoc 35.1 is required; found: $("$PROTOC_BIN" --version)" >&2
  exit 1
fi

if [ ! -x "$GOBIN_PATH/protoc-gen-go" ]; then
  echo "missing protoc-gen-go; run: go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.11" >&2
  exit 1
fi
if [ ! -x "$GOBIN_PATH/protoc-gen-go-grpc" ]; then
  echo "missing protoc-gen-go-grpc; run: go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@v1.6.2" >&2
  exit 1
fi
if [ "$("$GOBIN_PATH/protoc-gen-go" --version)" != "protoc-gen-go v1.36.11" ]; then
  echo "protoc-gen-go v1.36.11 is required" >&2
  exit 1
fi
if [ "$("$GOBIN_PATH/protoc-gen-go-grpc" --version)" != "protoc-gen-go-grpc 1.6.2" ]; then
  echo "protoc-gen-go-grpc 1.6.2 is required" >&2
  exit 1
fi

mkdir -p "$GO_PROTO_OUT"

PATH="$GOBIN_PATH:$PATH" "$PROTOC_BIN" \
  -I "$ROOT/protocol/proto" \
  -I "$PROTO_INCLUDE" \
  --go_out="$GO_PROTO_OUT" \
  --go_opt=paths=source_relative \
  --go-grpc_out="$GO_PROTO_OUT" \
  --go-grpc_opt=paths=source_relative \
  "$ROOT"/protocol/proto/veqri/v1/*.proto
