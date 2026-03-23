# modbus_slave — Эмулятор Modbus RTU Slave

Эмулирует до 30 независимых Modbus RTU slave устройств на одном последовательном порту.  
Каждое устройство отвечает на **FC03 (Read Holding Registers)** — 20 регистров.

Значения регистров могут быть:
- **Из файла** — текстовый файл, обновляемый внешним скриптом (реальные данные датчиков: I2C, 1-Wire, sysfs и т.д.)
- **Случайные** — случайные `uint16` значения (тестирование шины, проверка таймаутов мастера)

Работает на **Linux** (x86_64, aarch64) и **Windows** (x64).  
Создан для встраиваемых систем — один статический бинарь, без зависимостей, без записи на SD карту (tmpfs).

---

## Структура репозитория

```
modbus_slave/
├── bin/
│   ├── modbus_slave          # Linux x86_64 бинарь
│   ├── modbus_slave_aarch64  # Linux aarch64 статический бинарь (NAPI2, RK3568, RPi)
│   └── modbus_slave.exe      # Windows x64 бинарь
├── service/
│   └── modbus_slave.service  # systemd unit файл
└── src/
    ├── modbus_core.h         # Платформонезависимая логика Modbus
    ├── modbus_slave.c        # Linux платформа (termios, fork, syslog)
    └── modbus_slave_win.c    # Windows платформа (Win32 API)
```

---

## Сборка

### Linux x86_64
```bash
gcc -O2 -Wall -o bin/modbus_slave src/modbus_slave.c
```

### Linux aarch64 (статический — для NAPI2, RK3568, Raspberry Pi)
```bash
aarch64-linux-gnu-gcc -O2 -Wall -static -o bin/modbus_slave_aarch64 src/modbus_slave.c
```

### Windows x64 (кросс-компиляция с Linux)
```bash
x86_64-w64-mingw32-gcc -O2 -Wall -o bin/modbus_slave.exe src/modbus_slave_win.c
```

### Windows x64 (нативно в MinGW / MSYS2)
```bash
gcc -O2 -Wall -o modbus_slave.exe src/modbus_slave_win.c
```

### Windows x64 (MSVC)
```cmd
cl /O2 src\modbus_slave_win.c
```

---

## Использование

### Linux
```
modbus_slave -p <порт> -b <скорость> [-a <n>] [-o <режим>] [-f <id>:<файл>] [-t <сек>] [-d]
modbus_slave -k
modbus_slave -s
```

### Windows
```
modbus_slave.exe -p <порт> -b <скорость> [-a <n>] [-o <режим>] [-f <id>:<файл>] [-t <сек>] [-bg]
modbus_slave.exe -k
modbus_slave.exe -s
```

### Опции

| Опция | Описание |
|-------|----------|
| `-p <порт>` | Последовательный порт. Linux: `/dev/ttyUSB0`, `/dev/ttyS7`. Windows: `COM3`, `\\.\COM10` |
| `-b <скорость>` | Скорость: `1200`..`921600` |
| `-a <n>` | Количество slave устройств: `1`..`30` (по умолчанию `1`). ID назначаются `1..n` |
| `-o <режим>` | Параметры порта: `8N1` `8E1` `8O1` `7E1` `8N2` (по умолчанию `8N1`) |
| `-f <id>:<файл>` | Привязать файл регистров к slave `<id>`. Можно указывать несколько раз. Slave без `-f` использует случайные значения |
| `-t <сек>` | Максимальный возраст файла в секундах (по умолчанию `10`). Устаревший файл → нули. `-t 0` отключает проверку |
| `-d` | *(Linux)* Запустить как демон. Логи → syslog |
| `-bg` | *(Windows)* Запустить в фоне. Логи → `C:\temp\modbus_slave.log` |
| `-k` | Остановить запущенный демон / фоновый процесс |
| `-s` | Показать статус демона |

---

## Формат файла регистров

Одно целое число без знака (`0`..`65535`) на строку, до 20 строк.  
Недостающие строки → значение регистра `0`.  
Файл отсутствует → все регистры `0`.

Пример `/tmp/sensor1.dat`:
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
`4523` = 45.23 °C (соглашение: температура × 100)

### Атомарное обновление (Linux — tmpfs, SD карта не пишется)
```bash
printf "4523\n1013\n" > /tmp/s1.tmp && mv /tmp/s1.tmp /tmp/s1.dat
```

### Атомарное обновление (Windows)
```cmd
echo 4523 > C:\temp\s1.tmp
echo 1013 >> C:\temp\s1.tmp
move /Y C:\temp\s1.tmp C:\temp\s1.dat
```

---

## Примеры

### Linux — 5 устройств, два с реальными данными
```bash
./modbus_slave -p /dev/ttyUSB0 -b 115200 -a 5 \
    -f 1:/tmp/cpu.dat \
    -f 2:/tmp/time.dat
# устройства 3, 4, 5 → случайные значения
```

### Linux — демон на NAPI2
```bash
./modbus_slave -d -p /dev/ttyS7 -b 115200 -a 10 \
    -f 1:/tmp/cpu.dat \
    -t 15
```

### Windows — 3 устройства
```cmd
modbus_slave.exe -p COM4 -b 115200 -a 3 -f 1:C:\temp\s1.dat -f 2:C:\temp\s2.dat
```

### Windows — фоновый режим
```cmd
modbus_slave.exe -bg -p COM4 -b 115200 -a 3 -f 1:C:\temp\s1.dat
modbus_slave.exe -s
modbus_slave.exe -k
```

---

## Вспомогательные скрипты (Linux)

### Температура ядер CPU → регистры Modbus
```bash
chmod +x scripts/cpu_temp_to_modbus.sh
./scripts/cpu_temp_to_modbus.sh /tmp/cpu.dat 5 &
```
Читает из `/sys/class/thermal/thermal_zone*/temp`.  
Значение в регистре: `°C × 100` (например `4523` = 45.23 °C).

### Текущее время → регистры Modbus
```bash
chmod +x scripts/time_to_modbus.sh
./scripts/time_to_modbus.sh /tmp/time.dat 1 &
```

| Регистр | Значение | Диапазон |
|---------|----------|----------|
| 0 | Часы | 0..23 |
| 1 | Минуты | 0..59 |
| 2 | Секунды | 0..59 |
| 3 | День | 1..31 |
| 4 | Месяц | 1..12 |
| 5 | Год | 2025.. |
| 6 | День недели | 1=Пн..7=Вс |
| 7..19 | Резерв | 0 |

---

## systemd сервис (Linux)

```bash
sudo cp bin/modbus_slave /usr/local/bin/
sudo cp service/modbus_slave.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable modbus_slave
sudo systemctl start modbus_slave
```

Отредактируй `/etc/systemd/system/modbus_slave.service` — укажи нужный порт, скорость и файлы `-f`.

```bash
# Просмотр логов
journalctl -u modbus_slave -f
```

---

## Защита от устаревших данных

Если скрипт-источник данных упал, файл регистров перестаёт обновляться.  
Без защиты мастер будет бесконечно читать устаревшие значения.

С параметром `-t 10` (по умолчанию): если файл не обновлялся более 10 секунд,  
все его регистры возвращают **ноль** — неисправность сразу видна мастеру.

```
file /tmp/cpu.dat is stale (45s > 10s), returning zeros
```

Отключить: `-t 0` (если файлы обновляются редко).

---

## Детали протокола

| Параметр | Значение |
|----------|----------|
| Протокол | Modbus RTU |
| Поддерживаемые FC | FC03 — Read Holding Registers |
| Количество регистров | 20 на slave (адреса 0..19) |
| Максимум устройств | 30 (ID 1..30) |
| Разделитель фреймов | 10 мс тишина между символами |
| CRC | CRC16 (полином 0xA001) |

---

## Протестировано на

| Платформа | ОС | Примечание |
|-----------|----|------------|
| x86_64 | Ubuntu 24 | разработка / тестирование |
| aarch64 | Armbian (NAPI2 / RK3568J) | основная цель |
| x64 | Windows 10/11 | через MinGW |

---

## Платы NAPI

Проект разработан командой [NAPI Lab](https://github.com/napilab) и в первую очередь  
тестируется на **промышленных одноплатных компьютерах NAPI** на базе Rockchip SoC.

[![NAPI Boards](https://img.shields.io/badge/NAPI-промышленные%20SBC-blue)](https://github.com/napilab/napi-boards)

Если вы ищете надёжную аппаратную платформу для запуска `modbus_slave` в production —  
посмотрите линейку плат NAPI:

👉 **[github.com/napilab/napi-boards](https://github.com/napilab/napi-boards)**

- **NAPI2** — RK3568J, RS-485 на борту, Armbian
- **NAPI-C** — RK3308, компактный, промышленного класса

---

## Авторы

**Dmitrii Novikov** ([@dmnovikov](https://github.com/dmnovikov))  
[NAPI Lab](https://github.com/napilab)

**Claude** (Anthropic)  
ИИ-ассистент и соавтор — архитектура, код, документация

---

## Лицензия

MIT