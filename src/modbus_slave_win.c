/*
 * modbus_slave_win.c — Windows platform entry point for Modbus RTU slave emulator
 *
 * Provides all Windows-specific functionality:
 *   - Serial port open/configure via Win32 CreateFile / SetCommState / DCB
 *   - Frame reading via ReadFile + COMMTIMEOUTS (inter-character timeout)
 *   - Port-busy detection: Windows refuses CreateFile on an open port
 *   - Background mode via FreeConsole() (lightweight, no full Windows Service)
 *   - Logging via printf / logfile (no syslog on Windows)
 *   - Signal handling via SetConsoleCtrlHandler
 *   - Command-line parsing via manual argv loop (no getopt on MSVC)
 *
 * Platform-independent Modbus logic is in modbus_core.h.
 *
 * Build (MinGW / MSYS2):
 *   gcc -O2 -Wall -o modbus_slave_win modbus_slave_win.c
 *
 * Build (MSVC):
 *   cl /O2 /W3 modbus_slave_win.c
 *
 * Usage:
 *   modbus_slave_win.exe -p COM3 -b 115200 [-a <n>] [-o <mode>] [-f <id>:<file>] [-t <sec>] [-bg]
 *   modbus_slave_win.exe -k
 *   modbus_slave_win.exe -s
 *
 * Options:
 *   -p <port>       COM port name: COM1..COM9 or \\.\COM10 and above
 *   -b <baud>       baud rate: 1200..921600
 *   -a <n>          number of slave devices: 1..30 (default 1)
 *   -o <mode>       port framing: 8N1 8E1 8O1 7E1 8N2 (default 8N1)
 *   -f <id>:<file>  bind register file to slave id (repeatable)
 *                   recommended location: C:\temp\s1.dat
 *   -t <sec>        max file age in seconds, 0=disable (default 10)
 *   -bg             run in background (detach from console)
 *   -k              stop running background instance (via PID file)
 *   -s              show background instance status
 *
 * Register file format (same as Linux):
 *   One unsigned decimal per line (0..65535).
 *   Place in C:\temp\ or any local directory.
 *   Atomic update (CMD):
 *     echo 1234 > C:\temp\s1.tmp
 *     move /Y C:\temp\s1.tmp C:\temp\s1.dat
 *
 * Examples:
 *   modbus_slave_win -p COM3 -b 115200
 *   modbus_slave_win -p \\.\COM10 -b 115200 -a 5 -f 1:C:\temp\s1.dat
 *   modbus_slave_win -bg -p COM3 -b 115200 -a 3 -f 1:C:\temp\cpu.dat
 *   modbus_slave_win -k
 */

/* Ensure we get full Win32 API */
#ifndef _WIN32_WINNT
#define _WIN32_WINNT 0x0601  /* Windows 7 and above */
#endif

#include <windows.h>
#include <stdio.h>
#include <stdlib.h>
#include <stdint.h>
#include <string.h>
#include <time.h>
#include <sys/stat.h>   /* for stat() in modbus_core.h */

/* ── Global state (shared with modbus_core.h) ───────────────────────── */

/* On Windows we log to stdout (foreground) or a log file (background).
 * Define LOG macros before including modbus_core.h. */
static FILE *g_logfile = NULL;  /* NULL = use stdout/stderr */

#define LOG_I(fmt, ...) do { \
    FILE *_f = g_logfile ? g_logfile : stdout; \
    fprintf(_f, fmt "\n", ##__VA_ARGS__); fflush(_f); \
} while(0)

#define LOG_E(fmt, ...) do { \
    FILE *_f = g_logfile ? g_logfile : stderr; \
    fprintf(_f, "ERROR: " fmt "\n", ##__VA_ARGS__); fflush(_f); \
} while(0)

#include "modbus_core.h"

/* ── Global state definitions ───────────────────────────────────────── */
slave_stat_t g_stat[MAX_SLAVES + 1];
int          g_num_slaves   = SLAVES_DEFAULT;
const char  *g_regs_file[MAX_SLAVES + 1];
int          g_file_max_age = 10;
int          g_daemon       = 0;  /* unused on Windows, kept for core.h compat */

/* ── Shutdown flag — set by Ctrl+C handler or -k ────────────────────── */
static volatile int g_running = 1;

static BOOL WINAPI ctrl_handler(DWORD ctrl_type)
{
    if (ctrl_type == CTRL_C_EVENT || ctrl_type == CTRL_BREAK_EVENT ||
        ctrl_type == CTRL_CLOSE_EVENT) {
        g_running = 0;
        return TRUE;
    }
    return FALSE;
}

/* ── PID file (used by -bg / -k / -s) ───────────────────────────────── */
#define PID_FILE  "C:\\temp\\modbus_slave.pid"

static int write_pid(void)
{
    /* Ensure C:\temp exists */
    CreateDirectoryA("C:\\temp", NULL);
    FILE *f = fopen(PID_FILE, "w");
    if (!f) { LOG_E("cannot write PID file %s", PID_FILE); return -1; }
    fprintf(f, "%lu\n", (unsigned long)GetCurrentProcessId());
    fclose(f);
    return 0;
}

static DWORD read_pid(void)
{
    FILE *f = fopen(PID_FILE, "r");
    if (!f) return 0;
    unsigned long pid = 0;
    fscanf(f, "%lu", &pid);
    fclose(f);
    return (DWORD)pid;
}

/* ── serial_write() — Win32 serial write wrapper required by core.h ──── */
int serial_write(int fd, const uint8_t *buf, int len)
{
    /* On Windows we use HANDLE, cast from int */
    HANDLE h = (HANDLE)(intptr_t)fd;
    DWORD written = 0;
    if (!WriteFile(h, buf, (DWORD)len, &written, NULL)) return -1;
    return (int)written;
}

/* ── Open and configure serial port ─────────────────────────────────────
 * Win32 serial ports are opened as file handles.
 * Ports COM10 and above need the "\\.\COM10" prefix.
 * We detect single-digit COM names and add the prefix automatically.
 *
 * Returns a HANDLE cast to int (for compatibility with core.h interface).
 * Returns -1 on failure.
 */
static int open_port(const port_cfg_t *cfg)
{
    /* Build the device name — COM10+ need "\\.\" prefix */
    char dev_name[64];
    if (cfg->dev[0] == '\\') {
        /* Already has prefix (e.g. \\.\COM10) */
        strncpy(dev_name, cfg->dev, sizeof(dev_name) - 1);
    } else {
        snprintf(dev_name, sizeof(dev_name), "\\\\.\\%s", cfg->dev);
    }

    HANDLE h = CreateFileA(
        dev_name,
        GENERIC_READ | GENERIC_WRITE,
        0,              /* exclusive access — no sharing */
        NULL,
        OPEN_EXISTING,
        FILE_ATTRIBUTE_NORMAL,
        NULL
    );

    if (h == INVALID_HANDLE_VALUE) {
        DWORD err = GetLastError();
        if (err == ERROR_ACCESS_DENIED || err == ERROR_SHARING_VIOLATION)
            fprintf(stderr, "Error: %s is already in use by another process\n", cfg->dev);
        else
            fprintf(stderr, "Error: cannot open %s (code %lu)\n", cfg->dev, err);
        return -1;
    }

    /* Configure port parameters via DCB */
    DCB dcb;
    memset(&dcb, 0, sizeof(dcb));
    dcb.DCBlength = sizeof(dcb);

    if (!GetCommState(h, &dcb)) {
        fprintf(stderr, "Error: GetCommState failed\n");
        CloseHandle(h); return -1;
    }

    dcb.BaudRate = (DWORD)cfg->baud;

    /* Data bits */
    dcb.ByteSize = (BYTE)cfg->databits;

    /* Parity */
    switch (cfg->parity) {
        case 'N': dcb.Parity = NOPARITY;   dcb.fParity = FALSE; break;
        case 'E': dcb.Parity = EVENPARITY; dcb.fParity = TRUE;  break;
        case 'O': dcb.Parity = ODDPARITY;  dcb.fParity = TRUE;  break;
    }

    /* Stop bits */
    dcb.StopBits = (cfg->stopbits == 2) ? TWOSTOPBITS : ONESTOPBIT;

    /* Disable flow control */
    dcb.fOutxCtsFlow = FALSE;
    dcb.fOutxDsrFlow = FALSE;
    dcb.fDtrControl  = DTR_CONTROL_DISABLE;
    dcb.fRtsControl  = RTS_CONTROL_DISABLE;
    dcb.fOutX        = FALSE;
    dcb.fInX         = FALSE;

    if (!SetCommState(h, &dcb)) {
        fprintf(stderr, "Error: SetCommState failed\n");
        CloseHandle(h); return -1;
    }

    /* Set timeouts:
     * ReadIntervalTimeout: max milliseconds between bytes in a frame.
     * We use 10ms — matches our Linux select() inter-character timeout.
     * ReadTotalTimeoutConstant: overall read deadline (500ms = idle wait).
     */
    COMMTIMEOUTS timeouts;
    timeouts.ReadIntervalTimeout         = 10;   /* 10 ms inter-char gap */
    timeouts.ReadTotalTimeoutMultiplier  = 0;
    timeouts.ReadTotalTimeoutConstant    = 500;  /* 500 ms max wait      */
    timeouts.WriteTotalTimeoutMultiplier = 0;
    timeouts.WriteTotalTimeoutConstant   = 500;
    SetCommTimeouts(h, &timeouts);

    /* Flush any stale data */
    PurgeComm(h, PURGE_RXCLEAR | PURGE_TXCLEAR);

    return (int)(intptr_t)h;
}

/* ── Read one Modbus RTU frame from the serial port ─────────────────────
 * On Windows, COMMTIMEOUTS handles the inter-character gap (10ms).
 * We loop ReadFile until a gap is detected (ReadFile returns 0 bytes)
 * after at least 4 bytes have been received.
 *
 * Also handles periodic statistics output.
 * Returns byte count, 0 on timeout/signal, -1 on error.
 */
static int read_frame(int fd, uint8_t *buf, int maxlen, time_t *last_stat)
{
    HANDLE h = (HANDLE)(intptr_t)fd;
    int total = 0;

    while (g_running) {
        /* Periodic stats check */
        time_t now = time(NULL);
        if (now - *last_stat >= STAT_INTERVAL) {
            print_stats();
            *last_stat = now;
        }

        DWORD n = 0;
        BOOL ok = ReadFile(h, buf + total, (DWORD)(maxlen - total), &n, NULL);

        if (!ok) return -1;  /* read error */

        if (n == 0) {
            /* Timeout: either inter-character gap (frame end) or idle bus */
            if (total >= 4) return total;  /* complete frame */
            total = 0;                     /* incomplete — discard and retry */
            continue;
        }

        total += (int)n;
        if (total >= maxlen) return total;
    }
    return 0;
}

/* ── Background mode (Windows equivalent of daemon) ─────────────────────
 * We can't do fork() on Windows. Instead:
 *   1. Re-launch ourselves with the same arguments plus an internal flag
 *   2. The child calls FreeConsole() to detach from the terminal
 *   3. Write PID file so -k can find and terminate it
 *
 * This is simpler than a full Windows Service but works for our use case.
 * For production deployment, wrapping with NSSM or sc.exe is recommended.
 */
static void go_background(int argc, char *argv[])
{
    /* Build command line for child process, excluding -bg flag */
    char cmdline[4096] = {0};
    for (int i = 0; i < argc; i++) {
        if (strcmp(argv[i], "-bg") == 0) continue;
        if (strlen(cmdline) + strlen(argv[i]) + 4 >= sizeof(cmdline)) break;
        if (i > 0) strcat(cmdline, " ");
        strcat(cmdline, "\"");
        strcat(cmdline, argv[i]);
        strcat(cmdline, "\"");
    }
    /* Add internal flag to tell child not to fork again */
    strcat(cmdline, " --bg-child");

    STARTUPINFOA si;
    PROCESS_INFORMATION pi;
    memset(&si, 0, sizeof(si));
    si.cb = sizeof(si);
    si.dwFlags = STARTF_USESHOWWINDOW;
    si.wShowWindow = SW_HIDE;

    if (!CreateProcessA(NULL, cmdline, NULL, NULL, FALSE,
                        CREATE_NO_WINDOW | DETACHED_PROCESS,
                        NULL, NULL, &si, &pi)) {
        fprintf(stderr, "Error: failed to start background process (code %lu)\n",
                GetLastError());
        exit(1);
    }
    printf("Started background process PID %lu\n", pi.dwProcessId);
    CloseHandle(pi.hProcess);
    CloseHandle(pi.hThread);
    exit(0);
}

/* ── Usage / help ────────────────────────────────────────────────────── */
static void usage(const char *p)
{
    fprintf(stdout,
        "modbus_slave — Modbus RTU Slave Emulator\n"
        "Simulates up to %d independent slave devices on one serial port.\n"
        "Each slave responds to FC03 with %d holding registers.\n"
        "\n"
        "Usage:\n"
        "  %s -p <port> -b <baud> [options]\n"
        "  %s -k\n"
        "  %s -s\n"
        "\n"
        "Required:\n"
        "  -p <port>       COM port: COM1..COM9 or \\\\.\\COM10 and above\n"
        "  -b <baud>       baud rate: 1200 2400 4800 9600 19200 38400\n"
        "                             57600 115200 230400 460800 921600\n"
        "\n"
        "Optional:\n"
        "  -a <n>          number of slave devices: 1..%d (default: 1)\n"
        "                  slave IDs are assigned 1..n\n"
        "  -o <mode>       port framing (default: 8N1)\n"
        "                  databits : 7 or 8\n"
        "                  parity   : N=none  E=even  O=odd\n"
        "                  stopbits : 1 or 2\n"
        "                  examples : 8N1  8E1  8O1  7E1  8N2\n"
        "  -f <id>:<file>  bind register file to slave <id>\n"
        "                  can be specified multiple times\n"
        "                  file format: one integer (0..65535) per line\n"
        "                  missing lines -> 0, file absent -> all zeros\n"
        "                  slave without -f uses random values\n"
        "                  recommended path: C:\\temp\\s1.dat\n"
        "                  atomic update: write .tmp then move /Y\n"
        "  -t <sec>        max file age in seconds (default: 10)\n"
        "                  if file not updated within <sec>s -> return zeros\n"
        "                  use -t 0 to disable\n"
        "  -bg             run in background (detach from console)\n"
        "                  logs written to C:\\temp\\modbus_slave.log\n"
        "\n"
        "Control:\n"
        "  -k              stop background instance\n"
        "  -s              show background instance status\n"
        "\n"
        "------------------------------------------------------------\n"
        "Quick start examples:\n"
        "\n"
        "  # 1. Simplest: one slave (ID=1), random registers\n"
        "  #    poll with: mbpoll -m rtu -b 115200 -P none -a 1 -r 1 -c 20 COM4\n"
        "  %s -p COM3 -b 115200\n"
        "\n"
        "  # 2. Five slaves, all random (IDs 1..5)\n"
        "  #    poll with: mbpoll -m rtu -b 115200 -P none -a 3 -r 1 -c 20 COM4\n"
        "  %s -p COM3 -b 115200 -a 5\n"
        "\n"
        "  # 3. Two slaves with real data (IDs 1,2), rest random (IDs 3..5)\n"
        "  %s -p COM3 -b 115200 -a 5 -f 1:C:\\temp\\cpu.dat -f 2:C:\\temp\\time.dat\n"
        "\n"
        "  # 4. Background mode, 10 slaves (IDs 1..10)\n"
        "  %s -bg -p COM3 -b 115200 -a 10 -f 1:C:\\temp\\cpu.dat -t 15\n"
        "\n"
        "  # 5. Different port framing\n"
        "  %s -p COM3 -b 9600 -o 8E1 -a 3\n"
        "\n"
        "  # 6. Stop / status\n"
        "  %s -k\n"
        "  %s -s\n"
        "------------------------------------------------------------\n"
        "Register file format example (C:\\temp\\cpu.dat):\n"
        "  4523    <- reg 0: temperature * 100 = 45.23 C\n"
        "  1013    <- reg 1: pressure hPa\n"
        "  0       <- reg 2: reserved\n"
        "  ...     <- up to %d lines; missing lines -> 0\n"
        "\n",
        MAX_SLAVES, NUM_REGS,
        p, p, p,
        MAX_SLAVES,
        p, p, p, p, p, p, p,
        NUM_REGS);
}

/* ── main ────────────────────────────────────────────────────────────── */
int main(int argc, char *argv[])
{
    int do_kill = 0, do_status = 0, do_bg = 0, is_bg_child = 0;
    port_cfg_t cfg = { .dev=NULL, .baud=0, .databits=8, .parity='N', .stopbits=1 };

    memset(g_regs_file, 0, sizeof(g_regs_file));
    memset(g_stat,      0, sizeof(g_stat));

    /* Show help if no arguments given */
    if (argc == 1) { usage(argv[0]); return 0; }

    /* Manual argument parsing (getopt is not available on MSVC) */
    for (int i = 1; i < argc; i++) {
        if (strcmp(argv[i], "-p") == 0 && i+1 < argc) {
            cfg.dev = argv[++i];
        } else if (strcmp(argv[i], "-b") == 0 && i+1 < argc) {
            cfg.baud = atoi(argv[++i]);
        } else if (strcmp(argv[i], "-a") == 0 && i+1 < argc) {
            g_num_slaves = atoi(argv[++i]);
            if (g_num_slaves < 1 || g_num_slaves > MAX_SLAVES) {
                fprintf(stderr, "Error: -a must be 1..%d\n", MAX_SLAVES); return 1;
            }
        } else if (strcmp(argv[i], "-o") == 0 && i+1 < argc) {
            if (parse_mode(argv[++i], &cfg.databits, &cfg.parity, &cfg.stopbits) < 0) {
                fprintf(stderr, "Error: invalid mode '%s'\n", argv[i]); return 1;
            }
        } else if (strcmp(argv[i], "-f") == 0 && i+1 < argc) {
            char *arg = argv[++i];
            char *colon = strchr(arg, ':');
            /* On Windows paths contain ':' after drive letter — find the separator
             * between id and path: it's the ':' after the slave ID number */
            if (!colon || colon == arg) {
                fprintf(stderr, "Error: -f format is <id>:<file>\n"); return 1;
            }
            int fid = atoi(arg);
            if (fid < 1 || fid > MAX_SLAVES) {
                fprintf(stderr, "Error: -f slave id must be 1..%d\n", MAX_SLAVES); return 1;
            }
            g_regs_file[fid] = colon + 1;
        } else if (strcmp(argv[i], "-t") == 0 && i+1 < argc) {
            g_file_max_age = atoi(argv[++i]);
            if (g_file_max_age < 0) {
                fprintf(stderr, "Error: -t must be >= 0\n"); return 1;
            }
        } else if (strcmp(argv[i], "-bg") == 0) {
            do_bg = 1;
        } else if (strcmp(argv[i], "--bg-child") == 0) {
            is_bg_child = 1;  /* internal: we are the detached child */
        } else if (strcmp(argv[i], "-k") == 0) {
            do_kill = 1;
        } else if (strcmp(argv[i], "-s") == 0) {
            do_status = 1;
        } else if (strcmp(argv[i], "-h") == 0 || strcmp(argv[i], "--help") == 0) {
            usage(argv[0]); return 0;
        } else {
            fprintf(stderr, "Unknown option: %s\n", argv[i]);
            usage(argv[0]); return 1;
        }
    }

    /* Status query */
    if (do_status) {
        DWORD pid = read_pid();
        if (pid > 0) {
            HANDLE h = OpenProcess(PROCESS_QUERY_INFORMATION, FALSE, pid);
            if (h) {
                printf("running, PID=%lu\n", pid);
                CloseHandle(h);
                return 0;
            }
        }
        printf("not running\n");
        return 1;
    }

    /* Stop background instance */
    if (do_kill) {
        DWORD pid = read_pid();
        if (pid == 0) { fprintf(stderr, "No PID file: %s\n", PID_FILE); return 1; }
        HANDLE h = OpenProcess(PROCESS_TERMINATE, FALSE, pid);
        if (!h) { fprintf(stderr, "Cannot open process %lu\n", pid); return 1; }
        TerminateProcess(h, 0);
        CloseHandle(h);
        DeleteFileA(PID_FILE);
        printf("Terminated PID %lu\n", pid);
        return 0;
    }

    /* Validate required options */
    if (!cfg.dev)     { fprintf(stderr, "Error: -p <port> required\n\n"); usage(argv[0]); return 1; }
    if (cfg.baud <= 0){ fprintf(stderr, "Error: -b <baud> required\n\n"); usage(argv[0]); return 1; }

    /* Launch background child process if -bg and not already the child */
    if (do_bg && !is_bg_child) {
        go_background(argc, argv);
        /* go_background() calls exit() — never reaches here */
    }

    /* Background child: detach from console and open log file */
    if (is_bg_child) {
        FreeConsole();
        /* Ensure C:\temp exists and open log file */
        CreateDirectoryA("C:\\temp", NULL);
        g_logfile = fopen("C:\\temp\\modbus_slave.log", "a");
        write_pid();
    }

    /* Open serial port — Windows refuses if another process holds it */
    int serial_fd = open_port(&cfg);
    if (serial_fd < 0) return 1;

    /* Install Ctrl+C / close handler */
    SetConsoleCtrlHandler(ctrl_handler, TRUE);

    srand((unsigned)time(NULL) ^ (unsigned)GetCurrentProcessId());

    /* Log startup summary */
    LOG_I("Started: port=%s baud=%d mode=%d%c%d slaves=1..%d regs=%d pid=%lu",
          cfg.dev, cfg.baud, cfg.databits, cfg.parity, cfg.stopbits,
          g_num_slaves, NUM_REGS, (unsigned long)GetCurrentProcessId());
    for (int i = 1; i <= g_num_slaves; i++) {
        if (g_regs_file[i]) LOG_I("  slave %d: file=%s", i, g_regs_file[i]);
        else                 LOG_I("  slave %d: random", i);
    }
    if (g_file_max_age > 0)
        LOG_I("File max age: %d seconds (stale -> zeros, -t 0 disables)", g_file_max_age);
    else
        LOG_I("File max age: disabled");
    LOG_I("Stats every %d seconds", STAT_INTERVAL);

    /* Main loop */
    uint8_t buf[BUF_SIZE];
    time_t last_stat = time(NULL);

    while (g_running) {
        int n = read_frame(serial_fd, buf, sizeof(buf), &last_stat);
        if (n < 0) { LOG_E("Read error (code %lu)", GetLastError()); break; }
        if (n > 0)  handle_request(serial_fd, buf, n);
    }

    /* Clean shutdown */
    print_stats();
    LOG_I("Stopped");
    CloseHandle((HANDLE)(intptr_t)serial_fd);
    if (is_bg_child) { DeleteFileA(PID_FILE); if (g_logfile) fclose(g_logfile); }
    return 0;
}
