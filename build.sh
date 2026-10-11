#!/usr/bin/env bash
set -e
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

echo "Building romtool statically with Go..."
cd "$DIR/romtool"
CGO_ENABLED=0 go build -ldflags="-s -w" -o "$DIR/bin/romtool" ./cmd/romtool
chmod +x "$DIR/bin/romtool"

echo "Build successful: $DIR/bin/romtool"
"$DIR/bin/romtool" doctor
