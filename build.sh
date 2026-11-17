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
out=dist/modbus-slave-windows-amd64.exe
# the icon and the version info of the .exe (x.y.z of the tag)
rv=$(echo "$VERSION" | sed -n 's/^v\{0,1\}\([0-9]*\.[0-9]*\.[0-9]*\).*/\1/p')
go run ./tools/mkres -ico assets/napi.ico -version "${rv:-0.0.0}" -o cmd/modbus-slave/rsrc_windows_amd64.syso
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "-s -w -X main.version=$VERSION" -o "$out" ./cmd/modbus-slave
echo "$out"
