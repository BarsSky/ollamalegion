// diag_bundle.go — R476 (2026-11-07): один архив со стенда для разбора.
//
// ЗАЧЕМ. «Пришлите логи» на практике означало несколько сообщений: логи
// балансера, сводку хода с воркера, промпт, тело запроса, конфигурацию окна.
// Часть этого лежала в docker, часть — в памяти процесса, часть — в файлах
// /app/diag. Здесь всё собирается одним запросом и отдаётся zip-файлом,
// который скачивается кнопкой из WebUI.
//
// СОСТАВ АРХИВА:
//
//	summary.json          — то же, что /api/v1/diag/summary (версии сервисов,
//	                        флаги, окно, профили, снимок кластера)
//	balancer-logs.txt     — кольцевой буфер логов балансера (LB_LOG_BUFFER_ENTRIES)
//	balancer-logs.json    — тот же буфер массивом записей (R715: со счётчиком
//	                        повторов repeated / first_time / last_time)
//	logs-meta.json        — R715: метаданные окна логов (сколько строк пришло,
//	                        сколько схлопнуто, какое время покрывает буфер)
//	failures.json         — последние сбои по стадиям (R477)
//	identity.json         — запечённая идентичность образа балансера (R486)
//	flags.txt             — флаги по белому списку: заданные и незаданные
//	backends/<id>.zip     — диагностический архив воркера (R476; R715 — и для
//	                        image_cpp, через /api/v1/sdworker/debug/bundle)
//	backends/<id>.info.json — /api/info воркера (сборка, GPU, модели)
//	backends/<id>.error.txt — если воркер не ответил: код/таймаут и подсказка
//	backends/<id>.note.txt — только для типов, у которых архива нет по контракту
//	README.txt            — что это и в каком порядке читать
//
// СЕКРЕТЫ: в summary.txt-only флаги (белый список), в логи секретов не пишем;
// токены воркеров используются только как заголовок запроса и в архив не
// попадают.
package api

import (
	"archive/zip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"ollama-loadbalancer/internal/balancer"
	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
	"ollama-loadbalancer/pkg/version"
)

const (
	// diagBundlePerBackendTimeoutR476 — сколько ждать один воркер. Загрузка
	// модели не мешает: эндпоинты диагностики не ждут её.
	diagBundlePerBackendTimeoutR476 = 20 * time.Second
	// diagBundleEntryMaxR476 — предел на вложенный архив/файл.
	diagBundleEntryMaxR476 = 16 << 20
	// diagBundleWorkerTimeoutR715 — сколько ждать диагностический АРХИВ воркера.
	//
	// R715 (D3): сборка архива у воркера — это чтение буфера и сериализация
	// десятков килобайт, а не работа модели, поэтому 5 секунд более чем
	// достаточно. Отдельный (маленький) таймаут нужен, чтобы один зависший
	// воркер не растягивал сборку всего архива: причина «timeout 5s» в
	// backends/<id>.error.txt полезнее, чем архив через три минуты.
	diagBundleWorkerTimeoutR715 = 5 * time.Second
	// diagBundleLogsMaxEntriesR715 — сколько записей буфера кладём в архив.
	// R715: при дефолте 5000 это весь типовой буфер, но при
	// LB_LOG_BUFFER_ENTRIES=20000 архив не должен расти вместе с настройкой.
	diagBundleLogsMaxEntriesR715 = 5000
	// diagBundleLogsMaxBytesR715 — предел текста/JSON логов в архиве (2 МБ на
	// балансерные логи). Иначе одна «болтливая» минута раздувает скачивание.
	diagBundleLogsMaxBytesR715 = 2 << 20
)

// handleDiagBundle — GET /api/v1/diag/bundle.
func (s *Server) handleDiagBundle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "GET only"})
		return
	}

	host, _ := os.Hostname()
	if strings.TrimSpace(host) == "" {
		host = "balancer"
	}
	stamp := time.Now().UTC().Format("20060102-150405")
	name := fmt.Sprintf("ollamalegion-diag-%s-%s.zip", host, stamp)

	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)

	// R476: `?since=30m` | RFC3339 — только записи после этого момента (свои логи
	// фильтруем здесь, параметр передаём воркеру). Нужно, чтобы архив по живому
	// дефекту не тащил сутки логов.
	sinceRaw := strings.TrimSpace(r.URL.Query().Get("since"))
	cutoff := diagBundleSinceCutoffR476(sinceRaw)

	zw := zip.NewWriter(w)
	defer func() { _ = zw.Close() }()

	// 1) Сводка: тот же JSON, что отдаёт /api/v1/diag/summary.
	s.diagBundleAddJSONR476(zw, "summary.json", s.diagSummaryPayloadR476())

	// R477: сбои по стадиям отдельным файлом — их читают первыми при разборе.
	s.diagBundleAddJSONR476(zw, "failures.json", balancer.RecentFailuresR477())

	// 2) Флаги отдельным текстовым файлом: удобно приложить к переписке.
	// R486: запечённая идентичность образа балансера отдельным файлом. В
	// summary.json она тоже есть, но отдельный файл читается человеком и
	// сравнивается с identity.json воркеров один-в-один.
	if id := version.BakedIdentityR486(); id != nil {
		s.diagBundleAddJSONR476(zw, "identity.json", id)
	}
	s.diagBundleAddTextR476(zw, "flags.txt", diagBundleFlagsTextR476())

	// 3) Логи балансера из кольцевого буфера.
	//
	// R715 (D3): текст + тот же буфер массивом (JSON) + метаданные окна.
	// Зачем JSON отдельным файлом: по тексту нельзя посчитать, сколько раз
	// повторилось событие и какое время буфер реально покрывает, — именно этого
	// не хватало при разборе «13 минут логов». Формат записи — расширение
	// прежнего (time/level/message/source/fields) полями repeated/first_time/
	// last_time; старые поля не переименованы.
	logsSnap := diagBundleBalancerLogsSnapshotR715(cutoff)
	s.diagBundleAddTextR476(zw, "balancer-logs.txt", logsSnap.Text)
	s.diagBundleAddTextR476(zw, "balancer-logs.json", logsSnap.JSONText())
	s.diagBundleAddJSONR476(zw, "logs-meta.json", logsSnap.Meta)

	// 4) По каждому бэкенду — диагностика воркера, если это cppworker.
	backends := s.diagBundleBackendsR476()
	for _, b := range backends {
		s.diagBundleAddBackendR476(zw, b, sinceRaw)
	}

	s.diagBundleAddTextR476(zw, "README.txt", diagBundleReadmeR476)
	logger.Get().Infow("diag/bundle: архив отдан",
		"file", name, "backends", len(backends), "remote", r.RemoteAddr)
}

// diagBundleBackendsR476 — список бэкендов для обхода (по id).
func (s *Server) diagBundleBackendsR476() []types.Backend {
	if s.proxy == nil {
		return nil
	}
	all := s.proxy.GetAllBackends()
	sort.Slice(all, func(i, j int) bool { return all[i].ID < all[j].ID })
	return all
}

// diagBundleAddBackendR476 — вложенный архив и /api/info одного бэкенда.
//
// R715 (D3): архив воркера забирается не только у llama_cpp. Живой дефект:
// для image_cpp в архив попадал ТОЛЬКО note.txt «бэкенд не cppworker», поэтому
// вопрос «почему не грузится image-модель» по архиву не разбирался вообще —
// ни логов воркера, ни списка моделей, ни версии. Теперь у image-воркера свой
// эндпоинт (cmd/sdworker/debug_bundle.go), а при недоступности пишется ПРИЧИНА
// (код HTTP/таймаут/сеть) вместо общего «недоступен».
func (s *Server) diagBundleAddBackendR476(zw *zip.Writer, b types.Backend, sinceRaw string) {
	base := ""
	if s.proxy != nil {
		base = s.proxy.BackendHTTPBaseURLR476(b)
	}
	if base == "" {
		base = fmt.Sprintf("http://%s:%d", b.Host, b.OllamaPort)
	}

	// /api/info есть у cppworker и у ollama, у image-воркера он называется
	// /api/image/info (см. cmd/sdworker/router.go) — путь выбирается по типу,
	// а прежний адрес остаётся запасным. Ошибку пишем файлом: «не ответил» —
	// это тоже факт для разбора.
	infoBody, infoErr := diagBundleFetchInfoR715(base, b, func(url string) ([]byte, error) {
		return diagBundleHTTPGetR476(url, b.CppWorkerApiToken)
	})
	if infoErr == nil {
		s.diagBundleAddTextR476(zw, fmt.Sprintf("backends/%s.info.json", b.ID), string(infoBody))
	} else {
		s.diagBundleAddTextR476(zw, fmt.Sprintf("backends/%s.info.error.txt", b.ID), infoErr.Error()+"\n")
	}

	workerPath, contract := diagBundleWorkerPathR715(b.Type)
	if workerPath == "" {
		// Архива нет по контракту типа (например ollama): это не ошибка сбора,
		// поэтому note.txt остаётся — но с названной причиной и подсказкой, что
		// именно можно забрать вместо него.
		s.diagBundleAddTextR476(zw, fmt.Sprintf("backends/%s.note.txt", b.ID),
			"диагностического архива нет по контракту типа "+string(b.Type)+"\n"+
				"contract="+contract+"\n"+
				"note=/api/info рядом: backends/"+b.ID+".info.json\n")
		return
	}

	bundleURL := base + workerPath
	if strings.TrimSpace(sinceRaw) != "" {
		bundleURL += "?since=" + url.QueryEscape(sinceRaw)
	}
	body, err := diagBundleHTTPGetTimeoutR715(bundleURL, b.CppWorkerApiToken, diagBundleWorkerTimeoutR715)
	if err != nil {
		// R715: файл ошибки начинается с прежнего текста ошибки (совместимость),
		// затем — разобранная причина и подсказка. «Бандл недоступен» без причины
		// и есть тот дефект, ради которого сделан шаг.
		s.diagBundleAddTextR476(zw, fmt.Sprintf("backends/%s.error.txt", b.ID),
			diagBundleWorkerErrorTextR715(err, bundleURL, string(b.Type), workerPath))
		return
	}
	if len(body) > diagBundleEntryMaxR476 {
		body = body[:diagBundleEntryMaxR476]
	}
	entry, createErr := zw.Create(fmt.Sprintf("backends/%s.bundle.zip", b.ID))
	if createErr != nil {
		logger.Get().Warnw("diag/bundle: вложенный архив не создан", "backend", b.ID, "error", createErr)
		return
	}
	if _, writeErr := entry.Write(body); writeErr != nil {
		logger.Get().Warnw("diag/bundle: вложенный архив не записан", "backend", b.ID, "error", writeErr)
	}
}

// diagBundleFetchInfoR715 — /api/info бэкенда с учётом типа.
//
// R715: у image-воркера (sdworker) нет /api/info — его поверхность это
// /api/image/info (алиас /info). До правки в архив каждого image-бэкенда
// попадал backends/<id>.info.error.txt с HTTP 404, хотя воркер жив: оператор
// читал это как «воркер не отвечает».
func diagBundleFetchInfoR715(base string, b types.Backend, get func(url string) ([]byte, error)) ([]byte, error) {
	paths := []string{"/api/info"}
	if b.Type == types.BackendTypeImage {
		paths = []string{"/api/image/info", "/api/info"}
	}
	var firstErr error
	for _, path := range paths {
		body, err := get(base + path)
		if err == nil {
			return body, nil
		}
		if firstErr == nil {
			firstErr = err
		}
	}
	if firstErr == nil {
		firstErr = fmt.Errorf("backend %s: /api/info недоступен", base)
	}
	return nil, firstErr
}

// diagBundleWorkerPathR715 — путь диагностического архива воркера по типу
// бэкенда и имя контракта (для note/ошибок).
//
// Пути намеренно РАЗНЫЕ у воркеров: cppworker отдаёт /api/v1/cppworker/debug/
// bundle с R476, imageworker получил аналог /api/v1/sdworker/debug/bundle в
// R715 (плюс алиас /api/debug/bundle). Один общий путь вида /api/debug/bundle
// не годится: старые образы cppworker его не знают, а «угадывать» путь по
// ответу 404 дороже, чем один раз указать тип.
func diagBundleWorkerPathR715(t types.BackendType) (path, contract string) {
	switch t {
	case types.BackendTypeLlamaCpp:
		return "/api/v1/cppworker/debug/bundle", "GET /api/v1/cppworker/debug/bundle (R476)"
	case types.BackendTypeImage:
		return "/api/v1/sdworker/debug/bundle", "GET /api/v1/sdworker/debug/bundle (R715)"
	default:
		return "", "нет: балансер/доки знают архивы только у llama_cpp и image_cpp"
	}
}

// diagBundleWorkerErrorTextR715 — причина, по которой архив воркера не получен.
//
// Текст начинается с исходной ошибки (как было до R715), дальше — поля, по
// которым видно, что именно случилось: HTTP-код, таймаут, отказ соединения, и
// что с этим делать. «Пусто вместо архива» — не диагноз.
func diagBundleWorkerErrorTextR715(err error, bundleURL, backendType, workerPath string) string {
	var b strings.Builder
	b.WriteString(err.Error())
	b.WriteString("\n")
	b.WriteString("# диагностический архив воркера НЕ получен (R715)\n")
	b.WriteString("url=" + bundleURL + "\n")
	b.WriteString("backend_type=" + backendType + "\n")
	b.WriteString("contract=" + workerPath + "\n")
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		b.WriteString("reason=timeout\n")
		b.WriteString(fmt.Sprintf("timeout_sec=%d\n", int(diagBundleWorkerTimeoutR715/time.Second)))
		b.WriteString("hint=за " + fmt.Sprint(int(diagBundleWorkerTimeoutR715/time.Second)) +
			"с архив не отдан: адрес недостижим (блэкхол/сеть) или воркер завис посреди сбора; " +
			"проверьте /health воркера и что он не занят загрузкой модели\n")
	case strings.HasPrefix(err.Error(), "backend "):
		// Диагностический GET вернул не-200: код уже в тексте ошибки.
		b.WriteString("reason=http_status\n")
		if strings.Contains(err.Error(), " 404") {
			b.WriteString("hint=эндпоинта нет в образе воркера: он старше R715 — соберите образ с новым debug_bundle.go\n")
		} else if strings.Contains(err.Error(), " 401") || strings.Contains(err.Error(), " 403") {
			b.WriteString("hint=воркер отклонил токен: сверьте CPPWORKER_API_TOKEN балансера и API_TOKEN воркера\n")
		} else {
			b.WriteString("hint=воркер ответил ошибкой; текст ответа — в первой строке\n")
		}
	default:
		b.WriteString("reason=connection\n")
		b.WriteString("hint=воркер недоступен по сети/порту: проверьте host/port бэкенда и что контейнер поднят\n")
	}
	return b.String()
}

// diagBundleHTTPGetR476 — GET с коротким таймаутом и токеном воркера.
//
// Зачем свой клиент: диагностика не должна ни висеть минутами, ни тянуть
// соединения из общего пула с рабочими таймаутами.
func diagBundleHTTPGetR476(url, token string) ([]byte, error) {
	return diagBundleHTTPGetTimeoutR715(url, token, diagBundlePerBackendTimeoutR476)
}

// diagBundleHTTPGetTimeoutR715 — тот же GET с явным таймаутом (R715).
//
// Таймаут разбора архива воркера (5 с) короче общего (20 с), поэтому длина
// вынесена в параметр. Ошибка таймаута помечается errors.Is(context.
// DeadlineExceeded) — по этой метке в архив пишется reason=timeout, а не
// невнятное «context deadline exceeded».
func diagBundleHTTPGetTimeoutR715(url, token string, timeout time.Duration) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(token) != "" {
		req.Header.Set("X-API-Token", token)
	}
	client := &http.Client{Timeout: timeout}
	resp, err := client.Do(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, fmt.Errorf("timeout: %s не ответил за %s: %w", url, timeout, err)
		}
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	limited := http.MaxBytesReader(nil, resp.Body, diagBundleEntryMaxR476)
	body := make([]byte, 0, 64*1024)
	buf := make([]byte, 32*1024)
	for {
		n, readErr := limited.Read(buf)
		if n > 0 {
			body = append(body, buf[:n]...)
		}
		if readErr != nil {
			break
		}
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("backend %s вернул %d: %s", url, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return body, nil
}

// diagBundleFlagsTextR476 — флаги «задано/не задано» в виде текста.
func diagBundleFlagsTextR476() string {
	snap := diagFlagsSnapshotR476()
	set, _ := snap["set"].(map[string]string)
	unset, _ := snap["unset"].([]string)
	var b strings.Builder
	b.WriteString("# Диагностические флаги балансера (R476)\n")
	b.WriteString("# unset = переменная не задана; действует значение по умолчанию из кода\n\n")
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		b.WriteString(k + "=" + set[k] + "\n")
	}
	for _, k := range unset {
		b.WriteString(k + "= (unset)\n")
	}
	return b.String()
}

// diagBundleBalancerLogsR476 — снимок кольцевого буфера логов.
func diagBundleBalancerLogsR476() string {
	return diagBundleBalancerLogsSinceR476(time.Time{})
}

// diagFieldValueR476 — значение поля в одну строку.
//
// R493-12a-dump: формат переехал в pkg/logger (FieldValueText), потому что тот же
// дамп пишет воркер (`cmd/cppworker/debug_bundle.go`), и два независимых
// форматтера расходились бы порядком ключей и кавычками. Оставлено как тонкая
// обёртка: имя используется в тестах и в логах разбора.
func diagFieldValueR476(v interface{}) string { return logger.FieldValueText(v) }

// diagBundleBalancerLogsSinceR476 — только записи позже cutoff (нулевое время =
// без фильтра).
func diagBundleBalancerLogsSinceR476(cutoff time.Time) string {
	return diagBundleBalancerLogsSnapshotR715(cutoff).Text
}

// diagLogsSnapshotR715 — снимок логов балансера для архива: текст, записи
// массивом и метаданные окна (R715, D3 часть 3).
type diagLogsSnapshotR715 struct {
	Text    string
	Entries []logger.LogEntry
	// JSON — готовый массив записей (тот же срез, что Entries, но с пределом по
	// байтам; собирается в diagBundleBalancerLogsSnapshotR715).
	JSON string
	Meta map[string]interface{}
}

// JSONText — записи массивом. Массив, а не объект: так формат остаётся прежним
// (экспорт /api/v1/logs/export?format=json отдаёт массив), новые поля
// (repeated/first_time/last_time) добавляются ВНУТРЬ элементов.
func (s diagLogsSnapshotR715) JSONText() string {
	if s.JSON != "" {
		return s.JSON
	}
	raw, err := json.MarshalIndent(s.Entries, "", "  ")
	if err != nil {
		return "# JSON логов не собран: " + err.Error() + "\n"
	}
	return string(raw) + "\n"
}

// diagBundleBalancerLogsSnapshotR715 — снимок буфера логов с фильтром since,
// пределами и метаданными окна.
//
// ПОЧЕМУ МЕТАДАННЫЕ. Дефект D3 начинался с «архив покрывает 13 минут, и мы не
// знали об этом до разбора». Теперь logs-meta.json сразу отвечает на вопросы:
// сколько строк пришло, сколько схлопнуто повторами, какое время буфер покрывает
// (window_seconds/first_time/last_time) и усечён ли он пределами архива.
func diagBundleBalancerLogsSnapshotR715(cutoff time.Time) diagLogsSnapshotR715 {
	snap := diagLogsSnapshotR715{Meta: map[string]interface{}{
		"generated_at": time.Now().UTC().Format(time.RFC3339),
		"since":        diagSinceTextR715(cutoff),
	}}
	broker := logger.GetBroker()
	if broker == nil {
		snap.Text = "# кольцевой буфер логов не включён (LB_LOG_BUFFER_ENTRIES=0)\n"
		snap.Entries = []logger.LogEntry{}
		snap.Meta["note"] = "кольцевой буфер не включён (LB_LOG_BUFFER_ENTRIES=0)"
		return snap
	}

	stats := broker.Stats()
	all := broker.Snapshot()
	filtered := make([]logger.LogEntry, 0, len(all))
	for _, e := range all {
		if !cutoff.IsZero() && e.LastOccurrence().Before(cutoff) {
			continue
		}
		filtered = append(filtered, e)
	}

	// Пределы: не больше maxEntries записей и не больше maxBytes текста. Режем
	// с ГОЛОВЫ (старое): при разборе свежее важнее, а усечение называется явно.
	truncated := false
	if len(filtered) > diagBundleLogsMaxEntriesR715 {
		truncated = true
		filtered = filtered[len(filtered)-diagBundleLogsMaxEntriesR715:]
	}

	// Текст собираем построчно и останавливаемся на пределе байтов: иначе один
	// «болтливый» интервал раздувает скачиваемый архив.
	var b strings.Builder
	linesRendered := 0
	for _, e := range filtered {
		line := diagBundleLogLineR715(e)
		if b.Len()+len(line) > diagBundleLogsMaxBytesR715 {
			truncated = true
			break
		}
		b.WriteString(line)
		linesRendered++
	}
	if linesRendered == 0 {
		if len(filtered) > 0 {
			// Первая же строка больше предела — отдаём её усечённой, иначе файл
			// будет пустым и «пусто» прочитается как «логов нет».
			line := diagBundleLogLineR715(filtered[0])
			if len(line) > diagBundleLogsMaxBytesR715 {
				line = line[:diagBundleLogsMaxBytesR715]
			}
			b.WriteString(line)
			linesRendered = 1
		} else {
			b.WriteString("# буфер пуст\n")
		}
	}
	if truncated {
		fmt.Fprintf(&b, "\n# ОБРЕЗАНО (R715): показаны последние %d записей из %d "+
			"(пределы архива: %d записей / %d байт). Полный буфер — GET /api/v1/logs/export\n",
			linesRendered, len(all), diagBundleLogsMaxEntriesR715, diagBundleLogsMaxBytesR715)
	}
	snap.Text = b.String()

	// JSON: те же записи, но с собственным пределом байт. Массив придётся
	// урезать итеративно — размер элемента заранее неизвестен.
	jsonEntries := filtered[:0:0]
	jsonEntries = append(jsonEntries, filtered...)
	jsonText := ""
	for attempt := 0; ; attempt++ {
		raw, err := json.Marshal(jsonEntries)
		if err != nil {
			jsonText = "# JSON логов не собран: " + err.Error() + "\n"
			snap.Meta["json_error"] = err.Error()
			break
		}
		if len(raw) <= diagBundleLogsMaxBytesR715 || len(jsonEntries) <= 1 || attempt >= 8 {
			rawIndented, indErr := json.MarshalIndent(jsonEntries, "", "  ")
			if indErr != nil {
				jsonText = string(raw) + "\n"
			} else {
				jsonText = string(rawIndented) + "\n"
			}
			if len(raw) > diagBundleLogsMaxBytesR715 {
				truncated = true
				snap.Meta["json_truncated"] = true
			}
			break
		}
		cut := len(jsonEntries) / 4
		if cut < 1 {
			cut = 1
		}
		jsonEntries = jsonEntries[cut:]
		truncated = true
		snap.Meta["json_truncated"] = true
	}
	snap.Entries = jsonEntries
	if jsonText != "" && len(jsonText) > diagBundleLogsMaxBytesR715 {
		jsonText = jsonText[:diagBundleLogsMaxBytesR715]
	}
	snap.JSON = jsonText

	snap.Meta["entries_in_buffer"] = len(all)
	snap.Meta["entries_in_window"] = len(filtered)
	snap.Meta["entries_written"] = linesRendered
	snap.Meta["aggregating"] = stats.Aggregating
	snap.Meta["buffer_capacity"] = stats.Capacity
	snap.Meta["published_total"] = stats.PublishedTotal
	snap.Meta["collapsed_total"] = stats.CollapsedTotal
	snap.Meta["approx_bytes"] = stats.ApproxBytes
	snap.Meta["truncated"] = truncated
	if !stats.FirstTime.IsZero() {
		snap.Meta["first_time"] = stats.FirstTime.UTC().Format(time.RFC3339Nano)
		snap.Meta["last_time"] = stats.LastTime.UTC().Format(time.RFC3339Nano)
		snap.Meta["window_seconds"] = int64(stats.LastTime.Sub(stats.FirstTime).Seconds())
	}
	return snap
}

// diagBundleLogLineR715 — одна строка текстового дампа.
//
// Формат прежний (время, уровень, источник, сообщение, поля), поэтому парсеры и
// глаза оператора не меняются. Для схлопнутой записи в КОНЕЦ строки добавлен
// счётчик повторов и границы времени — новые поля, прежние не тронуты.
func diagBundleLogLineR715(e logger.LogEntry) string {
	src := e.Source
	if src == "" {
		src = "balancer"
	}
	var b strings.Builder
	b.WriteString(fmt.Sprintf("%s %-5s %-9s %s",
		e.Time.Format("2006-01-02T15:04:05.000Z07:00"),
		strings.ToUpper(e.Level), src, e.Message))
	// R493 (2026-11-12, по архиву стенда): в файл попадали ТОЛЬКО сообщения,
	// структурированные поля терялись. Оператор видел «request trace» без
	// request_id/path/status/duration, «свободный узел пропущен» — без
	// backend/model/need_mb/free_mb, то есть архив, сделанный ради разбора,
	// не позволял разобрать ничего. Поля есть в LogEntry.Fields — печатаем их
	// общим форматтером (logger.FieldsText: стабильный порядок ключей, тот же
	// вид, что в архиве воркера).
	b.WriteString(logger.FieldsText(e.Fields))
	// R715: повторяющееся событие — одна строка вместо N. repeated/first/last
	// названы так же, как поля JSON, чтобы разбор текста и JSON совпадал.
	if e.Repeated > 1 {
		fmt.Fprintf(&b, " repeated=%d first=%s last=%s",
			e.Repeated,
			e.FirstOccurrence().UTC().Format("2006-01-02T15:04:05.000Z"),
			e.LastOccurrence().UTC().Format("2006-01-02T15:04:05.000Z"))
	}
	b.WriteString("\n")
	return b.String()
}

// diagSinceTextR715 — как записан параметр since в метаданных.
func diagSinceTextR715(cutoff time.Time) string {
	if cutoff.IsZero() {
		return "нет (весь буфер)"
	}
	return cutoff.UTC().Format(time.RFC3339)
}

// diagBundleSinceCutoffR476 — разобрать `since`: длительность ("30m", "2h") или
// RFC3339. Пустое/мусор = без фильтра: «не понял параметр» не должно молча
// отдавать пустой архив.
func diagBundleSinceCutoffR476(raw string) time.Time {
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
	logger.Get().Warnw("diag/bundle: since не разобран, фильтр не применён", "since", raw)
	return time.Time{}
}

// diagBundleAddJSONR476 — JSON-файл архива.
func (s *Server) diagBundleAddJSONR476(zw *zip.Writer, name string, value interface{}) {
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		s.diagBundleAddTextR476(zw, name+".error.txt", err.Error()+"\n")
		return
	}
	s.diagBundleAddTextR476(zw, name, string(raw)+"\n")
}

// diagBundleAddTextR476 — текстовый файл архива (ошибки пишем в лог, но архив
// не роняем: лучше неполный архив, чем никакого).
func (s *Server) diagBundleAddTextR476(zw *zip.Writer, name, text string) {
	f, err := zw.Create(name)
	if err != nil {
		logger.Get().Warnw("diag/bundle: файл не создан", "file", name, "error", err)
		return
	}
	if len(text) > diagBundleEntryMaxR476 {
		text = text[:diagBundleEntryMaxR476] + "\n\n# ОБРЕЗАНО: файл больше лимита\n"
	}
	if _, err := f.Write([]byte(text)); err != nil {
		logger.Get().Warnw("diag/bundle: файл не записан", "file", name, "error", err)
	}
}

// diagBundleReadmeR476 — как читать архив.
const diagBundleReadmeR476 = `Диагностический архив OllamaLegion (R476, дополнен R715).

Состав:
  summary.json        — версии ВСЕХ сервисов (секция services: balancer, webui,
                        cppworker[], imageworker[], agent[]), флаги, конфигурация
                        окна, профили моделей, снимок кластера (бэкенды, модели,
                        очередь)
  balancer-logs.txt   — кольцевой буфер логов балансера: время, уровень,
                        источник, сообщение и структурированные поля (key=value).
                        Повторяющееся событие — ОДНА строка с repeated=N
                        и границами first=/last=
  balancer-logs.json  — тот же буфер массивом записей (time/level/message/source/
                        fields + repeated/first_time/last_time)
  logs-meta.json      — метаданные окна логов: сколько строк пришло, сколько
                        схлопнуто, какое время буфер покрывает (window_seconds),
                        усечён ли файл пределами архива
  failures.json       — последние сбои по стадиям (какой компонент отказал)
  identity.json       — запечённая идентичность образа балансера
  flags.txt           — флаги по белому списку (set / unset)
  backends/<id>.bundle.zip — архив воркера: его логи, модели и их состояние,
                        версия/identity, состояние загрузок, конфиг (см. README
                        внутри). Есть у llama_cpp (cppworker) и у image_cpp
                        (imageworker); во втором случае ZIP собран
                        /api/v1/sdworker/debug/bundle
  backends/<id>.info.json  — /api/info воркера
  backends/<id>.error.txt  — архив воркера НЕ получен: причина (http_status/
                        timeout/connection) и подсказка
  backends/<id>.note.txt   — только для типа без архива по контракту

Как связать ход клиента с логами:
  1) в balancer-logs.txt найдите строку "request trace" нужного времени;
     у неё есть request_id, path, status, duration_ms, backend;
  2) по этому request_id в backends/<id>.bundle.zip (logs.txt) найдите
     "/api/chat stream response summary" — там streamed_content_bytes,
     emitted_len, final_content_len, tool_calls, done_reason, generated_tokens,
     budget_exhausted и dumped_args_instead_of_call.

Почему в логах «одна строка вместо тысячи»: одинаковые события схлопываются
(R715) — иначе 1565 одинаковых WARN вытесняли из буфера всё остальное, и архив
покрывал 13 минут вместо часов. Числа в тексте сообщения при схлопывании
сравниваются без цифр, а идентичность (request_id/backend/model/path) — точно:
разные запросы не склеиваются.

Как читать схлопнутую запись: время в начале строки — ПЕРВОЕ появление события,
дальше repeated=N (сколько раз повторилось) и first=/last= (границы). Такая
запись стоит на месте первого появления, а её last может быть позже следующих
строк. При «?since=» запись остаётся, если в окно попало хотя бы последнее
появление.

Чего в архиве НЕТ: токенов и паролей.
`
