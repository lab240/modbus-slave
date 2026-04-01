/*
 * modbus_core.h — Platform-independent Modbus RTU slave core
 *
 * This header contains everything that is identical on Linux and Windows:
 *   - compile-time constants
 *   - shared data types (port_cfg_t, slave_stat_t)
 *   - global state declarations
 *   - logging macros (adapted per platform via platform.h)
 *   - CRC16 calculation
 *   - port mode string parser
 *   - register file reader (stale file protection)
 *   - Modbus request handler (FC03)
 *   - statistics printer
 *
 * Platform-specific code (serial port open/read, daemon/service,
 * port-busy detection) lives in separate files:
 *   modbus_slave.c      — Linux  (POSIX, termios, fork, syslog)
 *   modbus_slave_win.c  — Windows (Win32 API, CreateFile, Service)
 */

#ifndef MODBUS_CORE_H
#define MODBUS_CORE_H

#include <stdio.h>
#include <stdlib.h>
#include <stdint.h>
#include <string.h>
#include <time.h>
#include <sys/stat.h>   /* stat() for file age check — available on both platforms */

/* ── Compile-time constants ──────────────────────────────────────────────
 * Adjust NUM_REGS and MAX_SLAVES here to change capacity for both platforms.
 */
#define NUM_REGS        20   /* holding registers per slave (FC03 addr 0..N-1) */
#define MAX_SLAVES      30   /* max simultaneous slave IDs on one port          */
#define SLAVES_DEFAULT   1   /* default slave count if -a not specified         */
#define BUF_SIZE       256   /* serial rx buffer; >= max Modbus RTU frame size  */
#define STAT_INTERVAL   60   /* seconds between periodic statistics log lines   */

/* ── Serial port configuration ───────────────────────────────────────────
 * Passed to the platform-specific open_port() function.
 * On Linux:   dev = "/dev/ttyUSB0"
 * On Windows: dev = "\\\\.\\COM7"
 */
typedef struct {
    const char *dev;       /* port device path / name  */
    int         baud;      /* baud rate, e.g. 115200   */
    int         databits;  /* 7 or 8                   */
    char        parity;    /* 'N', 'E', or 'O'         */
    int         stopbits;  /* 1 or 2                   */
} port_cfg_t;

/* ── Per-slave traffic statistics ────────────────────────────────────────
 * Indexed by slave ID (1..MAX_SLAVES). Index 0 is unused.
 */
typedef struct {
    unsigned long ok;        /* successfully answered FC03 requests  */
    unsigned long err_crc;   /* frames with bad CRC (silently dropped) */
    unsigned long err_addr;  /* FC03 register range out of bounds    */
    unsigned long err_fc;    /* unsupported function code received   */
} slave_stat_t;

/* ── Global state (defined in the platform-specific .c file) ────────────
 * Declared extern here so modbus_core.h functions can access them.
 */
extern slave_stat_t g_stat[MAX_SLAVES + 1]; /* per-slave counters          */
extern int          g_num_slaves;           /* number of active slaves      */
extern const char  *g_regs_file[MAX_SLAVES + 1]; /* NULL = random          */
extern int          g_file_max_age;         /* max file age in seconds      */
extern int          g_daemon;               /* 1 = running as daemon        */

/* ── Platform logging bridge ─────────────────────────────────────────────
 * Each platform file must define log_info() and log_err() before including
 * this header, or define MB_LOG_I / MB_LOG_E macros.
 * We provide a simple stdout/stderr fallback here.
 *
 * Usage in core code: LOG_I("started on %s", port);
 */
#ifndef LOG_I
#define LOG_I(fmt, ...) do { printf(fmt "\n",  ##__VA_ARGS__); fflush(stdout); } while(0)
#endif
#ifndef LOG_E
#define LOG_E(fmt, ...) do { fprintf(stderr, fmt "\n", ##__VA_ARGS__); } while(0)
#endif

/* ═══════════════════════════════════════════════════════════════════════
 * CRC16 (Modbus)
 * Standard CRC16 with polynomial 0xA001 (bit-reversed 0x8005).
 * Used to validate incoming request frames and sign outgoing responses.
 * ═══════════════════════════════════════════════════════════════════════ */
static inline uint16_t crc16(const uint8_t *buf, int len)
{
    uint16_t crc = 0xFFFF;
    for (int i = 0; i < len; i++) {
        crc ^= buf[i];
        for (int b = 0; b < 8; b++)
            crc = (crc & 1) ? (crc >> 1) ^ 0xA001 : crc >> 1;
    }
    return crc;
}

/* ═══════════════════════════════════════════════════════════════════════
 * parse_mode() — Parse serial port mode string, e.g. "8N1", "7E2"
 *
 * Format: <databits><parity><stopbits>
 *   databits : '7' or '8'
 *   parity   : 'N' (none), 'E' (even), 'O' (odd)
 *   stopbits : '1' or '2'
 *
 * Returns 0 on success, -1 on invalid input.
 * ═══════════════════════════════════════════════════════════════════════ */
static inline int parse_mode(const char *s, int *databits, char *parity, int *stopbits)
{
    if (!s || strlen(s) != 3) return -1;

    int db = s[0] - '0';
    if (db != 7 && db != 8) return -1;

    char p = s[1];
    if (p != 'N' && p != 'E' && p != 'O') return -1;

    int sb = s[2] - '0';
    if (sb != 1 && sb != 2) return -1;

    *databits = db;
    *parity   = p;
    *stopbits = sb;
    return 0;
}

/* ═══════════════════════════════════════════════════════════════════════
 * print_stats() — Log traffic statistics
 *
 * Shows per-slave activity since the last call (delta counters), with
 * timestamp, source type (file/random), and error breakdown.
 * Silent slaves (zero requests in the interval) are skipped to reduce noise.
 * Called periodically from the main loop and once on clean shutdown.
 * ═══════════════════════════════════════════════════════════════════════ */
static inline void print_stats(void)
{
    /* Snapshot of previous counters for delta calculation */
    static slave_stat_t prev[MAX_SLAVES + 1];
    static int first_call = 1;

    /* Format current timestamp */
    char ts[32];
    time_t now = time(NULL);
    struct tm *tm_info = localtime(&now);
    if (tm_info)
        strftime(ts, sizeof(ts), "%Y-%m-%d %H:%M:%S", tm_info);
    else
        snprintf(ts, sizeof(ts), "-");

    /* Calculate totals for summary line */
    unsigned long total_ok  = 0, total_err = 0;
    unsigned long delta_ok  = 0, delta_err = 0;
    for (int i = 1; i <= g_num_slaves; i++) {
        total_ok  += g_stat[i].ok;
        total_err += g_stat[i].err_crc + g_stat[i].err_addr + g_stat[i].err_fc;
        delta_ok  += g_stat[i].ok  - prev[i].ok;
        delta_err += (g_stat[i].err_crc  - prev[i].err_crc)
                   + (g_stat[i].err_addr - prev[i].err_addr)
                   + (g_stat[i].err_fc   - prev[i].err_fc);
    }

    LOG_I("[%s] stats: slaves=%d  interval_ok=%lu interval_err=%lu  total_ok=%lu total_err=%lu",
          ts, g_num_slaves, delta_ok, delta_err, total_ok, total_err);

    /* Per-slave detail — only slaves that had activity in this interval */
    for (int i = 1; i <= g_num_slaves; i++) {
        unsigned long d_ok   = g_stat[i].ok        - prev[i].ok;
        unsigned long d_crc  = g_stat[i].err_crc   - prev[i].err_crc;
        unsigned long d_addr = g_stat[i].err_addr  - prev[i].err_addr;
        unsigned long d_fc   = g_stat[i].err_fc    - prev[i].err_fc;
        unsigned long d_err  = d_crc + d_addr + d_fc;

        /* Skip silent slaves unless first call or there are errors */
        if (!first_call && d_ok == 0 && d_err == 0) continue;

        const char *src = g_regs_file[i] ? "file" : "random";

        if (d_err == 0)
            LOG_I("  id=%-2d  ok=%-6lu  src=%s%s",
                  i, d_ok, src,
                  g_regs_file[i] ? g_regs_file[i] : "");
        else
            LOG_I("  id=%-2d  ok=%-6lu  src=%s%s  ERRORS: crc=%lu addr=%lu fc=%lu",
                  i, d_ok, src,
                  g_regs_file[i] ? g_regs_file[i] : "",
                  d_crc, d_addr, d_fc);
    }

    /* Save current counters as baseline for next interval */
    for (int i = 1; i <= g_num_slaves; i++)
        prev[i] = g_stat[i];
    first_call = 0;
}

/* ═══════════════════════════════════════════════════════════════════════
 * read_regs_from_file() — Load register values from a plain-text file
 *
 * File format: one unsigned decimal integer (0..65535) per line.
 * Lines beyond <count> are ignored.
 * Lines missing (file shorter than <count>) → register value = 0.
 * File does not exist → all registers = 0.
 *
 * Stale file protection:
 *   If g_file_max_age > 0 and file mtime is older than g_file_max_age
 *   seconds, returns all zeros. Warning is rate-limited to once per minute
 *   to avoid flooding the log when a data script is down for a long time.
 *
 * Atomic update pattern (Linux shell):
 *   printf "1234\n5678\n" > /tmp/s1.tmp && mv /tmp/s1.tmp /tmp/s1.dat
 *
 * Atomic update pattern (Windows CMD):
 *   echo 1234 > C:\temp\s1.tmp && echo 5678 >> C:\temp\s1.tmp
 *   move /Y C:\temp\s1.tmp C:\temp\s1.dat
 * ═══════════════════════════════════════════════════════════════════════ */
static inline void read_regs_from_file(const char *path, uint16_t *regs, int count)
{
    /* Default: all registers zero */
    memset(regs, 0, count * sizeof(uint16_t));

    /* Stale data check: compare file modification time to current time */
    if (g_file_max_age > 0) {
        struct stat st;
        if (stat(path, &st) < 0) return;  /* file missing -> zeros */

        time_t age = time(NULL) - st.st_mtime;
        if (age > (time_t)g_file_max_age) {
            /* Rate-limit warning to once per minute */
            static time_t last_warn = 0;
            time_t now = time(NULL);
            if (now - last_warn >= 60) {
                LOG_E("file %s is stale (%lds > %ds), returning zeros",
                      path, (long)age, g_file_max_age);
                last_warn = now;
            }
            return;  /* stale -> zeros */
        }
    }

    FILE *f = fopen(path, "r");
    if (!f) return;  /* disappeared between stat() and fopen() -> zeros */

    for (int i = 0; i < count; i++) {
        unsigned int v = 0;
        if (fscanf(f, "%u", &v) != 1) break;  /* EOF or parse error */
        regs[i] = (uint16_t)(v & 0xFFFF);
    }
    fclose(f);
}

/* ═══════════════════════════════════════════════════════════════════════
 * send_exception() — Send a Modbus exception response
 *
 * Called when a valid frame is received but cannot be processed:
 *   code 0x01 = Illegal Function      (we only implement FC03)
 *   code 0x02 = Illegal Data Address  (register range out of bounds)
 *
 * Exception frame layout:
 *   [0]   slave ID
 *   [1]   function code | 0x80  (high bit = exception flag)
 *   [2]   exception code
 *   [3-4] CRC16 little-endian
 *
 * The serial_write() function must be provided by the platform file.
 * ═══════════════════════════════════════════════════════════════════════ */

/* Forward declaration — implemented in platform-specific file */
extern int serial_write(int fd, const uint8_t *buf, int len);

static inline void send_exception(int fd, uint8_t id, uint8_t fc, uint8_t code)
{
    uint8_t exc[5];
    exc[0] = id;
    exc[1] = fc | 0x80;
    exc[2] = code;
    uint16_t c = crc16(exc, 3);
    exc[3] = c & 0xFF;
    exc[4] = c >> 8;
    if (serial_write(fd, exc, 5) < 0)
        LOG_E("write exception failed");
}

/* ═══════════════════════════════════════════════════════════════════════
 * handle_request() — Process one received Modbus RTU request frame
 *
 * Supported function codes:
 *   FC03 (0x03) — Read Holding Registers
 *
 * FC03 request frame:
 *   [0]    slave address
 *   [1]    function code = 0x03
 *   [2-3]  starting register address (big-endian)
 *   [4-5]  number of registers to read (big-endian)
 *   [6-7]  CRC16 (little-endian)
 *
 * FC03 response frame:
 *   [0]    slave address
 *   [1]    0x03
 *   [2]    byte count = num_regs * 2
 *   [3..]  register values, 2 bytes each, big-endian
 *   [last] CRC16 (little-endian)
 *
 * Register values:
 *   - If g_regs_file[slave_id] is set: read from file (real sensor data)
 *   - Otherwise: random uint16 values (bus load / master testing)
 * ═══════════════════════════════════════════════════════════════════════ */
static inline void handle_request(int fd, const uint8_t *req, int rlen)
{
    /* Minimum valid RTU frame: addr(1) + fc(1) + addr(2) + count(2) + crc(2) = 8 */
    if (rlen < 8) return;

    uint8_t id = req[0];

    /* Validate CRC — stored little-endian in the last two bytes */
    uint16_t crc_rx = (uint16_t)req[rlen-2] | ((uint16_t)req[rlen-1] << 8);
    if (crc16(req, rlen - 2) != crc_rx) {
        /* Per Modbus spec: no response to frames with bad CRC */
        if (id >= 1 && id <= g_num_slaves) g_stat[id].err_crc++;
        return;
    }

    uint8_t  fc    = req[1];
    uint16_t start = ((uint16_t)req[2] << 8) | req[3];  /* first register address */
    uint16_t count = ((uint16_t)req[4] << 8) | req[5];  /* number of registers    */

    /* Ignore frames not addressed to any of our slave IDs */
    if (id < 1 || id > g_num_slaves) return;

    /* Reject unsupported function codes */
    if (fc != 0x03) {
        send_exception(fd, id, fc, 0x01);  /* 0x01 = Illegal Function */
        g_stat[id].err_fc++;
        return;
    }

    /* Validate register address range */
    if (count == 0 || start + count > NUM_REGS) {
        send_exception(fd, id, fc, 0x02);  /* 0x02 = Illegal Data Address */
        g_stat[id].err_addr++;
        return;
    }

    /* Build FC03 response frame */
    uint8_t resp[3 + NUM_REGS * 2 + 2];
    resp[0] = id;
    resp[1] = 0x03;
    resp[2] = (uint8_t)(count * 2);  /* byte count */

    if (g_regs_file[id]) {
        /* File mode: read all registers at once, serve the requested slice */
        uint16_t all[NUM_REGS];
        read_regs_from_file(g_regs_file[id], all, NUM_REGS);
        for (int i = 0; i < (int)count; i++) {
            resp[3 + i*2]   = all[start + i] >> 8;    /* high byte first (big-endian) */
            resp[3 + i*2+1] = all[start + i] & 0xFF;  /* low byte */
        }
    } else {
        /* Random mode: fresh random value per register per request */
        for (int i = 0; i < (int)count; i++) {
            uint16_t v = (uint16_t)(rand() & 0xFFFF);
            resp[3 + i*2]   = v >> 8;
            resp[3 + i*2+1] = v & 0xFF;
        }
    }

    /* Append CRC16 (little-endian) */
    int plen = 3 + count * 2;
    uint16_t crc_tx = crc16(resp, plen);
    resp[plen]   = crc_tx & 0xFF;
    resp[plen+1] = crc_tx >> 8;

    if (serial_write(fd, resp, plen + 2) == plen + 2)
        g_stat[id].ok++;
    else
        LOG_E("id=%d: serial write error", id);
}

#endif /* MODBUS_CORE_H */
