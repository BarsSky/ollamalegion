// load_error_tail.go — R697 (2026-11-24): ошибка ЗАГРУЗКИ обязана нести вердикт llama.cpp.
//
// ЖИВОЙ ФАКТ (стенд, файл Inkling-2-Mini-UD-Q4_K_M.gguf). Заголовок GGUF читается,
// general.architecture="inkling2", а POST /api/models/load заканчивается за десятки
// миллисекунд ответом:
//
//	load failed: load model failed: failed to load model from /app/models/Inkling-2-Mini-UD-Q4_K_M.gguf
//
// — без единого слова о причине, хотя llama.cpp в этот момент печатает:
//
//	llama_model_load: error loading model: unknown model architecture: 'inkling2'
//	llama_model_load_from_file_impl: failed to load model
//
// Мост эти строки ПЕРЕХВАТЫВАЕТ (R521: c/bridge/bridge.go → recordLogLine), но R521
// приклеивал хвост только к ошибкам ДЕКОДА (isDecodeFailureMessage), поэтому для
// загрузки он терялся: причиной в ответе стоял сам путь к файлу.
//
// Здесь то же правило распространено на загрузку: если в хвосте лога есть ВЕРДИКТ
// движка (неизвестная архитектура, сбой метаданных или словаря), он приклеивается к
// ошибке. Классификация причины (cmd/cppworker/load_failure_reason.go) и текст для
// оператора строятся уже по факту, а не по догадке.
package cppbackend

import (
	"fmt"
	"strings"

	"ollama-loadbalancer/c/bridge"
)

// loadVerdictMarkers — подстроки, по которым строка лога считается вердиктом
// загрузки. Список намеренно узкий: в хвост попадает причина, а не весь лог.
var loadVerdictMarkers = []string{
	"unknown model architecture",
	"unsupported model architecture",
	"error loading model", // llama_model_load: error loading model: <причина>
	"failed to load model",
}

// loadVerdictTailLines — сколько строк вердикта прикладывать к ошибке. Две-три
// строки (причина + «failed to load model») читаются, двадцать — уже простыня.
const loadVerdictTailLines = 4

// loadEngineVerdictLines — выбрать из хвоста лога строки вердикта (чистая функция:
// подмена входа не требует живого моста).
//
// Порядок строк сохраняется как в логе: причина всегда идёт ПЕРЕД итоговой строкой
// «failed to load model», и переставлять их значило бы менять смысл.
func loadEngineVerdictLines(lines []string) []string {
	out := make([]string, 0, loadVerdictTailLines)
	seen := make(map[string]bool, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || seen[line] {
			continue
		}
		low := strings.ToLower(line)
		for _, marker := range loadVerdictMarkers {
			if strings.Contains(low, marker) {
				seen[line] = true
				out = append(out, line)
				break
			}
		}
		if len(out) >= loadVerdictTailLines {
			break
		}
	}
	return out
}

// loadErrorWithTailText — приклеить вердикт к тексту ошибки (чистая функция).
//
// Префикс "[llama.cpp]" намеренно тот же, что у хвоста ошибок декода (R521): по нему
// оператор уже отличает слова движка от наших собственных проверок.
func loadErrorWithTailText(msg string, lines []string) string {
	verdict := loadEngineVerdictLines(lines)
	if len(verdict) == 0 {
		return msg
	}
	return msg + "\n[llama.cpp] " + strings.Join(verdict, "\n[llama.cpp] ")
}

// LoadErrorWithTail — ошибка загрузки с приклеенным вердиктом llama.cpp.
//
// Оборачивает через %w, чтобы errors.Is(err, bridge.ErrAborted) и типизированные
// ошибки выше по стеку продолжали работать: проверка «отменено пользователем» не
// должна ломаться из-за добавленного текста.
func LoadErrorWithTail(err error) error {
	if err == nil {
		return nil
	}
	tail := loadErrorWithTailText(err.Error(), bridge.RecentLogLines(decodeErrorTailLines))
	if tail == err.Error() {
		return err
	}
	return fmt.Errorf("%w\n%s", err, strings.TrimPrefix(tail, err.Error()))
}
