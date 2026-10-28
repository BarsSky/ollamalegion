package balancer

// model_loaded_r257.go — R257 (2026-10-28): «загружено» — ОДИН источник истины.
//
// ЖИВОЙ ФАКТ (стенд 8 ГБ VRAM). После того как Qwen3.8-27B была вытеснена моделью
// gemma-4-E4B, `GET /api/v1/backends/{id}/models` продолжал отдавать
// `loaded=true` у ДВУХ моделей одновременно, а `loadedModelCount` был завышен.
// Панель предлагала «загруженную» модель, которой в памяти нет: оператор жал её и
// получал непредсказуемую перезагрузку (модель выгружалась/грузилась заново).
//
// ПРИЧИНА. Флаг `loaded` собирался из ответа cppworker `/api/models` НА МОМЕНТ
// запроса (model_management.go:436-453), а модель-метрики и панель читают снапшот
// поллера. Два независимых «мнения» об одном факте расходятся ровно в момент
// вытеснения: `/api/models` успевает вернуть старое состояние (или состояние
// «пустое», которое поллер нормализовал в `loaded`), а снапшот уже обновился —
// и наоборот.
//
// РЕШЕНИЕ. Источник истины — снапшот поллера (`SnapshotLlamaCppMetrics`), тот же,
// что видят метрики и preflight. Ответ воркера остаётся источником РАНТАЙМ-полей
// (размер, слои, контекст), но не правом объявить модель загруженной. Если снапшота
// ещё нет (поллер не успел опросить узел) — поведение прежнее, иначе на старте
// балансера пропали бы все «загружено».

import (
	"strings"

	"ollama-loadbalancer/pkg/logger"
)

// loadedKeysR257 — ключи, по которым модель ищут в списке файлов.
//
// Ключей три, потому что имя модели приходит в разных видах: имя записи
// (`Qwen3-...gguf`), basename и basename без `.gguf`. Ровно так же строил ключи
// прежний код (model_management.go:444-450) — наблюдаемое поведение не меняем.
func loadedKeysR257(name string) []string {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil
	}
	base := name
	if i := strings.LastIndexAny(base, `/\`); i >= 0 {
		base = base[i+1:]
	}
	keys := []string{name, base}
	if trimmed := strings.TrimSuffix(base, ".gguf"); trimmed != base {
		keys = append(keys, trimmed)
	}
	out := make([]string, 0, len(keys))
	seen := make(map[string]bool, len(keys))
	for _, k := range keys {
		k = strings.TrimSpace(k)
		if k == "" || seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, k)
	}
	return out
}

// loadedSetFromSnapshotR257 — что РЕАЛЬНО загружено по снапшоту поллера.
//
// Строго `State == "loaded"`: снапшот содержит модели в любом состоянии
// (loaded / loading / error / unknown) — он для того и нужен, чтобы показывать
// процесс загрузки. Считать «загруженной» запись со state=loading значило бы
// повторять ту же ошибку, только в другом месте.
//
// known=false означает «снапшота нет» (поллер ещё не опрашивал узел) — вызывающий
// обязан сохранить прежнее поведение, а не объявить пустой список истиной.
func (p *Proxy) loadedSetFromSnapshotR257(backendID string) (map[string]bool, bool) {
	out := make(map[string]bool)
	if p == nil || p.metricsMgr == nil || backendID == "" {
		return out, false
	}
	lm, ok := p.metricsMgr.SnapshotLlamaCppMetrics(backendID)
	if !ok || lm == nil {
		return out, false
	}
	for _, m := range lm.LoadedModels {
		if !strings.EqualFold(strings.TrimSpace(m.State), "loaded") {
			continue
		}
		for _, k := range loadedKeysR257(m.Name) {
			out[k] = true
		}
	}
	return out, true
}

// resolveModelStateR257 — состояние модели для снапшота поллера.
//
// ЧТО БЫЛО НЕ ТАК. Поллер превращал ПУСТОЕ состояние в "loaded"
// (llamacpp_metrics_poller.go:392-396: «если state пустое — считаем loaded»).
// На старой сборке cppworker это было вынужденной мерой (воркер не отдавал state
// вовсе), но с текущей сборкой пустой state означает «модель в списке есть, а
// загружена ли она — не сказано». Из-за нормализации вытесненная модель
// оставалась «загруженной» для всех потребителей снапшота: панель, метрики,
// авто-выбор модели.
//
// ПРАВИЛО. Пустое состояние трактуем как "loaded" ТОЛЬКО при положительных
// признаках загрузки: время загрузки, известное окно контекста или занятая
// память. Иначе честно "unknown" — и тогда флаг loaded не выставляется, а
// диагностика в панели («модель в списке, состояние неизвестно») остаётся
// возможной.
func resolveModelStateR257(state, loadedAt string, contextSize int, vramMB, ramMB uint64) string {
	s := strings.ToLower(strings.TrimSpace(state))
	if s != "" {
		return s
	}
	if strings.TrimSpace(loadedAt) != "" || contextSize > 0 || vramMB > 0 || ramMB > 0 {
		return "loaded"
	}
	return "unknown"
}

// logLoadedDivergenceR257 — одна DEBUG-строка, когда ответ воркера и снапшот
// расходятся. Именно расхождение и было живым дефектом: без такой строки в логах
// невозможно понять, что панель показывала не то, что видит селектор.
//
// Уровень debug, а не warn: расхождение штатно в момент вытеснения (воркер
// отдаёт состояние до перестроения снапшота), и warn на каждый такой случай
// превратился бы в шум на живом стенде.
func logLoadedDivergenceR257(backendID string, worker, snapshot map[string]bool, authoritative bool) {
	if !authoritative {
		return
	}
	var onlyWorker, onlySnapshot []string
	for k := range worker {
		if !snapshot[k] {
			onlyWorker = append(onlyWorker, k)
		}
	}
	for k := range snapshot {
		if !worker[k] {
			onlySnapshot = append(onlySnapshot, k)
		}
	}
	if len(onlyWorker) == 0 && len(onlySnapshot) == 0 {
		return
	}
	logger.Get().Debugw("R257: loaded state differs between worker /api/models and poller snapshot; snapshot wins",
		"backend", backendID,
		"only_in_worker_response", onlyWorker,
		"only_in_snapshot", onlySnapshot)
}
