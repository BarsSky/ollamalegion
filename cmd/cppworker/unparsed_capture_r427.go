package main

// unparsed_capture_r427.go — R427 (2026-11-04): ДИАГНОСТИКА нераспознанных
// вызовов инструментов.
//
// ЗАЧЕМ. Живая сессия Cline упёрлась в ход, который cppworker закрыл ошибкой
// `model returned a tool call that could not be parsed into tool_calls`. Ответ
// модели в этот момент ВЫБРАСЫВАЛСЯ, и понять, что именно она выдала, по логам
// уже нельзя: предупреждение с preview писалось, но вытеснялось из docker logs
// (там хранится только хвост, а in-memory буфер балансера — ~10 минут).
//
// Что делает этот файл: на путях «разметка вызова есть, а вызова нет» сохраняет
// СЫРОЙ вывод модели — первые и последние символы в памяти и полный текст в
// файл, плюс метаданные (модель, длина, число инструментов, бюджет). Данные
// доступны двумя способами:
//
//	GET /api/v1/cppworker/debug/unparsed               — последние N срывов (JSON)
//	GET /api/v1/cppworker/debug/unparsed?clear=1       — очистить и вернуть что было
//
// ПОВЕДЕНИЕ КЛИЕНТОВ НЕ МЕНЯЕТСЯ: это только запись диагностики, ответ модели
// клиенту по-прежнему формируется прежними ветками.
//
// Файл: env CPPWORKER_UNPARSED_CAPTURE_FILE (по умолчанию
// /app/diag/unparsed-tool-calls.jsonl + соседние RAW-*.txt с полным выводом).
// При отсутствии прав на запись функция молча живёт только в памяти — терять
// диагностику из-за прав не нужно, но и падать нельзя.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"ollama-loadbalancer/pkg/logger"
)

// unparsedHeadChars / unparsedTailChars — сколько символов сырого вывода
// держать в памяти. Полный текст уходит в отдельный файл RAW-*.txt.
const (
	unparsedHeadChars = 4000
	unparsedTailChars = 2000
	// unparsedRingMax — сколько последних срывов доступно через endpoint.
	unparsedRingMax = 5
	// unparsedRawMaxBytes — предел на файл с полным сырым выводом.
	unparsedRawMaxBytes = 512 * 1024
	// unparsedJSONLMaxBytes — при превышении jsonl уезжает в .1 (перезапись).
	unparsedJSONLMaxBytes = 8 * 1024 * 1024
)

// unparsedCapture — один зафиксированный срыв.
//
// R429 (2026-11-04): поля с указателями (строки, срез) идут ПЕРВЫМИ, числа —
// после них: так GC сканирует непрерывный префикс структуры, меньше область
// сканирования (govet fieldalignment). Порядок JSON-полей от этого не значит
// ничего: объект читается по именам.
type unparsedCapture struct {
	Timestamp string   `json:"ts"`
	Model     string   `json:"model"`
	Endpoint  string   `json:"endpoint"`
	Reason    string   `json:"reason"` // markup_unparsed | empty_after_strip | markup_stripped
	RawHead   string   `json:"raw_head"`
	RawTail   string   `json:"raw_tail"`
	RawFile   string   `json:"raw_file,omitempty"`
	ToolNames []string `json:"tool_names,omitempty"`
	// R468 (2026-11-06): имена вызовов, которые РАСПОЗНАНЫ, но отклонены как
	// необъявленные (R428). Пусто — прежнее поведение.
	DroppedToolNames []string `json:"dropped_tool_names,omitempty"`
	RawLen           int      `json:"raw_len"`
	ParsedLen        int      `json:"parsed_len"`
	EmittedLen       int      `json:"emitted_len"`
	GeneratedTokens  int      `json:"generated_tokens,omitempty"`
	NPredict         int      `json:"n_predict,omitempty"`
	ToolsCount       int      `json:"tools_count"`
}

var (
	unparsedMu   sync.Mutex
	unparsedRing []unparsedCapture
)

// unparsedCapturePath — куда писать диагностику.
func unparsedCapturePath() string {
	if v := strings.TrimSpace(os.Getenv("CPPWORKER_UNPARSED_CAPTURE_FILE")); v != "" {
		return v
	}
	return filepath.Join("/app", "diag", "unparsed-tool-calls.jsonl")
}

// unparsedToolNames — имена объявленных инструментов (для отчёта).
func unparsedToolNames(tools []openAITool) []string {
	if len(tools) == 0 {
		return nil
	}
	out := make([]string, 0, len(tools))
	for _, t := range tools {
		if t.Function.Name != "" {
			out = append(out, t.Function.Name)
		}
	}
	return out
}

// recordUnparsedCapture — зафиксировать срыв: память + файлы.
//
// raw — ПОЛНЫЙ сырой вывод модели, parsed — то, что парсилось (без размышлений),
// emittedLen — сколько из сырого вывода уже ушло клиенту дельтами.
// recordUnparsedCapture — запись без сведений об отброшенных вызовах
// (поведение до R468).
func recordUnparsedCapture(model, endpoint, reason string, raw, parsed string,
	emittedLen, generatedTokens, nPredict int, tools []openAITool) {
	recordUnparsedCaptureFieldsR468(model, endpoint, reason, raw, parsed,
		emittedLen, generatedTokens, nPredict, tools, nil)
}

// recordUnparsedCaptureFieldsR468 — то же плюс имена вызовов, отклонённых как
// необъявленные. reason `call_undeclared` + этот список отвечают на вопрос «а
// инструмент вообще объявляли?» по артефакту, без чтения исходников.
func recordUnparsedCaptureFieldsR468(model, endpoint, reason string, raw, parsed string,
	emittedLen, generatedTokens, nPredict int, tools []openAITool, dropped []string) {

	head, tail := raw, raw
	if len(head) > unparsedHeadChars {
		head = head[:unparsedHeadChars]
	}
	if len(tail) > unparsedTailChars {
		tail = tail[len(tail)-unparsedTailChars:]
	}
	cap := unparsedCapture{
		Timestamp:        time.Now().UTC().Format(time.RFC3339Nano),
		Model:            model,
		Endpoint:         endpoint,
		Reason:           reason,
		RawLen:           len(raw),
		ParsedLen:        len(parsed),
		EmittedLen:       emittedLen,
		GeneratedTokens:  generatedTokens,
		NPredict:         nPredict,
		ToolsCount:       len(tools),
		ToolNames:        unparsedToolNames(tools),
		DroppedToolNames: dropped,
		RawHead:          head,
		RawTail:          tail,
	}

	// Полный сырой вывод — в отдельный файл, чтобы не обрезать самое интересное
	// (обычно срыв в СЕРЕДИНЕ огромного JSON-вызова).
	dir := filepath.Dir(unparsedCapturePath())
	if err := os.MkdirAll(dir, 0o755); err == nil {
		name := fmt.Sprintf("RAW-%s-%s.txt", time.Now().UTC().Format("20060102-150405.000"), sanitizeFilePart(reason))
		full := filepath.Join(dir, name)
		body := "=== MODEL ===\n" + model + "\n=== REASON ===\n" + reason +
			fmt.Sprintf("\n=== LEN raw=%d parsed=%d emitted=%d gen=%d n_predict=%d tools=%d ===\n",
				len(raw), len(parsed), emittedLen, generatedTokens, nPredict, len(tools)) +
			"\n=== DECLARED TOOLS ===\n" + strings.Join(unparsedToolNames(tools), ",") +
			"\n=== DROPPED (не объявлены) ===\n" + strings.Join(dropped, ",") +
			"\n=== FULL OUTPUT ===\n" + raw +
			"\n\n=== PARSE INPUT (без размышлений) ===\n" + parsed + "\n"
		if len(body) > unparsedRawMaxBytes {
			body = body[:unparsedRawMaxBytes] + "\n[truncated]\n"
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err == nil {
			cap.RawFile = full
		}
	}

	// JSONL-строка (то же, что в памяти, без полного текста).
	if data, err := json.Marshal(cap); err == nil {
		path := unparsedCapturePath()
		if fi, err := os.Stat(path); err == nil && fi.Size() > unparsedJSONLMaxBytes {
			_ = os.Rename(path, path+".1")
		}
		if f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
			_, _ = f.Write(append(data, '\n'))
			_ = f.Close()
		}
	}

	unparsedMu.Lock()
	unparsedRing = append(unparsedRing, cap)
	if len(unparsedRing) > unparsedRingMax {
		unparsedRing = unparsedRing[len(unparsedRing)-unparsedRingMax:]
	}
	unparsedMu.Unlock()

	if l := logger.Get(); l != nil {
		l.Warnw("unparsed tool-call capture saved (R427)",
			"model", model, "reason", reason, "raw_len", len(raw),
			"emitted_len", emittedLen, "tools", len(tools), "file", cap.RawFile)
	}
}

// sanitizeFilePart — имя файла без спецсимволов.
func sanitizeFilePart(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			out = append(out, r)
		default:
			out = append(out, '_')
		}
	}
	if len(out) == 0 {
		return "capture"
	}
	return string(out)
}

// handleDebugUnparsed — GET /api/v1/cppworker/debug/unparsed
//
// Отдаёт последние срывы (по убыванию свежести) и путь к файлу диагностики.
// ?clear=1 — вернуть и очистить кольцо (файлы остаются).
func handleDebugUnparsed(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "use GET")
		return
	}
	unparsedMu.Lock()
	items := make([]unparsedCapture, len(unparsedRing))
	copy(items, unparsedRing)
	if r.URL.Query().Get("clear") == "1" {
		unparsedRing = nil
	}
	unparsedMu.Unlock()

	// Свежие — первыми (в кольце храним в порядке записи).
	for i, j := 0, len(items)-1; i < j; i, j = i+1, j-1 {
		items[i], items[j] = items[j], items[i]
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"count": len(items),
		"file":  unparsedCapturePath(),
		"items": items,
		"hint":  "полный сырой вывод — в raw_file (RAW-*.txt рядом с jsonl)",
		"note":  "R427: только диагностика, поведение ответов не изменяется",
	})
}
