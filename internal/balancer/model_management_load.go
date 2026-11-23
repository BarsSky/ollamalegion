// model_management_load.go — R632 (2026-11-19): ожидание загрузки: pollLoadCompletionUntilLoaded, прогресс, сборка тела загрузки для cppworker.
//
// ЗАЧЕМ ВЫНЕСЕНО. `model_management.go` держал 2172 строки: конструктор,
// операции, ожидание загрузки, отдельный 447-строчный цикл загрузки llama.cpp и
// общий транспорт. Здесь — один из слоёв; поведение не менялось, код перенесён как есть.
package balancer

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"ollama-loadbalancer/internal/i18n"
	"ollama-loadbalancer/pkg/logger"
	"strings"
	"time"
)

// pollLoadCompletionUntilLoaded — после HTTP timeout на /api/models/load
// проверяет cppworker (state="loaded" в /api/models) периодически до завершения
// загрузки или до истечения maxWait. Возвращает success если модель загружена,
// nil если polling превысил maxWait.
//
// Round 8 (2026-07-10): решает проблему "model loaded but client got timeout":
//  1. cppworker начинает загружать 21GB модель (5-10 мин).
//  2. balancer HTTP client timeout (default 120s) срабатывает.
//  3. НО cppworker всё ещё грузит — polling показывает state="loading".
//  4. По завершении cppworker state="loaded" — мы возвращаем success клиенту.
//
// Без этой логики клиент получал ошибку даже при успешной загрузке.
func (mm *ModelManager) pollLoadCompletionUntilLoaded(
	host string, port int, backendID string, modelName string, maxWait time.Duration,
) *ModelOpResult {
	if maxWait <= 0 {
		maxWait = 5 * time.Minute
	}
	deadline := time.Now().Add(maxWait)
	pollInterval := 2 * time.Second
	pollClient := &http.Client{
		Timeout: 10 * time.Second, // poll-запросы быстрые
	}

	logger.Get().Infow("executeLlamaCppLoad: polling for model load completion",
		"backend", backendID, "model", modelName,
		"max_wait", maxWait, "poll_interval", pollInterval)

	// R66d (2026-09-22): сколько поллов подряд модель не появляется ни в
	// /api/models, ни в /api/models/load/progress. Раньше провалившаяся загрузка
	// была неотличима от «ещё грузится»: cppworker удалял запись о модели и
	// нигде не хранил причину, поэтому поллинг шёл до maxWait (3-15 минут),
	// операция висела в статусе running, а клиент получал 503 «подожди 90с».
	//
	// 5 поллов × 2 с = 10 с «льготного окна»: за это время cppworker успевает
	// создать запись state=loading (она появляется в начале LoadModelWithOpts, но
	// между 202 Accepted и созданием записи есть окно на планировщик и проверку
	// ресурсов/чтение GGUF-заголовка). Если запись так и не появилась — загрузка
	// не идёт, и врать клиенту «жди 90с» смысла нет.
	const missPollsBeforeFail = 5
	missPolls := 0

	for time.Now().Before(deadline) {
		time.Sleep(pollInterval)

		// 1. /api/models — там state="loaded"/"loading".
		modelsURL := fmt.Sprintf("http://%s:%d/api/models", host, port)
		resp, err := pollClient.Get(modelsURL)
		if err != nil {
			logger.Get().Debugw("pollLoadCompletion: /api/models poll failed",
				"backend", backendID, "error", err)
			continue
		}
		body, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			continue
		}

		var modelsResp struct {
			Models []struct {
				Name  string `json:"name"`
				State string `json:"state"`
			} `json:"models"`
		}
		if json.Unmarshal(body, &modelsResp) != nil {
			continue
		}

		seen := false
		for _, m := range modelsResp.Models {
			// R66d: cppworker регистрирует модель БЕЗ расширения .gguf, а клиент
			// (WebUI-карточка файла, Cline) присылает имя С расширением. Точное
			// сравнение объявляло успешную загрузку провалившейся (5 miss-поллов)
			// и WebUI показывал «Ошибка загрузки» при загруженной модели.
			if !modelNameMatches(m.Name, modelName) {
				continue
			}
			seen = true
			if m.State == "loaded" {
				logger.Get().Infow("executeLlamaCppLoad: model loaded successfully (recovered from HTTP timeout)",
					"backend", backendID, "model", modelName,
					"elapsed", maxWait-time.Until(deadline))
				return &ModelOpResult{
					Success:   true,
					Operation: "load",
					ModelName: modelName,
					BackendID: backendID,
					Message:   "model loaded (load request timed out but polling confirmed completion)",
				}
			}
		}

		// 2. /api/models/load/progress — там state="failed" + error (R66d).
		// Это основной сигнал: cppworker теперь помнит причину провала.
		state, errText, reason, fromProgress := mm.fetchLoadProgressWithReason(pollClient, host, port, backendID, modelName)
		if state == "failed" {
			logger.Get().Errorw("executeLlamaCppLoad: load reported failed by cppworker, stop polling",
				"backend", backendID, "model", modelName, "error", errText, "reason", reason,
				"elapsed_ms", int(time.Since(deadline.Add(-maxWait)).Milliseconds()))
			// R697: неподдерживаемая архитектура — терминальный отказ воркера, а не
			// наш сбой. StatusCode воркера переносим в результат, чтобы HTTP-слой
			// отдал 409 (modelOpStatusCode), а не 500 «повтори».
			workerStatus := 0
			if reason == "model_unsupported" {
				workerStatus = http.StatusConflict
			}
			return &ModelOpResult{
				Success:      false,
				Operation:    "load",
				ModelName:    modelName,
				BackendID:    backendID,
				WorkerStatus: workerStatus,
				Error:        fmt.Sprintf("auto-load failed: %s", errText),
			}
		}

		// 3. Страховка: модель не видна ни как loading, ни как loaded.
		// Успешная загрузка сразу появляется в /api/models со state=loading,
		// поэтому несколько «пустых» поллов подряд означают, что загрузка
		// не идёт (файла нет / упала до создания записи).
		loading := state == "loading"
		if !seen && !loading && !fromProgress {
			missPolls++
			if missPolls >= missPollsBeforeFail {
				logger.Get().Errorw("executeLlamaCppLoad: model never appeared in cppworker list/progress, stop polling",
					"backend", backendID, "model", modelName, "miss_polls", missPolls)
				return &ModelOpResult{
					Success:   false,
					Operation: "load",
					ModelName: modelName,
					BackendID: backendID,
					Error: i18n.T("balancer.model_management.auto_load_not_appeared_error",
						modelName, missPolls),
				}
			}
		} else {
			missPolls = 0
		}
	}

	logger.Get().Warnw("executeLlamaCppLoad: poll for load completion exceeded max_wait",
		"backend", backendID, "model", modelName, "max_wait", maxWait)

	// R262 (2026-10-28): таймаут ожидания — НЕ вердикт «провал».
	//
	// Живой факт: загрузка Qwen3.8-27B вернула HTTP 500 «poll timeout» через
	// ~380 с, а воркер догрузил модель через несколько десятков секунд. Оператор
	// видел ошибку при фактически загруженной модели. Поэтому перед вердиктом —
	// ФИНАЛЬНАЯ сверка с воркером: после таймаута модель часто уже `loaded`.
	if state, known := mm.loadStateOnWorkerR262(host, port, modelName); known {
		if state == "loaded" {
			logger.Get().Infow("executeLlamaCppLoad: модель загружена к моменту таймаута ожидания — это успех",
				"backend", backendID, "model", modelName, "max_wait", maxWait)
			return &ModelOpResult{
				Success:   true,
				Operation: "load",
				ModelName: modelName,
				BackendID: backendID,
				Message:   "model loaded (wait window expired, but the worker reports state=loaded)",
			}
		}
		if state == "loading" || state == "" {
			// Загрузка идёт: отдаём «ещё грузится», а не ошибку. Пустой state у
			// известной воркеру модели означает «запись есть, состояние не
			// сообщено» — трактовать это как провал ещё хуже, чем как «идёт».
			return stillLoadingResultR262(ModelOpRequest{
				Operation: "load", ModelName: modelName,
			}, backendID, maxWait)
		}
		// Прочие известные состояния (failed) остаются провалом: их обрабатывает
		// ветка progress выше; здесь возвращаем nil, сохраняя прежнее поведение.
	}
	return nil
}

// fetchLoadProgress — состояние загрузки модели из /api/models/load/progress.
//
// R66d: возвращает state ("loading"/"loaded"/"failed"), текст ошибки (для
// failed) и признак того, что cppworker вообще знает про эту модель
// (loaded/loading/failed). Пустой state + false = cppworker ничего не знает.
func (mm *ModelManager) fetchLoadProgress(
	client *http.Client, host string, port int, backendID, modelName string,
) (state string, errText string, known bool) {
	state, errText, _, known = mm.fetchLoadProgressWithReason(client, host, port, backendID, modelName)
	return state, errText, known
}

// fetchLoadProgressWithReason — то же, плюс машиночитаемый код причины (R697).
//
// ЗАЧЕМ КОД. Провал «архитектура не поддерживается сборкой llama.cpp» нельзя считать
// транзиентным: 500 на него значит «повтори, станет лучше», и клиенты (Cline,
// OpenWebUI) повторяют, а оператор видит поломку вместо ограничения движка. По коду
// async-путь загрузки отдаёт терминальный 409 — ровно как это уже делает sync-путь
// самого cppworker (cmd/cppworker/model_unsupported.go).
func (mm *ModelManager) fetchLoadProgressWithReason(
	client *http.Client, host string, port int, backendID, modelName string,
) (state string, errText string, reason string, known bool) {
	url := fmt.Sprintf("http://%s:%d/api/models/load/progress?model=%s", host, port, urlQueryEscape(modelName))
	resp, err := client.Get(url)
	if err != nil {
		logger.Get().Debugw("pollLoadCompletion: load/progress poll failed",
			"backend", backendID, "model", modelName, "error", err)
		return "", "", "", false
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		// «model not found and not loading» — cppworker не знает про модель.
		return "", "", "", false
	}
	body, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		return "", "", "", false
	}
	var pr struct {
		State  string `json:"state"`
		Error  string `json:"error"`
		Reason string `json:"reason"`
	}
	if json.Unmarshal(body, &pr) != nil {
		return "", "", "", false
	}
	switch pr.State {
	case "failed":
		return "failed", pr.Error, pr.Reason, true
	case "loading", "loaded":
		return pr.State, pr.Error, pr.Reason, true
	default:
		return "", pr.Error, pr.Reason, false
	}
}

// urlQueryEscape — минимальное экранирование имени модели для query-параметра.
// Имена моделей у нас обычно [A-Za-z0-9._:-], но бывают и со слэшами (HF).
func urlQueryEscape(s string) string {
	return strings.ReplaceAll(url.QueryEscape(s), "+", "%20")
}

// executeLlamaCppLoad — загрузка модели в память на cppworker-бэкенде.
// cppworker принимает POST /api/models/load с JSON {"name": "...", ...}.
// Если в req заданы ContextSize или GPULayers — передаёт их в теле.
//
// Round 7: если заданы OverrideTensors/OverrideTensorBufts (явно в req или
// через сохранённый LlamaCppModelProfile) — роутим на /api/models/load-with-params
// чтобы cppworker применил per-tensor routing для MoE моделей.
//
// При получении HTTP 503 с body содержащим "model is loading" — повторяет запрос
// через llamaCppLoadRetryInterval, до llamaCppLoadMaxRetries попыток. Это
// закрывает race condition, когда параллельные запросы приходят на cppworker
// в момент, когда другая горутина уже грузит эту же модель (другая запрос
// получил `TryLockLoad=false`, а handleLoadModel ещё не завершил WaitForLoad).
// llamaCppLoadBodyKeys — КОНТРАКТ тех ключей, которые балансер кладёт в тело
// POST /api/models/load (или /api/models/load-with-params) для cppworker.
//
// ЗАЧЕМ СПИСОК. cppworker декодирует тело СТРОГИМ декодером
// (pkg/types.DecodeJSONRequest → json.Decoder.DisallowUnknownFields), поэтому
// любое поле, которое балансер добавил, а loadModelRequest /
// loadWithParamsRequest (cmd/cppworker/types.go) не знает, превращается в
// HTTP 400 `invalid JSON: json: unknown field "..."` — и загрузка модели падает
// ЦЕЛИКОМ, а не деградирует. Так было уже дважды:
//   - R44 (2026-08-19): kvCacheType / reason на legacy /api/models/load;
//   - R83/v67 (2026-10-02): enableReasoning — профиль с галочкой «размышления»
//     вообще не давал модель поднять (см. лог балансера: «async load failed»).
//
// Дисциплина: добавление ключа сюда ОБЯЗАНО сопровождаться приёмом поля на
// стороне cppworker. Тесты держат обе стороны синхронными:
//   - internal/balancer: TestLlamaCppLoadBody_KeyContract
//   - cmd/cppworker:     TestBalancerLoadBody_AcceptedByLegacyAndParamsEndpoints
var llamaCppLoadBodyKeys = []string{
	"name",
	"contextSize",
	"gpuLayers",
	"kvCacheType",
	"useMmap",
	"flashAttn",
	"batchSize",
	"parallel",
	"contextPerSeq",
	"enableReasoning",
	"overrideTensors",
	"overrideTensorBufts",
}

// buildLlamaCppLoadBody собирает тело запроса на загрузку модели для cppworker.
//
// Вынесено из executeLlamaCppLoad отдельной функцией, чтобы контракт ключей
// (llamaCppLoadBodyKeys) можно было проверить тестом без HTTP и без живого
// бэкенда. Поля с nil-указателем не попадают в тело вовсе: cppworker тогда
// берёт свой env/config-дефолт (см. handleLoadModel в cmd/cppworker).
//
// R83 (2026-09-30) про contextPerSeq: суммарный n_ctx считает cppworker (у него
// есть и слоты, и env-дефолт CPPWORKER_N_PARALLEL), поэтому передаём как есть и
// НЕ подменяем contextSize.
//
// R83/v67 (2026-10-02) про enableReasoning: приходит сюда из профиля модели
// (WebUI) через applyProfileLoadParams — балансер не имеет права решать за
// оператора, поэтому поле уходит всегда, когда задано.
func buildLlamaCppLoadBody(req ModelOpRequest, overrideTensors, overrideTensorBufts []string, useLoadWithParams bool) map[string]interface{} {
	body := map[string]interface{}{
		"name": req.ModelName,
	}
	if req.ContextSize != nil {
		body["contextSize"] = *req.ContextSize
	}
	if req.GPULayers != nil {
		body["gpuLayers"] = *req.GPULayers
	}
	// R54.6 (2026-08-24): AutoTune fields — apply to /api/models/load body.
	if req.KVCacheType != nil {
		body["kvCacheType"] = *req.KVCacheType
	}
	if req.UseMmap != nil {
		body["useMmap"] = *req.UseMmap
	}
	if req.FlashAttn != nil {
		body["flashAttn"] = *req.FlashAttn
	}
	if req.BatchSize != nil {
		body["batchSize"] = *req.BatchSize
	}
	// R83 (2026-09-29): параллельность и размышления — раньше терялись по пути
	// WebUI → балансер → cppworker (см. комментарий у полей ModelOpRequest).
	if req.Parallel != nil {
		body["parallel"] = *req.Parallel
	}
	if req.ContextPerSeq != nil && *req.ContextPerSeq > 0 {
		body["contextPerSeq"] = *req.ContextPerSeq
	}
	if req.EnableReasoning != nil {
		body["enableReasoning"] = *req.EnableReasoning
	}
	if useLoadWithParams {
		body["overrideTensors"] = overrideTensors
		body["overrideTensorBufts"] = overrideTensorBufts
	}
	return body
}
