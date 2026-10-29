package balancer

// model_load_wait_r262.go — R262 (2026-10-28): таймаут ожидания загрузки —
// это «ещё грузится», а НЕ провал.
//
// ЖИВОЙ ФАКТ (стенд 8 ГБ VRAM). `POST` загрузки Qwen3.8-27B вернул HTTP 500
// («poll timeout») через ~380 с, при этом cppworker модель догрузил и через
// несколько десятков секунд она была `state=loaded`. Оператор видел «ошибка
// загрузки» при фактически загруженной модели: жал «ещё раз» и получал
// непредсказуемую перезагрузку (модель выгружалась/грузилась заново).
//
// ПРИЧИНА. `pollLoadCompletionUntilLoaded` по истечении maxWait возвращал nil, а
// вызывающие ветки (HTTP-таймаут и 202-Accepted) превращали nil в ошибку:
//   - HTTP-taimаут: "request failed: context deadline exceeded" → 500;
//   - 202: "model not ready after 3m20s" → 500.
// То есть «мы не дождались» и «загрузка провалилась» были одним и тем же
// ответом. Клиент (WebUI) не мог отличить одно от другого и не имел способа
// узнать финал.
//
// РЕШЕНИЕ. Перед вердиктом — ФИНАЛЬНАЯ сверка состояния с воркером (после
// таймаута модель часто уже загружена). Если загружена — это успех. Если всё
// ещё `loading` — результат помечается StillLoading, и HTTP-слой отдаёт
// 202 Accepted с `pollAfterSec`: «работа идёт, спроси позже», без 5xx.
// Ошибка остаётся только для реального провала (failed/нет модели/воркер
// недоступен).

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

// loadStateOnWorkerR262 — спросить у воркера состояние конкретной модели.
//
// Возвращает (state, known): state нормализован в нижний регистр ("loaded",
// "loading", "failed", ...); known=false — модели в списке воркера нет
// (или спросить не удалось), то есть судить о её судьбе нельзя.
//
// Отдельный короткий запрос, а не переиспользование поллинга: вызывается РОВНО
// один раз, когда мы уже собирались объявить таймаут. Ошибка сети здесь не
// превращается в провал загрузки — она означает «не знаем».
func (mm *ModelManager) loadStateOnWorkerR262(host string, port int, modelName string) (string, bool) {
	if strings.TrimSpace(host) == "" || port <= 0 || strings.TrimSpace(modelName) == "" {
		return "", false
	}
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(fmt.Sprintf("http://%s:%d/api/models", host, port))
	if err != nil {
		logger.Get().Debugw("loadStateOnWorkerR262: /api/models недоступен",
			"host", host, "port", port, "error", err)
		return "", false
	}
	defer resp.Body.Close()
	body, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		return "", false
	}
	var modelsResp struct {
		Models []struct {
			Name  string `json:"name"`
			State string `json:"state"`
		} `json:"models"`
	}
	if err := json.Unmarshal(body, &modelsResp); err != nil {
		return "", false
	}
	for _, m := range modelsResp.Models {
		if !modelNameMatches(m.Name, modelName) {
			continue
		}
		return strings.ToLower(strings.TrimSpace(m.State)), true
	}
	return "", false
}

// stillLoadingResultR262 — результат «загрузка ещё идёт» для HTTP-слоя.
//
// Success=false (загрузка НЕ подтверждена), но StillLoading=true и Error пустой:
// это не провал, а незавершённое ожидание. Такой ответ уходит клиенту как
// 202 Accepted, и панель показывает «загружается», а не «ошибка загрузки».
func stillLoadingResultR262(req ModelOpRequest, backendID string, maxWait time.Duration) *ModelOpResult {
	pollAfter := int((30 * time.Second).Seconds())
	return &ModelOpResult{
		Success:      false,
		StillLoading: true,
		Operation:    req.Operation,
		ModelName:    req.ModelName,
		BackendID:    backendID,
		PollAfterSec: pollAfter,
		Message: fmt.Sprintf(
			"модель «%s» ещё загружается на бэкенде «%s» (ждали %v): загрузка продолжается в воркере, "+
				"опрос через %ds покажет итог — это не ошибка",
			req.ModelName, backendID, maxWait.Round(time.Second), pollAfter),
	}
}

// stillLoadingAfterLoadRetriesR299 — «воркер всё ещё грузит модель после всех
// попыток» (R299).
//
// ЖИВОЙ ДЕФЕКТ (стенд 793db393, 148 с): эта ситуация возвращалась как
// ModelOpResult{Success:false, Error:"cppworker still loading model ..."}, то
// есть как ОШИБКА, а HTTP-слой по правилу отдавал 500. Текст при этом был
// «модель ещё загружается» — балансер сам себе противоречил: 5xx значит
// «сломалось, повтори», и оператор нажимал «загрузить» поверх живой загрузки.
//
// Правило уже существует (R262): «ещё идёт» — это StillLoading + 202 + совет
// опросить статус, а не Error. Здесь оно просто применяется к последней попытке
// цикла загрузки; ждать заново те же 30 с смысла нет, потому что следующий
// опрос состояния делает клиент.
func stillLoadingAfterLoadRetriesR299(req ModelOpRequest, backendID string) *ModelOpResult {
	return stillLoadingResultR262(req, backendID,
		time.Duration(llamaCppLoadMaxRetries)*llamaCppLoadRetryInterval)
}

// loadedModelsCountR262 — сколько моделей РЕАЛЬНО загружено в переданном списке.
//
// ЖИВОЙ ФАКТ (диагностика врала): поллер писал
// «llamaCppMetricsPoller: updated llama.cpp metrics … loaded_models:2», считая
// `len(loadedModels)` — то есть модели в ЛЮБОМ состоянии (включая `loading` и
// `error`). Оператор в логе видел «2 загружено», когда одна ещё грузилась.
//
// Считаем строго `state == "loaded"` — тот же критерий, что у R257
// (`loadedSetFromSnapshotR257`), чтобы диагностика и API не расходились.
func loadedModelsCountR262(models []types.LlamaCppModel) int {
	n := 0
	for _, m := range models {
		if strings.EqualFold(strings.TrimSpace(m.State), "loaded") {
			n++
		}
	}
	return n
}
