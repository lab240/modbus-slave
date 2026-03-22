# modbus_slave — Modbus RTU Slave Emulator

Emulates up to 30 independent Modbus RTU slave devices on a single serial port.  
Each slave responds to **FC03 (Read Holding Registers)** with 20 registers.

Register values can come from:
- **File mode** — a plain-text file updated by an external script (real sensor data: I2C, 1-Wire, sysfs, etc.)
- **Random mode** — random `uint16` values (bus load testing, master timeout verification)

Runs on **Linux** (x86_64, aarch64) and **Windows** (x64).  
Designed for embedded systems — single static binary, no dependencies, no SD card writes (tmpfs).

---

## Repository structure

```
modbus_slave/
├── bin/
│   ├── modbus_slave          # Linux x86_64 binary
│   ├── modbus_slave_aarch64  # Linux aarch64 static binary (NAPI2, RK3568, RPi)
│   └── modbus_slave.exe      # Windows x64 binary
├── service/
│   └── modbus_slave.service  # systemd unit file
└── src/
    ├── modbus_core.h         # Platform-independent Modbus logic
    ├── modbus_slave.c        # Linux platform (termios, fork, syslog)
    └── modbus_slave_win.c    # Windows platform (Win32 API)
```

---

## Build

### Linux x86_64
```bash
gcc -O2 -Wall -o bin/modbus_slave src/modbus_slave.c
```

### Linux aarch64 (static — for NAPI2, RK3568, Raspberry Pi)
```bash
aarch64-linux-gnu-gcc -O2 -Wall -static -o bin/modbus_slave_aarch64 src/modbus_slave.c
```

### Windows x64 (cross-compile from Linux)
```bash
x86_64-w64-mingw32-gcc -O2 -Wall -o bin/modbus_slave.exe src/modbus_slave_win.c
```

### Windows x64 (native MinGW / MSYS2)
```bash
gcc -O2 -Wall -o modbus_slave.exe src/modbus_slave_win.c
```

### Windows x64 (MSVC)
```cmd
cl /O2 src\modbus_slave_win.c
```

---

## Usage

### Linux
```
modbus_slave -p <port> -b <baud> [-a <n>] [-o <mode>] [-f <id>:<file>] [-t <sec>] [-d]
modbus_slave -k
modbus_slave -s
```

### Windows
```
modbus_slave.exe -p <port> -b <baud> [-a <n>] [-o <mode>] [-f <id>:<file>] [-t <sec>] [-bg]
modbus_slave.exe -k
modbus_slave.exe -s
```

### Options

| Option | Description |
|--------|-------------|
| `-p <port>` | Serial port. Linux: `/dev/ttyUSB0`, `/dev/ttyS7`. Windows: `COM3`, `\\.\COM10` |
| `-b <baud>` | Baud rate: `1200`..`921600` |
| `-a <n>` | Number of slave devices: `1`..`30` (default `1`). IDs assigned `1..n` |
| `-o <mode>` | Port framing: `8N1` `8E1` `8O1` `7E1` `8N2` (default `8N1`) |
| `-f <id>:<file>` | Bind register file to slave `<id>`. Repeatable. Slaves without `-f` use random values |
| `-t <sec>` | Max register file age in seconds (default `10`). Stale file → zeros. `-t 0` disables |
| `-d` | *(Linux)* Run as background daemon. Logs → syslog |
| `-bg` | *(Windows)* Run in background. Logs → `C:\temp\modbus_slave.log` |
| `-k` | Stop running daemon / background instance |
| `-s` | Show daemon status |

---

## Register file format

One unsigned decimal integer (`0`..`65535`) per line, up to 20 lines.  
Missing lines → register value `0`.  
File does not exist → all registers `0`.

Example `/tmp/sensor1.dat`:
```
4523
1013
0
0
0
0
0
0
0
0
```
`4523` = 45.23 °C (temperature × 100 convention)

### Atomic update (Linux — tmpfs, no SD writes)
```bash
printf "4523\n1013\n" > /tmp/s1.tmp && mv /tmp/s1.tmp /tmp/s1.dat
```

### Atomic update (Windows)
```cmd
echo 4523 > C:\temp\s1.tmp
echo 1013 >> C:\temp\s1.tmp
move /Y C:\temp\s1.tmp C:\temp\s1.dat
```

---

## Examples

### Linux — 5 slaves, two with real data
```bash
./modbus_slave -p /dev/ttyUSB0 -b 115200 -a 5 \
    -f 1:/tmp/cpu.dat \
    -f 2:/tmp/time.dat
# slaves 3, 4, 5 → random
```

### Linux — daemon mode on NAPI2
```bash
./modbus_slave -d -p /dev/ttyS7 -b 115200 -a 10 \
    -f 1:/tmp/cpu.dat \
    -t 15
```

### Windows — 3 slaves
```cmd
modbus_slave.exe -p COM4 -b 115200 -a 3 -f 1:C:\temp\s1.dat -f 2:C:\temp\s2.dat
```

### Windows — background
```cmd
modbus_slave.exe -bg -p COM4 -b 115200 -a 3 -f 1:C:\temp\s1.dat
modbus_slave.exe -s
modbus_slave.exe -k
```

---

## Helper scripts (Linux)

### CPU temperature → Modbus registers
```bash
chmod +x scripts/cpu_temp_to_modbus.sh
./scripts/cpu_temp_to_modbus.sh /tmp/cpu.dat 5 &
```
Reads from `/sys/class/thermal/thermal_zone*/temp`.  
Values in register: `°C × 100` (e.g. `4523` = 45.23 °C).

### Current time → Modbus registers
```bash
chmod +x scripts/time_to_modbus.sh
./scripts/time_to_modbus.sh /tmp/time.dat 1 &
```

| Register | Value | Range |
|----------|-------|-------|
| 0 | Hours | 0..23 |
| 1 | Minutes | 0..59 |
| 2 | Seconds | 0..59 |
| 3 | Day | 1..31 |
| 4 | Month | 1..12 |
| 5 | Year | 2025.. |
| 6 | Day of week | 1=Mon..7=Sun |
| 7..19 | Reserved | 0 |

---

## systemd service (Linux)

```bash
sudo cp bin/modbus_slave /usr/local/bin/
sudo cp service/modbus_slave.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable modbus_slave
sudo systemctl start modbus_slave
```

Edit `/etc/systemd/system/modbus_slave.service` to adjust port, baud and `-f` bindings.

```bash
journalctl -u modbus_slave -f
```

---

## Stale data protection

If a data-provider script crashes, its register file stops being updated.  
Without protection the Modbus master would keep reading stale values.

With `-t 10` (default): if a file has not been modified for more than 10 seconds,  
all its registers return **zero** — making the failure immediately visible to the master.

```
file /tmp/cpu.dat is stale (45s > 10s), returning zeros
```

Disable with `-t 0` if you update files infrequently.

---

## Protocol details

| Parameter | Value |
|-----------|-------|
| Protocol | Modbus RTU |
| Supported FC | FC03 — Read Holding Registers |
| Register count | 20 per slave (addr 0..19) |
| Max slaves | 30 (IDs 1..30) |
| Frame delimiter | 10 ms inter-character silence |
| CRC | CRC16 (poly 0xA001) |

---

## Tested on

| Platform | OS | Notes |
|----------|----|-------|
| x86_64 | Ubuntu 24 | development / testing |
| aarch64 | Armbian (NAPI2 / RK3568J) | production target |
| x64 | Windows 10/11 | via MinGW |

---

## License

MIT
