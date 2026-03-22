#!/bin/sh
# cpu_temp_to_modbus.sh — пишет температуры ядер CPU в файл регистров Modbus
#
# Формат файла: одно целое число на строку (°C * 100, uint16)
# Пример: 4523 = 45.23°C
#
# Использование:
#   ./cpu_temp_to_modbus.sh [output_file] [interval_sec]
#
# По умолчанию:
#   output_file  = /tmp/cpu_temp.dat
#   interval_sec = 5
#
# Запуск с modbus_slave:
#   ./modbus_slave -p /dev/ttyS7 -b 115200 -f 1:/tmp/cpu_temp.dat
#
# Запуск в фоне:
#   ./cpu_temp_to_modbus.sh &

OUTPUT="${1:-/tmp/cpu_temp.dat}"
INTERVAL="${2:-5}"
TMPFILE="${OUTPUT}.tmp"
NUM_REGS=10

# ── Найти источник температур ────────────────────────────────────────

find_temps() {
    temps=""

    # 1. /sys/class/thermal/thermal_zone* — универсально (Linux, NAPI2, RPi)
    for zone in /sys/class/thermal/thermal_zone*/temp; do
        [ -f "$zone" ] || continue
        raw=$(cat "$zone" 2>/dev/null) || continue
        # значение в милликельвинах/милиградусах → делим на 10 чтобы получить °C*100
        val=$((raw / 10))
        # отфильтруем невалидные (0 или слишком большие)
        [ "$val" -gt 0 ] && [ "$val" -lt 15000 ] || continue
        temps="$temps $val"
    done

    # 2. /sys/devices/platform/coretemp.*/hwmon/hwmon*/temp*_input — x86 coretemp
    if [ -z "$temps" ]; then
        for t in /sys/devices/platform/coretemp.*/hwmon/hwmon*/temp*_input; do
            [ -f "$t" ] || continue
            raw=$(cat "$t" 2>/dev/null) || continue
            val=$((raw / 10))
            [ "$val" -gt 0 ] && [ "$val" -lt 15000 ] || continue
            temps="$temps $val"
        done
    fi

    # 3. sensors -u (lm-sensors) — fallback если есть
    if [ -z "$temps" ] && command -v sensors >/dev/null 2>&1; then
        while IFS= read -r line; do
            case "$line" in
                *temp*_input*)
                    raw=$(echo "$line" | awk -F: '{print $2}' | tr -d ' .')
                    # sensors выдаёт например "45.230" → убираем точку → 45230 → /100 → 4523
                    val=$((raw / 100))
                    [ "$val" -gt 0 ] && [ "$val" -lt 15000 ] || continue
                    temps="$temps $val"
                    ;;
            esac
        done << EOF
$(sensors -u 2>/dev/null)
EOF
    fi

    echo "$temps"
}

# ── Главный цикл ─────────────────────────────────────────────────────

echo "cpu_temp_to_modbus: output=$OUTPUT interval=${INTERVAL}s"

while true; do
    temps=$(find_temps)

    if [ -z "$temps" ]; then
        echo "WARNING: no temperature sensors found" >&2
        # пишем нули чтобы modbus slave видел что данных нет
        i=0
        while [ $i -lt $NUM_REGS ]; do
            echo "0"
            i=$((i + 1))
        done > "$TMPFILE"
    else
        # Записываем до NUM_REGS значений, остаток дополняем нулями
        count=0
        > "$TMPFILE"
        for val in $temps; do
            [ $count -ge $NUM_REGS ] && break
            echo "$val" >> "$TMPFILE"
            count=$((count + 1))
        done
        # добиваем нулями до NUM_REGS
        while [ $count -lt $NUM_REGS ]; do
            echo "0" >> "$TMPFILE"
            count=$((count + 1))
        done
    fi

    # Атомарная замена — mv на tmpfs атомарна
    mv "$TMPFILE" "$OUTPUT"

    # Лог первой строки для контроля
    first=$(head -1 "$OUTPUT")
    echo "$(date '+%H:%M:%S') written $(wc -l < "$OUTPUT") regs, reg0=${first} ($(( first / 100 )).$(( first % 100 ))°C)"

    sleep "$INTERVAL"
done
