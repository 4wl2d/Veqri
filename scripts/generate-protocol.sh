#!/usr/bin/env sh
set -eu

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
GOBIN_PATH=$(go env GOPATH)/bin
if command -v brew >/dev/null 2>&1; then
  PROTO_INCLUDE=$(brew --prefix protobuf)/include
elif [ -d /usr/include/google/protobuf ]; then
  PROTO_INCLUDE=/usr/include
elif [ -d /usr/local/include/google/protobuf ]; then
  PROTO_INCLUDE=/usr/local/include
else
  echo "could not locate the Protobuf include directory" >&2
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

PATH="$GOBIN_PATH:$PATH" protoc \
  -I "$ROOT/protocol/proto" \
  -I "$PROTO_INCLUDE" \
  --go_out="$ROOT/protocol/generated/go" \
  --go_opt=paths=source_relative \
  --go-grpc_out="$ROOT/protocol/generated/go" \
  --go-grpc_opt=paths=source_relative \
  "$ROOT"/protocol/proto/veqri/v1/*.proto
