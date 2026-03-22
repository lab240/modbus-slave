/*
 * modbus_slave.c — Linux platform entry point for Modbus RTU slave emulator
 *
 * Provides all Linux/POSIX-specific functionality:
 *   - Serial port open/configure via termios
 *   - Frame reading via select() with inter-character timeout
 *   - Port-busy detection via /proc filesystem
 *   - Daemon mode via double-fork technique
 *   - PID file management (/var/run or /tmp fallback)
 *   - Logging via syslog / stdout
 *   - Signal handling (SIGTERM, SIGINT)
 *   - getopt() command-line parsing
 *
 * Platform-independent Modbus logic is in modbus_core.h.
 *
 * Build (x86_64):
 *   gcc -O2 -Wall -o modbus_slave modbus_slave.c
 *
 * Build (aarch64 static — e.g. NAPI2 / RK3568):
 *   aarch64-linux-gnu-gcc -O2 -Wall -static -o modbus_slave modbus_slave.c
 *
 * Usage:
 *   ./modbus_slave -p <port> -b <baud> [-a <n>] [-o <mode>] [-f <id>:<file>] [-t <sec>] [-d]
 *   ./modbus_slave -k
 *   ./modbus_slave -s
 *
 * Options:
 *   -p <port>       serial port, e.g. /dev/ttyUSB0 or /dev/ttyS7
 *   -b <baud>       baud rate: 1200..921600
 *   -a <n>          number of slave devices: 1..30 (default 1)
 *   -o <mode>       port framing: 8N1 8E1 8O1 7E1 8N2 ... (default 8N1)
 *   -f <id>:<file>  bind register file to slave id (repeatable)
 *   -t <sec>        max file age in seconds, 0=disable (default 10)
 *   -d              run as background daemon (logs -> syslog)
 *   -k              stop running daemon
 *   -s              show daemon status
 */

#include <stdio.h>
#include <stdlib.h>
#include <stdint.h>
#include <string.h>
#include <unistd.h>
#include <fcntl.h>
#include <termios.h>
#include <time.h>
#include <errno.h>
#include <signal.h>
#include <sys/stat.h>
#include <sys/types.h>
#include <syslog.h>
#include <dirent.h>
#include <limits.h>

/* ── Global state (shared with modbus_core.h) ───────────────────────── */
#include "modbus_core.h"

/* Override default LOG macros with syslog-aware versions */
#undef LOG_I
#undef LOG_E
#define LOG_I(fmt, ...) do { \
    if (g_daemon) syslog(LOG_INFO, fmt, ##__VA_ARGS__); \
    else { printf(fmt "\n", ##__VA_ARGS__); fflush(stdout); } \
} while(0)
#define LOG_E(fmt, ...) do { \
    if (g_daemon) syslog(LOG_ERR, fmt, ##__VA_ARGS__); \
    else fprintf(stderr, fmt "\n", ##__VA_ARGS__); \
} while(0)

/* ── Global state definitions ───────────────────────────────────────── */
slave_stat_t g_stat[MAX_SLAVES + 1];
int          g_num_slaves  = SLAVES_DEFAULT;
const char  *g_regs_file[MAX_SLAVES + 1];
int          g_file_max_age = 10;
int          g_daemon       = 0;

/* Graceful shutdown flag — set by SIGTERM / SIGINT */
static volatile sig_atomic_t g_running = 1;
static void sig_handler(int sig) { (void)sig; g_running = 0; }

/* ── PID file paths ─────────────────────────────────────────────────── */
#define PID_FILE_RUN  "/var/run/modbus_slave.pid"
#define PID_FILE_TMP  "/tmp/modbus_slave.pid"
static const char *g_pid_file = PID_FILE_RUN;

/* ── serial_write() — platform write wrapper required by modbus_core.h ─
 * On Linux, write() on a serial fd is effectively atomic for small frames.
 * Returns number of bytes written, or -1 on error.
 */
int serial_write(int fd, const uint8_t *buf, int len)
{
    return (int)write(fd, buf, (size_t)len);
}

/* ── Check if serial port is already held by another process ────────────
 * Walks /proc/<pid>/fd/ symlinks looking for a match with the target device.
 * Uses realpath() to resolve symlinks like /dev/serial/by-id/...
 * No root required, no external tools needed.
 * Returns PID of holding process or 0 if port is free.
 */
static pid_t check_port_busy(const char *dev)
{
    char real_dev[PATH_MAX];
    if (!realpath(dev, real_dev)) {
        strncpy(real_dev, dev, sizeof(real_dev) - 1);
        real_dev[sizeof(real_dev) - 1] = '\0';
    }

    DIR *proc_dir = opendir("/proc");
    if (!proc_dir) return 0;

    struct dirent *proc_ent;
    pid_t found = 0;

    while (!found && (proc_ent = readdir(proc_dir)) != NULL) {
        char *endp;
        long pid = strtol(proc_ent->d_name, &endp, 10);
        if (*endp != '\0' || pid <= 0) continue;
        if ((pid_t)pid == getpid()) continue;

        char fd_dir[64];
        snprintf(fd_dir, sizeof(fd_dir), "/proc/%ld/fd", pid);
        DIR *fd_dir_h = opendir(fd_dir);
        if (!fd_dir_h) continue;

        struct dirent *fd_ent;
        while ((fd_ent = readdir(fd_dir_h)) != NULL) {
            if (fd_ent->d_name[0] == '.') continue;
            char fd_path[PATH_MAX], link_target[PATH_MAX];
            snprintf(fd_path, sizeof(fd_path), "/proc/%ld/fd/%s",
                     pid, fd_ent->d_name);
            ssize_t len = readlink(fd_path, link_target, sizeof(link_target) - 1);
            if (len <= 0) continue;
            link_target[len] = '\0';
            if (strcmp(link_target, real_dev) == 0) {
                found = (pid_t)pid;
                break;
            }
        }
        closedir(fd_dir_h);
    }
    closedir(proc_dir);
    return found;
}

/* Read process name from /proc/<pid>/comm */
static void get_proc_name(pid_t pid, char *buf, size_t buflen)
{
    char path[64];
    snprintf(path, sizeof(path), "/proc/%d/comm", (int)pid);
    FILE *f = fopen(path, "r");
    if (!f) { snprintf(buf, buflen, "?"); return; }
    if (!fgets(buf, (int)buflen, f)) snprintf(buf, buflen, "?");
    fclose(f);
    size_t l = strlen(buf);
    if (l > 0 && buf[l-1] == '\n') buf[l-1] = '\0';
}

/* ── Open and configure serial port ─────────────────────────────────────
 * Opens with O_NONBLOCK to avoid blocking on DCD, then switches back to
 * blocking after termios configuration. Uses select() in read_frame() for
 * precise timeout control instead of VTIME.
 */
static int open_port(const port_cfg_t *cfg)
{
    int fd = open(cfg->dev, O_RDWR | O_NOCTTY | O_NONBLOCK);
    if (fd < 0) { LOG_E("open %s: %s", cfg->dev, strerror(errno)); return -1; }

    struct termios tty;
    if (tcgetattr(fd, &tty) < 0) {
        LOG_E("tcgetattr: %s", strerror(errno)); close(fd); return -1;
    }

    speed_t speed;
    switch (cfg->baud) {
        case 1200:   speed = B1200;   break;
        case 2400:   speed = B2400;   break;
        case 4800:   speed = B4800;   break;
        case 9600:   speed = B9600;   break;
        case 19200:  speed = B19200;  break;
        case 38400:  speed = B38400;  break;
        case 57600:  speed = B57600;  break;
        case 115200: speed = B115200; break;
        case 230400: speed = B230400; break;
        case 460800: speed = B460800; break;
        case 921600: speed = B921600; break;
        default:
            LOG_E("Unsupported baud %d", cfg->baud); close(fd); return -1;
    }
    cfsetispeed(&tty, speed);
    cfsetospeed(&tty, speed);

    tty.c_cflag &= ~CSIZE;
    tty.c_cflag |= (cfg->databits == 7) ? CS7 : CS8;
    tty.c_cflag &= ~(PARENB | PARODD);
    if      (cfg->parity == 'E') tty.c_cflag |= PARENB;
    else if (cfg->parity == 'O') tty.c_cflag |= PARENB | PARODD;
    if (cfg->stopbits == 2) tty.c_cflag |= CSTOPB;
    else                    tty.c_cflag &= ~CSTOPB;
    tty.c_cflag &= ~CRTSCTS;
    tty.c_cflag |= CREAD | CLOCAL;
    tty.c_iflag = tty.c_oflag = tty.c_lflag = 0;
    tty.c_cc[VMIN] = tty.c_cc[VTIME] = 0;

    tcflush(fd, TCIFLUSH);
    if (tcsetattr(fd, TCSANOW, &tty) < 0) {
        LOG_E("tcsetattr: %s", strerror(errno)); close(fd); return -1;
    }
    int flags = fcntl(fd, F_GETFL, 0);
    fcntl(fd, F_SETFL, flags & ~O_NONBLOCK);
    return fd;
}

/* ── PID file management ────────────────────────────────────────────────
 * Prefer /var/run (FHS standard). Fall back to /tmp if not writable
 * (non-root user). Both paths are checked by read_pid() so -k/-s work
 * regardless of which was used at daemon start.
 */
static void select_pid_file(void)
{
    FILE *f = fopen(PID_FILE_RUN, "w");
    if (f) { fclose(f); unlink(PID_FILE_RUN); g_pid_file = PID_FILE_RUN; }
    else   { g_pid_file = PID_FILE_TMP; }
}

static int write_pid(void)
{
    FILE *f = fopen(g_pid_file, "w");
    if (!f) { LOG_E("write pid %s: %s", g_pid_file, strerror(errno)); return -1; }
    fprintf(f, "%d\n", (int)getpid());
    fclose(f);
    return 0;
}

static pid_t read_pid(void)
{
    const char *paths[] = { PID_FILE_RUN, PID_FILE_TMP, NULL };
    for (int i = 0; paths[i]; i++) {
        FILE *f = fopen(paths[i], "r");
        if (!f) continue;
        pid_t pid = -1;
        if (fscanf(f, "%d", &pid) != 1) pid = -1;
        fclose(f);
        if (pid > 0) { g_pid_file = paths[i]; return pid; }
    }
    return -1;
}

/* ── Daemonize via double-fork ───────────────────────────────────────────
 * Uses a pipe to notify the parent process of success/failure so the user
 * sees an error in the terminal if startup fails (port busy, bad config).
 */
static int g_pipe_wr = -1;

static void daemon_ok(void)
{
    if (g_pipe_wr >= 0) {
        char c = 0;
        if (write(g_pipe_wr, &c, 1)) {}
        close(g_pipe_wr);
        g_pipe_wr = -1;
    }
}

static int daemonize(void)
{
    int pipefd[2];
    if (pipe(pipefd) < 0) return -1;

    pid_t pid = fork();
    if (pid < 0) return -1;
    if (pid > 0) {
        /* Parent: wait for daemon's go signal */
        close(pipefd[1]);
        char c = 1;
        ssize_t n = read(pipefd[0], &c, 1);
        close(pipefd[0]);
        if (n == 1 && c == 0) exit(EXIT_SUCCESS);
        fprintf(stderr, "modbus_slave: daemon failed to start\n");
        exit(EXIT_FAILURE);
    }

    close(pipefd[0]);
    g_pipe_wr = pipefd[1];

    if (setsid() < 0) return -1;

    pid = fork();
    if (pid < 0) return -1;
    if (pid > 0) exit(EXIT_SUCCESS);

    if (chdir("/") < 0) return -1;
    umask(0);

    int devnull = open("/dev/null", O_RDWR);
    if (devnull >= 0) {
        dup2(devnull, STDIN_FILENO);
        dup2(devnull, STDOUT_FILENO);
        dup2(devnull, STDERR_FILENO);
        if (devnull > STDERR_FILENO) close(devnull);
    }
    return 0;
}

/* ── Read one complete Modbus RTU frame from the serial port ────────────
 * Uses select() with two different timeouts:
 *   500 ms — waiting for the start of a new frame (idle bus)
 *    10 ms — inter-character gap (frame delimiter in Modbus RTU)
 * When a 10 ms silence follows received bytes, the frame is considered
 * complete and returned. This matches the Modbus RTU 3.5-char rule at
 * 115200 baud (3.5 chars ~ 0.3 ms); we use 10 ms for safety margin.
 *
 * Also handles periodic statistics output (no separate thread needed).
 * Returns: byte count on complete frame, 0 on signal/empty, -1 on error.
 */
static int read_frame(int fd, uint8_t *buf, int maxlen, time_t *last_stat)
{
    int  total   = 0;
    char started = 0;
    fd_set fds;
    struct timeval tv;

    while (g_running) {
        /* Check statistics timer on each iteration */
        time_t now = time(NULL);
        if (now - *last_stat >= STAT_INTERVAL) {
            print_stats();
            *last_stat = now;
        }

        FD_ZERO(&fds);
        FD_SET(fd, &fds);
        tv.tv_sec  = 0;
        tv.tv_usec = started ? 10000 : 500000;

        int r = select(fd + 1, &fds, NULL, NULL, &tv);
        if (r < 0) {
            if (errno == EINTR) return 0;
            LOG_E("select: %s", strerror(errno));
            return -1;
        }
        if (r == 0) {
            if (started && total >= 4) return total;
            total = 0; started = 0;
            continue;
        }

        int n = read(fd, buf + total, maxlen - total);
        if (n <= 0) return -1;
        total  += n;
        started = 1;
        if (total >= maxlen) return total;
    }
    return 0;
}

/* ── Usage / help ────────────────────────────────────────────────────── */
static void usage(const char *p)
{
    fprintf(stderr,
        "Modbus RTU slave emulator — simulate up to %d devices on one port\n\n"
        "Usage:\n"
        "  %s -p <port> -b <baud> [-a <n>] [-o <mode>] [-f <id>:<file>] [-t <sec>] [-d]\n"
        "  %s -k\n"
        "  %s -s\n\n"
        "Options:\n"
        "  -p <port>       serial port, e.g. /dev/ttyUSB0 or /dev/ttyS7\n"
        "  -b <baud>       baud rate: 1200..921600\n"
        "  -a <n>          number of slave devices: 1..%d (default %d)\n"
        "                  slave IDs assigned 1..n\n"
        "  -o <mode>       port framing: <databits><parity><stopbits>\n"
        "                  databits : 7 or 8\n"
        "                  parity   : N (none) | E (even) | O (odd)\n"
        "                  stopbits : 1 or 2\n"
        "                  default  : 8N1\n"
        "  -f <id>:<file>  bind register file to slave <id> (repeatable)\n"
        "                  file: one uint16 per line, /tmp recommended (tmpfs)\n"
        "                  slave without -f -> random values\n"
        "                  file absent or short -> missing regs are 0\n"
        "                  atomic update: write .tmp then mv\n"
        "  -t <sec>        max register file age in seconds (default: 10)\n"
        "                  stale file -> all zeros; -t 0 disables check\n"
        "  -d              run as background daemon (logs -> syslog)\n"
        "  -k              stop running daemon\n"
        "  -s              show daemon status\n\n"
        "Each slave responds to FC03 with %d registers (addr 0..%d).\n"
        "Statistics logged every %d seconds.\n\n"
        "Examples:\n"
        "  %s -p /dev/ttyUSB0 -b 115200\n"
        "  %s -p /dev/ttyUSB0 -b 115200 -a 5 -f 1:/tmp/s1.dat -f 2:/tmp/s2.dat\n"
        "  %s -d -p /dev/ttyS7 -b 115200 -a 10 -f 1:/tmp/cpu.dat -t 15\n"
        "  %s -k\n",
        MAX_SLAVES, p, p, p,
        MAX_SLAVES, SLAVES_DEFAULT,
        NUM_REGS, NUM_REGS - 1, STAT_INTERVAL,
        p, p, p, p);
}

/* ── main ────────────────────────────────────────────────────────────── */
int main(int argc, char *argv[])
{
    int do_kill = 0, do_status = 0;
    port_cfg_t cfg = { .dev=NULL, .baud=0, .databits=8, .parity='N', .stopbits=1 };

    memset(g_regs_file, 0, sizeof(g_regs_file));
    memset(g_stat,      0, sizeof(g_stat));

    int opt;
    while ((opt = getopt(argc, argv, "p:b:a:o:f:t:dks")) != -1) {
        switch (opt) {
            case 'p': cfg.dev  = optarg; break;
            case 'b': cfg.baud = atoi(optarg); break;
            case 'a':
                g_num_slaves = atoi(optarg);
                if (g_num_slaves < 1 || g_num_slaves > MAX_SLAVES) {
                    fprintf(stderr, "Error: -a must be 1..%d\n", MAX_SLAVES); return 1;
                }
                break;
            case 'o':
                if (parse_mode(optarg, &cfg.databits, &cfg.parity, &cfg.stopbits) < 0) {
                    fprintf(stderr, "Error: invalid mode '%s'\n", optarg); return 1;
                }
                break;
            case 'f': {
                char *colon = strchr(optarg, ':');
                if (!colon || colon == optarg) {
                    fprintf(stderr, "Error: -f format is <id>:<file>\n"); return 1;
                }
                int fid = atoi(optarg);
                if (fid < 1 || fid > MAX_SLAVES) {
                    fprintf(stderr, "Error: -f slave id must be 1..%d\n", MAX_SLAVES); return 1;
                }
                g_regs_file[fid] = colon + 1;
                break;
            }
            case 't':
                g_file_max_age = atoi(optarg);
                if (g_file_max_age < 0) {
                    fprintf(stderr, "Error: -t must be >= 0\n"); return 1;
                }
                break;
            case 'd': g_daemon  = 1; break;
            case 'k': do_kill   = 1; break;
            case 's': do_status = 1; break;
            default:  usage(argv[0]); return 1;
        }
    }

    /* Status query */
    if (do_status) {
        pid_t pid = read_pid();
        int alive = (pid > 0 && kill(pid, 0) == 0);
        printf("%s\n", alive ? "running" : "not running");
        if (alive) printf("PID=%d\n", (int)pid);
        return alive ? 0 : 1;
    }

    /* Stop daemon */
    if (do_kill) {
        pid_t pid = read_pid();
        if (pid <= 0) {
            fprintf(stderr, "No PID file found (%s or %s)\n", PID_FILE_RUN, PID_FILE_TMP);
            return 1;
        }
        if (kill(pid, SIGTERM) == 0) { printf("Sent SIGTERM to PID %d\n", (int)pid); return 0; }
        fprintf(stderr, "kill(%d): %s\n", (int)pid, strerror(errno));
        return 1;
    }

    /* Validate required options */
    if (!cfg.dev)     { fprintf(stderr, "Error: -p <port> required\n\n"); usage(argv[0]); return 1; }
    if (cfg.baud <= 0){ fprintf(stderr, "Error: -b <baud> required\n\n"); usage(argv[0]); return 1; }

    /* Pre-flight: port busy check */
    {
        pid_t busy = check_port_busy(cfg.dev);
        if (busy > 0) {
            char name[64];
            get_proc_name(busy, name, sizeof(name));
            fprintf(stderr, "Error: %s is held by '%s' (PID %d)\n"
                            "       Stop it first or choose another port.\n",
                    cfg.dev, name, (int)busy);
            return 1;
        }
    }

    /* Open port before fork so errors are visible in terminal */
    int serial_fd = open_port(&cfg);
    if (serial_fd < 0) return 1;

    /* Daemonize if requested */
    if (g_daemon) {
        select_pid_file();
        if (daemonize() < 0) {
            fprintf(stderr, "daemonize: %s\n", strerror(errno));
            close(serial_fd); return 1;
        }
        openlog("modbus_slave", LOG_PID | LOG_NDELAY, LOG_DAEMON);
        if (write_pid() < 0) { closelog(); close(serial_fd); return 1; }
        daemon_ok();
    }

    /* Signal handlers */
    struct sigaction sa;
    memset(&sa, 0, sizeof(sa));
    sa.sa_handler = sig_handler;
    sigemptyset(&sa.sa_mask);
    sigaction(SIGTERM, &sa, NULL);
    sigaction(SIGINT,  &sa, NULL);
    signal(SIGHUP, SIG_IGN);

    srand((unsigned)time(NULL) ^ (unsigned)getpid());

    /* Log startup summary */
    LOG_I("Started: port=%s baud=%d mode=%d%c%d slaves=1..%d regs=%d pid=%d",
          cfg.dev, cfg.baud, cfg.databits, cfg.parity, cfg.stopbits,
          g_num_slaves, NUM_REGS, (int)getpid());
    for (int i = 1; i <= g_num_slaves; i++) {
        if (g_regs_file[i]) LOG_I("  slave %d: file=%s", i, g_regs_file[i]);
        else                 LOG_I("  slave %d: random", i);
    }
    if (g_file_max_age > 0)
        LOG_I("File max age: %d seconds (stale -> zeros, -t 0 disables)", g_file_max_age);
    else
        LOG_I("File max age: disabled (-t 0)");
    LOG_I("Stats every %d seconds", STAT_INTERVAL);

    /* Main loop */
    uint8_t buf[BUF_SIZE];
    time_t last_stat = time(NULL);

    while (g_running) {
        int n = read_frame(serial_fd, buf, sizeof(buf), &last_stat);
        if (n < 0) { LOG_E("Read error: %s", strerror(errno)); break; }
        if (n > 0)  handle_request(serial_fd, buf, n);
    }

    /* Clean shutdown */
    print_stats();
    LOG_I("Stopped, pid=%d", (int)getpid());
    close(serial_fd);
    if (g_daemon) { unlink(g_pid_file); closelog(); }
    return 0;
}
