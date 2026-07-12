#!/bin/sh
set -eu

repo_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
cd "$repo_dir"

go build -trimpath -ldflags '-X main.version=v0.1.6' -o agent-history ./cmd/agent-history

exec ./agent-history serve \
  --bind 127.0.0.1:54321 \
  --no-url-token \
  --no-open \
  --analyze-pending \
  --scan-on-start=true \
  --scan-interval=15m
