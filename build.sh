#!/bin/sh
# Static binaries into dist/: linux amd64 / arm64 / armv7. Needs Go >= 1.24.
set -e
cd "$(dirname "$0")"
VERSION=${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}
mkdir -p dist
for t in linux/amd64 linux/arm64 linux/arm; do
	os=${t%/*}; arch=${t#*/}
	out=dist/modbus-slave-$os-$arch
	CGO_ENABLED=0 GOOS=$os GOARCH=$arch GOARM=7 go build -trimpath -ldflags "-s -w -X main.version=$VERSION" -o "$out" ./cmd/modbus-slave
	echo "$out"
done
