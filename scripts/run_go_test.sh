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
echo "--- упавшие пакеты и тесты ---"
grep -E '^(FAIL|--- FAIL:|panic:|# )' "$out" | head -100 || true
echo "--- последние 60 строк вывода ---"
tail -60 "$out"
exit "$status"
