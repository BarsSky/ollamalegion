// debug_bundle_r476.go — R476 (2026-11-07): диагностический архив воркера.
//
// ЗАЧЕМ. Разбор дефекта агентского цикла упирался в доступ: у оператора нет
// SSH к стенду, `docker logs` недоступен, а нужные артефакты лежат в разных
// местах (`last-prompt`, `last-stream`, файлы /app/diag, кольцевой буфер логов,
// /api/info). Вопрос «пришлите логи» означал переписку.
//
// ЧТО ЗДЕСЬ. GET /api/v1/cppworker/debug/bundle отдаёт zip одним файлом:
//
//	info.json              — /api/info на момент снятия (сборка, GPU, модели)
//	logs.txt               — кольцевой буфер логов процесса (R476 включил его
//	                         для воркера: CPPWORKER_LOG_BUFFER_ENTRIES, дефолт 2000)
//	last-prompt.json       — метаданные последнего inference-запроса
//	last-stream.json       — последний обрыв стрима (если был)
//	prompt-last.txt        — собранный промпт (если CPPWORKER_DEBUG_PROMPT_FILE задан)
//	request-last.json      — тело последнего запроса (если CPPWORKER_DEBUG_REQUEST_FILE задан)
//	unparsed-tool-calls.jsonl — хвост файла срывов разбора вызовов
//	unparsed-ring.json     — последние срывы из памяти
//	env.json               — только флаги диагностики/логов/окна (без секретов)
//	README.txt             — что это за архив и как его читать
//
// Секреты (токены, ключи) в архив НЕ попадают: env.json собирается по
// белому списку, а /api/info их не содержит.
package main

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/version"
)

// bundleMaxFileBytesR476 — предел на один файл в архиве. Промпт на 100k
// токенов и RAW-вывод бывают большими; архив должен остаться передаваемым
// (его скачивают через WebUI), поэтому лишнее усекается с явной пометкой.
const bundleMaxFileBytesR476 = 4 << 20

// bundleEnvWhitelistR476 — переменные, которые реально влияют на поведение и
// которые нужны для разбора. Белый список, а не «всё окружение»: в env есть
// ключи API, их нельзя отдавать в архив.
var bundleEnvWhitelistR476 = []string{
	"CPPWORKER_LOG_LEVEL",
	"CPPWORKER_LOG_BUFFER_ENTRIES",
	"CPPWORKER_MAX_N_PREDICT",
	"CPPWORKER_DEFAULT_N_PREDICT",
	"CPPWORKER_DEFAULT_N_PREDICT_REASONING",
	"CPPWORKER_TOOLS_THINKING",
	// R485: сторож удержания и аварийный тумблер — без них в архиве не видно,
	// почему ход был не потоковым и какие пределы действовали.
	"CPPWORKER_TOOLS_HOLD_MAX_SEC",
	"CPPWORKER_TOOLS_HOLD_MAX_BYTES",
	"CPPWORKER_TOOLS_HOLD_RELEASE_REASONING",
	"CPPWORKER_TOOLS_STREAM_CONTENT",
	"CPPWORKER_STREAM_TRACE",
	"CPPWORKER_DEBUG_PROMPT_FILE",
	"CPPWORKER_DEBUG_REQUEST_FILE",
	"CPPWORKER_DEBUG_TOOL_ARGS",
	"CPPWORKER_UNPARSED_CAPTURE_FILE",
	"CPPWORKER_LLAMA_BUILD_DATE",
	"CUDA_ARCH",
	"APP_VERSION",
	"GIT_COMMIT",
	"BUILD_DATE",
}

// initLogBufferR476 — включить кольцевой буфер логов процесса.
//
// До R476 у воркера брокера не было вовсе: логи уходили только в stdout, а его
// хвост виден лишь тому, у кого есть docker на стенде. Размер настраивается
// CPPWORKER_LOG_BUFFER_ENTRIES (дефолт 2000, 0 или мусор → дефолт, отрицательное
// значение выключает).
func initLogBufferR476() {
	entries := 2000
	if raw := strings.TrimSpace(os.Getenv("CPPWORKER_LOG_BUFFER_ENTRIES")); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil {
			entries = n
		}
	}
	// R476: псевдоним CPPWORKER_DIAG_RING — только если основная не задана.
	if strings.TrimSpace(os.Getenv("CPPWORKER_LOG_BUFFER_ENTRIES")) == "" {
		if raw := strings.TrimSpace(os.Getenv("CPPWORKER_DIAG_RING")); raw != "" {
			if n, err := strconv.Atoi(raw); err == nil {
				entries = n
			}
		}
	}
	if entries <= 0 {
		logger.Get().Infow("log buffer disabled", "entries", entries)
		return
	}
	if entries > 50000 {
		entries = 50000
	}
	logger.SetBrokerSource("cppworker")
	logger.SetBroker(logger.NewLogBroker(entries))
}

// handleDebugBundle — GET /api/v1/cppworker/debug/bundle.
func handleDebugBundle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed, use GET")
		return
	}

	stamp := time.Now().UTC().Format("20060102-150405")
	host, _ := os.Hostname()
	if strings.TrimSpace(host) == "" {
		host = "worker"
	}
	name := fmt.Sprintf("cppworker-diag-%s-%s.zip", host, stamp)
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)

	// R476: `?since=30m` (или RFC3339) — отдать только записи после этого момента.
	// Нужно, чтобы архив по живому дефекту не тащил сутки логов.
	cutoff := bundleSinceCutoffR476(r.URL.Query().Get("since"))

	zw := zip.NewWriter(w)
	write := func() {
		_ = zw.Close()
	}

	if err := bundleAddJSONR476(zw, "info.json", collectInfoStatus()); err != nil {
		logger.Get().Warnw("debug/bundle: info.json failed", "error", err)
	}
	if err := bundleAddJSONR476(zw, "last-prompt.json", lastPromptSnapshotCopyR476()); err != nil {
		logger.Get().Warnw("debug/bundle: last-prompt.json failed", "error", err)
	}
	if err := bundleAddJSONR476(zw, "last-stream.json", getLastStreamInfo()); err != nil {
		logger.Get().Warnw("debug/bundle: last-stream.json failed", "error", err)
	}
	// R477: сбои воркера (5xx) — первым делом при разборе «ответ не дошёл».
	if err := bundleAddJSONR476(zw, "failures.json", RecentWorkerFailuresR477()); err != nil {
		logger.Get().Warnw("debug/bundle: failures.json failed", "error", err)
	}
	if err := bundleAddJSONR476(zw, "unparsed-ring.json", unparsedRingCopyR476()); err != nil {
		logger.Get().Warnw("debug/bundle: unparsed-ring.json failed", "error", err)
	}
	if err := bundleAddJSONR476(zw, "env.json", bundleEnvSnapshotR476()); err != nil {
		logger.Get().Warnw("debug/bundle: env.json failed", "error", err)
	}
	// R486: запечённая идентичность образа — отдельным файлом. env.json теперь
	// вторичен: он показывает, что ПЕРЕДАЛИ в контейнер, а identity.json — что
	// реально СОБРАНО. Разбирать «какой образ крутится» надо по второму.
	if id := version.BakedIdentityR486(); id != nil {
		if err := bundleAddJSONR476(zw, "identity.json", id); err != nil {
			logger.Get().Warnw("debug/bundle: identity.json failed", "error", err)
		}
	}
	bundleAddTextR476(zw, "logs.txt", bundleLogsTextSinceR476(cutoff))
	bundleAddFileR476(zw, "prompt-last.txt", strings.TrimSpace(os.Getenv("CPPWORKER_DEBUG_PROMPT_FILE")))
	bundleAddFileR476(zw, "request-last.json", strings.TrimSpace(os.Getenv("CPPWORKER_DEBUG_REQUEST_FILE")))
	bundleAddFileR476(zw, "unparsed-tool-calls.jsonl", unparsedCapturePath())
	bundleAddTextR476(zw, "README.txt", bundleReadmeR476)

	write()
	logger.Get().Infow("debug/bundle: архив отдан",
		"request_id", getRequestID(r), "file", name, "remote", r.RemoteAddr)
}

// lastPromptSnapshotCopyR476 — копия снимка последнего промпта под мьютексом.
func lastPromptSnapshotCopyR476() *LastPromptInfo {
	lastPromptMu.RLock()
	defer lastPromptMu.RUnlock()
	if lastPromptSnapshot == nil {
		return nil
	}
	cp := *lastPromptSnapshot
	return &cp
}

// unparsedRingCopyR476 — копия кольца срывов разбора.
func unparsedRingCopyR476() []unparsedCapture {
	unparsedMu.Lock()
	defer unparsedMu.Unlock()
	if len(unparsedRing) == 0 {
		return nil
	}
	cp := make([]unparsedCapture, len(unparsedRing))
	copy(cp, unparsedRing)
	return cp
}

// bundleEnvSnapshotR476 — значения флагов по белому списку (пустые не пишем).
func bundleEnvSnapshotR476() map[string]string {
	out := make(map[string]string, len(bundleEnvWhitelistR476))
	for _, key := range bundleEnvWhitelistR476 {
		if v := strings.TrimSpace(os.Getenv(key)); v != "" {
			out[key] = v
		}
	}
	return out
}

// bundleLogsTextR476 — снимок кольцевого буфера логов процесса.
func bundleLogsTextR476() string {
	return bundleLogsTextSinceR476(time.Time{})
}

// bundleLogsTextSinceR476 — то же, но только записи позже cutoff (нулевое время
// = без фильтра).
func bundleLogsTextSinceR476(cutoff time.Time) string {
	broker := logger.GetBroker()
	if broker == nil {
		return "# кольцевой буфер логов не включён (CPPWORKER_LOG_BUFFER_ENTRIES=0)\n"
	}
	entries := broker.Snapshot()
	var b strings.Builder
	for _, e := range entries {
		if !cutoff.IsZero() && e.Time.Before(cutoff) {
			continue
		}
		src := e.Source
		if src == "" {
			src = "worker"
		}
		b.WriteString(fmt.Sprintf("%s %-5s %-9s %s\n",
			e.Time.Format("2006-01-02T15:04:05.000Z07:00"),
			strings.ToUpper(e.Level), src, e.Message))
	}
	if b.Len() == 0 {
		return "# буфер пуст: с момента старта не было записей уровня логирования\n"
	}
	return b.String()
}

// bundleAddJSONR476 — сериализованная запись архива.
func bundleAddJSONR476(zw *zip.Writer, name string, value interface{}) error {
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return bundleAddTextErrR476(zw, name, string(raw)+"\n")
}

// bundleAddTextR476 — текстовая запись архива (ошибку только логируем).
func bundleAddTextR476(zw *zip.Writer, name, text string) {
	_ = bundleAddTextErrR476(zw, name, text)
}

func bundleAddTextErrR476(zw *zip.Writer, name, text string) error {
	f, err := zw.Create(name)
	if err != nil {
		return err
	}
	body := text
	if len(body) > bundleMaxFileBytesR476 {
		body = body[:bundleMaxFileBytesR476] +
			"\n\n# ОБРЕЗАНО: файл больше " + fmt.Sprint(bundleMaxFileBytesR476) +
			" байт, показано начало\n"
	}
	_, err = f.Write([]byte(body))
	return err
}

// bundleAddFileR476 — файл с диска, если он есть; отсутствие не ошибка.
func bundleAddFileR476(zw *zip.Writer, name, path string) {
	if strings.TrimSpace(path) == "" {
		return
	}
	data, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		if !os.IsNotExist(err) {
			logger.Get().Debugw("debug/bundle: файл недоступен", "file", path, "error", err)
		}
		bundleAddTextR476(zw, name+".missing.txt",
			"# файла нет: "+path+"\n# включите соответствующий CPPWORKER_DEBUG_* флаг\n")
		return
	}
	body := string(data)
	if len(body) > bundleMaxFileBytesR476 {
		body = body[:bundleMaxFileBytesR476] +
			"\n\n# ОБРЕЗАНО: файл больше " + fmt.Sprint(bundleMaxFileBytesR476) + " байт, показано начало\n"
	}
	bundleAddTextR476(zw, name, body)
}

// bundleSinceCutoffR476 — разобрать `since`: длительность ("30m", "2h", "90s")
// или абсолютное время RFC3339. Пустое/мусор = без фильтра (нулевое время),
// потому что «не понял параметр» не должно молча отдавать пустой архив.
func bundleSinceCutoffR476(raw string) time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}
	}
	if d, err := time.ParseDuration(raw); err == nil && d > 0 {
		return time.Now().Add(-d)
	}
	if ts, err := time.Parse(time.RFC3339, raw); err == nil {
		return ts
	}
	logger.Get().Warnw("debug/bundle: since не разобран, фильтр не применён", "since", raw)
	return time.Time{}
}

// bundleReadmeR476 — краткая инструкция к архиву.
const bundleReadmeR476 = `Диагностический архив cppworker (R476).

Что внутри:
  info.json       — сборка (build/commit/llama_build), GPU, загруженные модели
  logs.txt        — кольцевой буфер логов процесса (CPPWORKER_LOG_BUFFER_ENTRIES)
  last-prompt.json — метаданные последнего inference-запроса
  last-stream.json — последний обрыв стрима (если был)
  prompt-last.txt  — собранный промпт (нужен CPPWORKER_DEBUG_PROMPT_FILE)
  request-last.json — тело последнего запроса (CPPWORKER_DEBUG_REQUEST_FILE)
  unparsed-tool-calls.jsonl — срывы разбора вызовов инструментов
  unparsed-ring.json — те же срывы из памяти (до 5 последних)
  env.json        — только флаги диагностики/логов/окна (белый список)

Чего НЕТ: токенов и ключей (env.json собирается по белому списку).

Как читать сводку хода: строка "/api/chat stream response summary" в logs.txt.
В ней request_id (тот же, что в логе балансера в строке "request trace"),
streamed_content_bytes/emitted_len/final_content_len (что реально отдано),
tool_calls, done_reason, generated_tokens, budget_exhausted,
dumped_args_instead_of_call.
`
