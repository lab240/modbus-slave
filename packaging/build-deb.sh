#!/bin/sh
# Debian packages from the dist/ binaries: dist/modbus-slave_<ver>_<arch>.deb (amd64, arm64, armhf).
#   ./build.sh && packaging/build-deb.sh
set -e
cd "$(dirname "$0")/.."
VER=${DEB_VERSION:-$(git describe --tags --match 'v2*' 2>/dev/null | sed 's/^v//; s/-/+/g')}
[ -n "$VER" ] || VER="2.0.0~dev+$(git log -1 --format=%cd.%h --date=format:%Y%m%d)"
for pair in amd64:amd64 arm64:arm64 arm:armhf; do
	goarch=${pair%:*}; arch=${pair#*:}
	bin=dist/modbus-slave-linux-$goarch
	[ -x "$bin" ] || { echo "no $bin, run ./build.sh first" >&2; exit 1; }
	root=$(mktemp -d)
	chmod 0755 "$root"
	install -D -m 0755 "$bin" "$root/usr/bin/modbus-slave"
	ln -s modbus-slave "$root/usr/bin/modbus_slave"                  # the v1 name
	install -D -m 0644 packaging/modbus-slave.service "$root/lib/systemd/system/modbus-slave.service"
	install -D -m 0644 packaging/default "$root/etc/default/modbus-slave"
	install -d -m 0755 "$root/etc/modbus-slave"
	install -D -m 0644 README.md "$root/usr/share/doc/modbus-slave/README.md"
	install -D -m 0644 README-RU.md "$root/usr/share/doc/modbus-slave/README-RU.md"
	install -D -m 0644 LICENSE "$root/usr/share/doc/modbus-slave/copyright"
	for f in examples/*.yaml internal/profile/builtin/*.yaml; do
		install -D -m 0644 "$f" "$root/usr/share/modbus-slave/profiles/$(basename "$f")"
	done
	for f in scripts/*.sh; do
		install -D -m 0755 "$f" "$root/usr/share/modbus-slave/scripts/$(basename "$f")"
	done
	install -d "$root/DEBIAN"
	echo /etc/default/modbus-slave > "$root/DEBIAN/conffiles"
	cat > "$root/DEBIAN/control" <<CTL
Package: modbus-slave
Version: $VER
Architecture: $arch
Maintainer: Dmitrii Novikov <dj.novikov@gmail.com>
Section: utils
Priority: optional
Homepage: https://github.com/lab240/modbus-slave
Installed-Size: $(du -sk "$root" | cut -f1)
Description: Modbus RTU slave emulator and soft sensor
 Makes a serial port look like one or more Modbus RTU devices: test a master
 (SCADA, gateway, PLC) with realistic values and fault injection, or publish
 real data from files as a Modbus device. YAML device profiles, local JSON API,
 raw RS-485 line test. Static binary, the v1 command line still works.
CTL
	cat > "$root/DEBIAN/postinst" <<'PI'
#!/bin/sh
set -e
if [ "$1" = configure ] && [ -d /run/systemd/system ]; then
	systemctl daemon-reload || true
	# installed disabled; on an upgrade a running service is restarted with the new binary
	if systemctl is-active --quiet modbus-slave; then systemctl restart modbus-slave || true; fi
fi
PI
	cat > "$root/DEBIAN/prerm" <<'PR'
#!/bin/sh
set -e
if [ "$1" = remove ] && [ -d /run/systemd/system ]; then
	systemctl disable --now modbus-slave 2>/dev/null || true
fi
PR
	cat > "$root/DEBIAN/postrm" <<'PO'
#!/bin/sh
set -e
[ -d /run/systemd/system ] && systemctl daemon-reload || true
PO
	chmod 0755 "$root/DEBIAN/postinst" "$root/DEBIAN/prerm" "$root/DEBIAN/postrm"
	out=dist/modbus-slave_${VER}_$arch.deb
	dpkg-deb --root-owner-group -Zxz --build "$root" "$out" >/dev/null
	rm -rf "$root"
	echo "$out"
done
