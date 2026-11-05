// model_known_r193.go — R193 (2026-10-20): НЕ ОТВЕЧАТЬ ЧУЖОЙ МОДЕЛЬЮ НА
// НЕИЗВЕСТНОЕ ИМЯ.
//
// ЖИВОЙ ДЕФЕКТ (errors-matrix-r180). Запрос с несуществующей моделью:
//
//	напрямую в воркер  → HTTP 404 {"code":"model_file_not_found", ...}
//	через балансер      → HTTP 200 и ОТВЕТ ОТ ЗАГРУЖЕННОЙ МОДЕЛИ,
//	                       в теле при этом возвращалось запрошенное имя.
//
// То есть клиент (Cline) получал ответ другой модели и не узнавал об этом —
// тихая ошибка, которая подрывает доверие ко всем ответам агента.
//
// ЧТО ДЕЛАЕМ. Перед проксированием спрашиваем у бэкенда список моделей
// (/api/models) и сверяем имя — точно, по нормализованной форме и по basename
// файла (алиасы воркера). Если имя не найдено — отдаём ТЕРМИНАЛЬНЫЙ ответ в
// форме поверхности (писец R119 уже умеет и OpenAI-конверт, и нативную форму).
//
// БЕЗОПАСНОСТЬ: если каталог не опрошен или ответ не разобран — НЕ блокируем
// (probed=false). Предпроверка не должна становиться новым источником отказов.
package balancer

import (
	"encoding/json"
	"io"
	"net/http"
	"path"
	"strings"
	"time"
)

// modelCatalogProbeTimeout — предпроверка, а не полезная работа.
const modelCatalogProbeTimeout = 2500 * time.Millisecond

// backendModelCatalog — имена моделей, которые бэкенд реально знает.
type backendModelCatalog struct {
	Models []struct {
		Name    string   `json:"name"`
		Path    string   `json:"path"`
		Aliases []string `json:"aliases"`
	} `json:"models"`
	Available []string `json:"available_models"`
}

// modelKnownOnBackend — знает ли бэкенд такое имя модели.
//
// Возвращает (known, available, probed). probed=false означает «спросить не
// удалось» — тогда решение принимает прежний код (никаких отказов от нас).
func (p *Proxy) modelKnownOnBackend(host string, port int, token, model string) (bool, []string, bool) {
	if model == "" || host == "" || port <= 0 {
		return false, nil, false
	}
	url := "http://" + host + ":" + itoaPort(port) + "/api/models"
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return false, nil, false
	}
	if token != "" {
		req.Header.Set("X-API-Token", token)
	}
	resp, err := (&http.Client{Timeout: modelCatalogProbeTimeout}).Do(req)
	if err != nil {
		return false, nil, false
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return false, nil, false
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return false, nil, false
	}
	var cat backendModelCatalog
	if err := json.Unmarshal(raw, &cat); err != nil {
		return false, nil, false
	}
	if len(cat.Models) == 0 && len(cat.Available) == 0 {
		// Пустой каталог — не доказательство отсутствия (модель могла грузиться).
		return false, nil, false
	}

	want := normalizeModelName(model)
	wantBase := strings.TrimSuffix(path.Base(strings.ReplaceAll(model, "\\", "/")), ".gguf")

	available := make([]string, 0, len(cat.Models))
	known := false
	for _, m := range cat.Models {
		if m.Name != "" {
			available = append(available, m.Name)
		}
		if sameModelName(m.Name, want, wantBase) || sameModelName(m.Path, want, wantBase) {
			known = true
		}
		for _, a := range m.Aliases {
			if sameModelName(a, want, wantBase) {
				known = true
			}
		}
	}
	for _, name := range cat.Available {
		available = append(available, name)
		if sameModelName(name, want, wantBase) {
			known = true
		}
	}
	if len(available) == 0 {
		return false, nil, false
	}
	return known, available, true
}

// sameModelName — совпадает ли имя из каталога с запрошенным.
func sameModelName(candidate, wantNormalized, wantBase string) bool {
	c := strings.TrimSpace(candidate)
	if c == "" {
		return false
	}
	if normalizeModelName(c) == wantNormalized {
		return true
	}
	candidateBase := strings.TrimSuffix(path.Base(strings.ReplaceAll(c, "\\", "/")), ".gguf")
	return candidateBase != "" && candidateBase == wantBase
}

// itoaPort — маленький помощник, чтобы не тянуть strconv в этот файл дважды.
func itoaPort(port int) string {
	if port <= 0 {
		return "0"
	}
	var digits [8]byte
	i := len(digits)
	for port > 0 {
		i--
		digits[i] = byte('0' + port%10)
		port /= 10
	}
	return string(digits[i:])
}

// modelAnswerable — можно ли обслужить модель этим бэкендом.
//
// Возвращает (ok, available). ok=false только когда ОПРОШЕНЫ ОБА источника и
// модели нет ни в одном: ни среди загруженных (/api/models), ни среди файлов на
// диске (/api/models/files, кэш каталога балансера).
//
// R449 (2026-11-05): ЖИВОЙ ДЕФЕКТ, поймавший эту функцию. Стенд, порт 18079,
// модель Qwen3-4B (файл на диске, 2.3 ГБ). В этот момент нагрузку держала другая
// модель — Qwen3.8-9B (шла загрузка 207 с). Ответ клиенту:
//
//	HTTP 404 {"code":"model_not_found","decision":"model_missing",
//	 "message":"model not found on this backend: модели нет в каталоге бэкенда
//	 (available: Qwen3.8-9B-...)"}
//
// То есть модель, которая ЛЕЖИТ НА ДИСКЕ, объявлялась отсутствующей — только
// потому что сейчас загружена другая. Для агента это терминальная ошибка «нет
// модели»; повторить он не может, а воркер эту модель загрузил бы по первому
// запросу. Причина: предпроверка R193 спрашивала /api/models, который отдаёт
// ЗАГРУЖЕННЫЕ модели, и считала «не среди загруженных» = «нет модели».
//
// Правило: «нет ни в загруженных, ни на диске» — отказ; «на диске есть, сейчас
// не загружена» — не отказ (дальше preflight её загрузит).
func (p *Proxy) modelAnswerable(backendID, model string) (bool, []string) {
	if p == nil || backendID == "" || model == "" {
		return true, nil
	}
	b := p.GetBackend(backendID)
	if b == nil {
		return true, nil
	}
	port := b.CppWorkerPort
	if port <= 0 {
		return true, nil
	}
	known, available, probed := p.modelKnownOnBackend(b.Host, port, b.CppWorkerApiToken, model)
	if !probed {
		return true, nil
	}
	if known {
		return true, available
	}

	// R449: в загруженных нет — смотрим файлы на диске (кэш каталога).
	onDisk, diskKnown := p.BackendModelOnDisk(backendID, model)
	if diskKnown {
		// Список в отказе должен быть про ДИСК, а не про «что сейчас загружено»:
		// иначе оператор читает «доступна одна модель», хотя файлов три.
		if files := p.catalogFilesForBackend(backendID); len(files) > 0 {
			available = files
		}
	}
	if onDisk {
		return true, available
	}
	if !diskKnown {
		// Каталог файлов ещё не опрошен — не превращаем это в отказ (правило R193:
		// предпроверка не должна становиться источником новых отказов).
		return true, available
	}
	return false, available
}

// catalogFilesForBackend — имена файлов модели на диске одного бэкенда
// (пустой список, если каталог этого бэкенда не опрошен).
func (p *Proxy) catalogFilesForBackend(backendID string) []string {
	if p == nil || backendID == "" {
		return nil
	}
	snap := p.ModelCatalogSnapshot()
	entry, ok := snap[backendID]
	if !ok {
		return nil
	}
	out := make([]string, 0, len(entry.Files))
	for _, name := range entry.Files {
		if strings.TrimSpace(name) != "" {
			out = append(out, name)
		}
	}
	return out
}
