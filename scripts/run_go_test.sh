#!/usr/bin/env bash
# run_go_test.sh — запуск `go test` с диагностируемым логом.
#
# Зачем (R66c, 2026-09-22). Шаги CI гоняли `go test ... 2>&1 | tail -N`,
# чтобы лог был читаемым. Проблема двойная:
#   1) без pipefail падение тестов не роняло шаг вообще (workflow был зелёным
#      при красных тестах — прогон 35656811670);
#   2) даже после включения pipefail `tail` прячет САМО падение: для
#      многосоставного `./internal/...` строка `FAIL <pkg>` печатается в
#      середине потока, и в последних 50 строках её нет — по логу нельзя
#      понять, какой пакет упал (именно так потерялся прогон 35739854906).
#
# Теперь: при успехе печатаем хвост, при падении — сводку FAIL/panic и хвост,
# и всегда возвращаем код `go test`.
#
# Использование:
#   scripts/run_go_test.sh "internal/balancer" -race -tags llama_stub -timeout 300s ./internal/balancer/
set -o pipefail

label="$1"
shift

if [ -z "$label" ]; then
  echo "usage: $0 <label> <go test args...>" >&2
  exit 2
fi

out="$(mktemp)"
trap 'rm -f "$out"' EXIT

go test "$@" >"$out" 2>&1
status=$?

if [ "$status" -eq 0 ]; then
  echo "=== ${label}: OK ==="
  tail -10 "$out"
  exit 0
fi

echo "=== ${label}: FAILED (exit ${status}) ==="
echo "--- упавшие пакеты и тесты (с сообщениями об ошибках) ---"
# Печатаем не только строки FAIL/--- FAIL:, но и следующие за ними
# отступленные строки — это и есть t.Errorf/t.Fatalf-сообщения и фреймы
# детектора гонок. Без них по логу нельзя понять, ЧТО именно упало
# (в прогоне 35746620782 от internal/balancer остались только два
# '--- FAIL:' без единого сообщения).
# R245 (2026-10-26): гонка НЕ печатает `--- FAIL:` — детектор выводит
# `WARNING: DATA RACE` со стеком, а тест падает позже строкой
# `race detected during execution of test`. Раньше этот заголовок не попадал в
# сводку, и по логу ubuntu-джобы (прогон 40326149098) оставались только
# `FAIL <pkg>` и хвост чужих логов — имя упавшего теста терялось.
#
# R458 (2026-11-06): печатаем и строки ПЕРЕД `--- FAIL:`. Go пишет сообщение
# t.Errorf/t.Fatalf ДО строки `--- FAIL:` (это `t.Log`-порядок), поэтому сводка
# без контекста снова теряла самого главного — ЧТО именно не сошлось. Живой
# случай: прогон 42184010804, `TestReload_DegradedHeaderOnCPUOnly` — в логе
# осталось только имя теста, и причину пришлось бы искать заново.
# Держим «окно» из предыдущих строк и печатаем его при срабатывании FAIL.
awk '
  { prev[NR % 12] = $0 }
  /^(FAIL|--- FAIL:|panic:|# )/ {
    if ($0 ~ /^--- FAIL:/) {
      print "    … контекст перед этим тестом …"
      start = NR - 11
      if (start < 1) { start = 1 }
      for (i = start; i < NR; i++) {
        idx = i % 12
        if (idx in prev) { print prev[idx] }
      }
    }
    show = 1; print; next
  }
  /^WARNING: DATA RACE/          { race=45; show=0; print; next }
  race > 0                       { print; race--; next }
  show && /^[[:space:]]/         { print; next }
  { show=0 }
' "$out" | head -260
echo "--- последние 60 строк вывода ---"
tail -60 "$out"
exit "$status"
