#!/bin/sh
# time_to_modbus.sh — пишет текущее время в файл регистров Modbus
#
# Регистры:
#   0 — часы    (0..23)
#   1 — минуты  (0..59)
#   2 — секунды (0..59)
#   3 — день    (1..31)
#   4 — месяц   (1..12)
#   5 — год     (например 2025)
#   6 — день недели (1=Пн .. 7=Вс)
#   7..9 — 0 (резерв)
#
# Использование:
#   ./time_to_modbus.sh [output_file] [interval_sec]
#
# По умолчанию:
#   output_file  = /tmp/time.dat
#   interval_sec = 1
#
# Запуск с modbus_slave:
#   ./modbus_slave -p /dev/ttyS7 -b 115200 -f 1:/tmp/time.dat

OUTPUT="${1:-/tmp/time.dat}"
INTERVAL="${2:-1}"
TMPFILE="${OUTPUT}.tmp"

echo "time_to_modbus: output=$OUTPUT interval=${INTERVAL}s"

while true; do
    printf "%s\n%s\n%s\n%s\n%s\n%s\n%s\n0\n0\n0\n" \
        "$(date '+%H')" \
        "$(date '+%M')" \
        "$(date '+%S')" \
        "$(date '+%d')" \
        "$(date '+%m')" \
        "$(date '+%Y')" \
        "$(date '+%u')" \
        > "$TMPFILE"

    mv "$TMPFILE" "$OUTPUT"
    sleep "$INTERVAL"
done
