//go:build llama_stub

// llamacpp_sources_r630_test.go — R630 (2026-11-19): единый способ прочитать
// исходники транспортного слоя llama.cpp для сторожей.
//
// ЗАЧЕМ. `llamacpp_transport.go` разложен на пять файлов (транспорт, чтение
// апстрима, перевод кадров, хвост стрима, «вызов апстрима»). Сторожа, которые
// читают ОДИН файл по имени, после такого переноса краснеют не потому, что
// свойство вернулось, а потому, что код переехал:
//
//	TestR493_TransportPassesStatsWindows        — не прочитать llamacpp_transport.go
//	TestR360_CancelPathsPassRequestID_STEWARD   — не найдено ни одного вызова отмены
//	TestR298_CoveredPathsActuallyUseTheSharedWatch — нет newStreamStallWatch
//
// Это уже третий раз за цикл (R623 — cppworker, R626/R627 — handlers_chat, R628 —
// handlers_model). Здесь тот же приём, что в chat_sources_r627_test.go: сторож
// читает ВСЮ семью файлов, а не один файл, поэтому следующий перенос внутри
// слоя транспорта его не сломает.
//
// ВАЖНО: helper не ослабляет проверки. Он склеивает текст всех боевых
// `llamacpp_*.go`, и все прежние требования (вызовы с request_id, окна
// статистики, подключение сторожа тишины) проверяются по объединённому тексту.
package balancer

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// llamaCppTransportLayerFilesR630 — файлы ОДНОГО транспортного слоя: те, между
// которыми разложен бывший `llamacpp_transport.go`.
//
// ПОЧЕМУ СПИСОК, А НЕ ШАБЛОН `llamacpp_*`. Первая версия helper-а собирала все
// `llamacpp_*.go` — и сторож R493 сразу показал, почему так нельзя: он считает
// вхождения вызова (`ожидалось 1`), а в пакете есть соседние файлы
// (`llamacpp_translate_resp.go`), где объявление и ещё один вызов. Склейка всего
// пакета ломает СМЫСЛ счётчиков, а не только их значение.
//
// Цена списка: переименование файла слоя ломает сторож — но громко и с инструкцией
// (см. readLlamaCppSourcesR630), а не тихим «ноль вхождений».
var llamaCppTransportLayerFilesR630 = []string{
	"llamacpp_transport.go",
	"llamacpp_stream_upstream.go",
	"llamacpp_stream_translate.go",
	"llamacpp_stream_finalize.go",
	"llamacpp_upstream_call.go",
}

// llamaCppSourceFilesR630 — файлы слоя, которые реально существуют на диске.
func llamaCppSourceFilesR630(t *testing.T) []string {
	t.Helper()
	var names []string
	for _, n := range llamaCppTransportLayerFilesR630 {
		if _, err := os.Stat(filepath.Join(".", n)); err != nil {
			t.Fatalf("файл слоя %s не найден: если он переименован или разложен дальше — "+
				"обновите llamaCppTransportLayerFilesR630 (это сторож, он обязан знать "+
				"про перенос, иначе краснеет ложной тревогой)", n)
		}
		names = append(names, n)
	}
	sort.Strings(names) // порядок не должен зависеть от файловой системы
	return names
}

// readLlamaCppSourcesR630 — склеенный текст всех боевых `llamacpp_*.go`.
func readLlamaCppSourcesR630(t *testing.T) string {
	t.Helper()
	var sb strings.Builder
	for _, n := range llamaCppSourceFilesR630(t) {
		raw, err := os.ReadFile(filepath.Join(".", n))
		if err != nil {
			t.Fatalf("чтение %s: %v", n, err)
		}
		sb.Write(raw)
		sb.WriteString("\n")
	}
	return sb.String()
}
