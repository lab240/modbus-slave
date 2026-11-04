# modbus-slave v2 — Modbus RTU slave emulator and soft sensor

[Русская версия](README-RU.md)

`modbus-slave` makes a serial port look like one or more Modbus RTU devices. It is used to:

- **test a master** (SCADA, a gateway, a PLC) without real devices: realistic values, any baud rate,
  many devices on a line, fault injection;
- **publish real data as a Modbus device** (a "soft sensor"): register values come from text files that
  any script keeps up to date (I2C, 1-Wire, sysfs, CPU temperature, time...);
- **check a line**: a pty mode for tests without hardware, statistics per line.

One static binary, no dependencies. v2 is a rewrite in Go of the C version (v1, still in `src/`);
the v1 command line keeps working.

> Status: **v2 is in development** (branch `v2`). Linux is tested (also on real FT4232H lines); the Windows
> build (`COM3`, Win32 API) compiles but has **not been tried on Windows yet** — the v1 `.exe` stays in `bin/`
> until it has. On Windows: ports are `COM<n>`; no `--pty` (use a com0com pair), no FT4232H channel numbers,
> no `-d/-k/-s`; the FTDI latency is set in the driver (Device Manager → port → Advanced).

## Build

```sh
./build.sh                 # dist/modbus-slave-linux-{amd64,arm64,arm} (static), dist/modbus-slave-windows-amd64.exe
go build ./cmd/modbus-slave
go test -race ./...
```

## Quick start

```sh
# v1 style: 5 slaves on one port, random values; slave 1 from a file
modbus-slave -p /dev/ttyUSB0 -b 115200 -a 5 -f 1:/tmp/cpu.dat -t 15

# 4 lines of an FT4232H, 4 KTH2-R sensors on each, answer after 5 ms
modbus-slave -p 0 -p 1 -p 2 -p 3 -b 57600 --profile kth2r --units 1-4 --delay 5

# no hardware: a pty; the master opens /tmp/ttyEMU
modbus-slave --pty /tmp/ttyEMU -b 9600 --profile kth2r
mbpoll -m rtu -b 9600 -P none -a 1 -r 769 -c 2 /tmp/ttyEMU

# your own device from a YAML profile, with the local API
modbus-slave -p /dev/ttyS7 -b 9600 --profile examples/soft-sensor.yaml --api 127.0.0.1:8650
```

## Command line

| Option | Meaning |
|---|---|
| `-p <port>` | Serial port: `/dev/ttyUSB0`, `/dev/ttyS7`, or an **FT4232H/FT2232H channel** `0..3` / `A..D`, found by USB interface number (survives a re-plug, when the kernel hands out `ttyUSB4..7`). **Repeat for several lines.** |
| `--pty <link>` | Serve a new pty and point `<link>` at it (tests without hardware). Repeatable. |
| `-b <baud>` | Any baud rate the UART can do, 1200 … 4000000 (default 19200). |
| `-o <mode>` | `8N1` `8E1` `8O1` `7E1` `8N2` (default `8N1`). |
| `-a <n>` / `--units 1-4,7` | Unit IDs: `1..n`, or a list. Every line gets the same IDs. |
| `--profile <p>` | `generic` (default, v1 device), a built-in profile (`--list-profiles`) or a YAML file. |
| `-f [line.]id:file` | `generic`: values of slave `id` from a file; `2.1:/tmp/x` = line 2 only. |
| `-t <s>` | Max age of a file, older → zeros (default 10, `0` = no check). |
| `--regs <n>` | `generic`: number of registers (default 20). |
| `--delay`, `--jitter` | Answer delay and random extra delay, ms. |
| `--drop`, `--exc`, `--badcrc` | Probability of no answer / exception 4 / a broken CRC. |
| `--frag <n>`, `--frag-gap <ms>` | Send the answer in `n` pieces with a pause (tests the master's t3.5). |
| `--echo` | Discard the local echo of our answers (a transceiver that hears itself). |
| `--latency <ms>` | FTDI `latency_timer` (default 1; FTDI's own default 16 ms adds up to 16 ms per answer). |
| `--gap <ms>` | Silence that discards a partial frame (default 20; `0` = t3.5 of the baud rate). |
| `--report <s>` | Statistics every `s` seconds (default 60, `0` = off). |
| `--api <addr>` | Local JSON API: `127.0.0.1:PORT` or `unix:/path`. A non-loopback address needs `--api-token-file`. |
| `--seed <n>` | Random seed for repeatable runs. |
| `-d` / `-k` / `-s` | Run in the background (syslog) / stop it / status — as v1. Under systemd use a unit instead. |

Requests are framed by the length of their function code, so the answer can start right after the last
byte of the request; a broken frame or garbage in front of a good request does not swallow it. A port that
disappears (USB unplugged) is reopened when it comes back; the devices keep their state.

## Values from files (soft sensor)

A file holds one number per line; a script rewrites it atomically:

```sh
printf "4523\n1013\n" > /tmp/s1.tmp && mv /tmp/s1.tmp /tmp/s1.dat
```

- missing file → zeros; file older than `-t` / `max_age` → zeros (or exception 4 with `on_stale: exception`),
  so a dead data script is visible at the master instead of frozen values;
- the file is re-read only when it changes;
- numbers may be negative (`-5` → two's complement), hex (`0x1F`) or fractional (`45.23`, with a `scale`
  in a profile); text after the number is ignored (`4523  # T x100`).

`scripts/cpu_temp_to_modbus.sh` and `scripts/time_to_modbus.sh` are ready-made data scripts.

## Device profiles (YAML)

A profile lists the function codes and the registers. Every block of registers has **one source**:

| Source | Example | Notes |
|---|---|---|
| `value` / `values` | `{addr: 0, values: [1, 0x120]}` | const; `base`, `line_step`, `unit_step`, `index_step` are added |
| `string` | `{addr: 2, string: "KTH2-R"}` | 2 characters per register |
| `wave` | `{addr: 0x300, wave: {shape: sine, base: 215, amp: 30, period: 600, noise: 1}}` | `saw`, `triangle`, `sine`, `square`, `const`; `line_step`/`unit_step` shift the base |
| `file` | `{addr: 20, file: /run/s{unit}.dat, file_line: 0, max_age: 10, on_stale: exception}` | `{line}`, `{unit}` in the path |
| `random` | `{addr: 50, count: 10, random: true}` | new values on every read |
| `special(s)` | `{addr: 0x1000, specials: [served_hi, served_lo, line, unit, baud100]}` | also `speed_code`, `parity_code`; `unit` with `rw: true` changes the address |

Per block: `count` (number of values), `type` (`uint16`, `int16`, `uint32`, `int32`, `float32`),
`word_order` (`big` = high word first, or `little`), `scale` (value × scale goes into the register),
`rw: true` (the master may write it; FC 6/16 writes are all or nothing), `add: <addr>` (+ the value of another
register, e.g. a correction the master writes). `input:` is an optional separate map for FC 4; without it
FC 4 reads the holding map. Unknown keys, overlapping registers and two sources in one block are errors.

Built-in: `kth2r` — KTH2-R T/RH sensor (see `internal/profile/builtin/kth2r.yaml`).
Example: [`examples/soft-sensor.yaml`](examples/soft-sensor.yaml) — CPU temperature from a file as float32 and
int16, uptime as uint32, writable set points.

## Local JSON API

`--api 127.0.0.1:8650` (or `unix:/run/modbus-slave.sock`). Errors are `{"detail": "..."}`.

| Request | |
|---|---|
| `GET /api/v1/status` | version, profile, mode, uptime; per line: device, open, counters, faults, devices |
| `GET /api/v1/profiles` | the profiles |
| `GET /api/v1/lines/{line}/faults` | fault settings |
| `PUT /api/v1/lines/{line}/faults` | change them on the fly: `{"delay_ms": 20, "jitter_ms": 30, "drop": 0.01, "exc": 0, "badcrc": 0, "frag": 0, "frag_gap_ms": 5, "echo": false}` — only the fields given |
| `GET /api/v1/lines/{line}/devices/{unit}/registers?addr=0x300&count=2[&fc=4]` | read registers |
| `PUT /api/v1/lines/{line}/devices/{unit}/registers` | write writable registers: `{"addr": 1279, "values": [-50]}` |

```sh
curl -s localhost:8650/api/v1/status
curl -s -X PUT -d '{"drop": 0.05}' localhost:8650/api/v1/lines/2/faults
```

## Line test

A raw test of the RS-485 line itself, without Modbus: one side sends on all ports at once, the other listens;
then swap the roles. It shows crossed pairs, dead pairs and the speed limit of a long line in a minute.

```sh
modbus-slave line-test listen -p 0 -p 1 -p 2 -p 3 -b 57600 --secs 6     # start first
modbus-slave line-test send   -p 0 -p 1 -p 2 -p 3 -b 57600 --secs 3
```

`--pattern tokens` (default) sends `<P1 00042>`: the listener tells which port reached it and how many bytes are
wrong. `--pattern bytes` sends `00..FF` and counts flipped bits — a line too slow for the baud rate turns a lone 0
after 1s into 1 (`03` arrives as `07`). `--burst 8 --pause 20` sends short bursts with silence between them, like
Modbus frames. **Check a speed with `--pattern bytes`**: tokens contain only digits and brackets and miss the bit
combinations that a long line distorts first (on 915 m at 230400 tokens were clean while `C4 4F` arrived as `C4 FE`).
A real run, 915 m of twisted pair at 115200 without a terminator at the far end:

```
  0              30730 bytes  30597 bytes off the counter; first 64 bytes: bits 0->1 33, 1->0 0
               first bytes: 00 01 02 07 04 05 0E 0F 08 09 0A 0F 1C 1D 1E 1F
```

## Statistics

```
[    60s] line 0: 1952 ans 3/s  line 1: 1952 ans 3/s (resync 2)  ...
```

`ans` answers, `exc` exceptions sent, `foreign` requests for other units, `bcast` broadcasts,
`resync` bytes skipped to find a frame (noise, a broken frame), `drop`/`badcrc` injected faults.

## Debian package

```sh
./build.sh && packaging/build-deb.sh     # dist/modbus-slave_<ver>_{amd64,arm64,armhf}.deb
```

`/usr/bin/modbus-slave` (+ `modbus_slave`, the v1 name), `modbus-slave.service` (installed **disabled**), options in
`/etc/default/modbus-slave` (`MODBUS_SLAVE_OPTS`, a conffile), profiles and examples in
`/usr/share/modbus-slave/profiles/`, data scripts in `/usr/share/modbus-slave/scripts/`.

```sh
sudoedit /etc/default/modbus-slave
sudo systemctl enable --now modbus-slave
journalctl -u modbus-slave -f
```

## v1 (C)

The C version is in `src/` and its binaries in `bin/` (Linux x86_64, aarch64, Windows x64). The v2 command line
accepts the v1 options (`-p -b -a -o -f -t -d -k -s`); a v1 device = `--profile generic` (the default):
20 holding registers, FC 3 (v2 also answers FC 4 on the same map), random or from a file.

## Authors

**Dmitrii Novikov** ([@dmnovikov](https://github.com/dmnovikov)), [NAPI Lab](https://github.com/napilab)
**Claude** (Anthropic) — AI assistant and co-author

MIT License
