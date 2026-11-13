// window_decision_paths_r493.go — R493-12n (этап 3 «два мозга»): сборка входа
// для `decideWindowR493` из состояния балансера и флаг переключения.
//
// ФЛАГ — `LB_WINDOW_UNIFIED` (горячая настройка, группа `nctx`):
//
//	0 — историческое поведение: правила окна живут в двух местах (guard R301 для
//	    стриминга и preflight-движок для остального). Значение по умолчанию.
//	1 — единое решение: guard, транспортный preflight и план роста считают вид
//	    вердикта одной функцией `decideWindowR493`.
//	2 — «тень»: единое решение СЧИТАЕТСЯ и расхождения с историческим
//	    записываются в лог, но поведение остаётся историческим. Это режим для
//	    стенда: сначала посмотреть, где пути разойдутся, потом включать.
//
// ЗАЧЕМ РЕЖИМ «ТЕНЬ». Включить единое решение сразу — значит поменять поведение
// на живом стенде без доказательств. Тень даёт ровно то, чего не хватало при
// прошлых правках окна: список расхождений с числами по каждому пути, снятый с
// реального трафика, при нулевом изменении поведения.
package balancer

import (
	"os"
	"strconv"
	"strings"

	"ollama-loadbalancer/internal/settings"
	"ollama-loadbalancer/pkg/logger"
)

// Режимы флага LB_WINDOW_UNIFIED.
const (
	windowUnifiedOffR493    = 0 // историческое поведение (по умолчанию)
	windowUnifiedOnR493     = 1 // единое решение
	windowUnifiedShadowR493 = 2 // считать единое, писать расхождения, вести себя исторически
)

// windowUnifiedModeR493 — действующий режим флага.
//
// Приоритет: горячая настройка (панель) → env → 0. Неизвестное значение —
// историческое поведение: сомнительное значение флага не должно включать новый
// путь само по себе.
func windowUnifiedModeR493() int {
	if v, ok := settings.HotInt("LB_WINDOW_UNIFIED"); ok {
		if v >= windowUnifiedOffR493 && v <= windowUnifiedShadowR493 {
			return int(v)
		}
		return windowUnifiedOffR493
	}
	// Реестра нет (юнит-тесты, лёгкая сборка) — читаем env тем же числовым
	// правилом. Второго парсера (со словами «true»/«shadow») здесь ОСОЗНАННО нет:
	// два разбора одного флага — ровно тот класс, который эта правка убирает.
	raw := strings.TrimSpace(os.Getenv("LB_WINDOW_UNIFIED"))
	if n, err := strconv.Atoi(raw); err == nil && n >= windowUnifiedOffR493 && n <= windowUnifiedShadowR493 {
		return n
	}
	return windowUnifiedOffR493
}

// windowUnifiedDecisionR493 — решение считается новым путём (режим 1) или только
// для сравнения (режим 2).
func windowUnifiedDecisionR493() bool {
	m := windowUnifiedModeR493()
	return m == windowUnifiedOnR493 || m == windowUnifiedShadowR493
}

// windowInputForR493 — собрать вход решения из состояния балансера.
//
// streaming=true означает, что решение спрашивает путь, который сам
// перезагружает модель под стрим (транспортный preflight / guard до очереди).
// Для такого пути в `GrowBlockedBy` попадает причина, по которой рост запрещён
// оператором или ролью узла: так отличие пути становится ВХОДОМ, а не вторым
// набором правил.
func (p *Proxy) windowInputForR493(backendID, model string, body []byte, path string, streaming bool) (windowInputR493, bool) {
	if p == nil || backendID == "" || len(body) == 0 {
		return windowInputR493{}, false
	}
	meta := p.extractRequestMeta(body, path)
	if meta == nil {
		return windowInputR493{}, false
	}
	estimated, loaded, needed := p.contextGrowthNumbers(backendID, model, body, path)
	active, pending := growthBlockersR288(p, backendID, model)
	in := windowInputR493{
		Needed:        needed,
		Estimated:     estimated,
		Requested:     meta.RequestedNCtxOverride,
		Loaded:        loaded,
		Slots:         p.loadedSlotsNCtx(backendID, model),
		Ceiling:       p.physicalNumCtxCeiling(model, backendID),
		Active:        active,
		ReloadPending: pending,
	}
	if streaming {
		in.GrowBlockedBy = p.growBlockedReasonR493(backendID, model)
	}
	return in, true
}

// growBlockedReasonR493 — почему этот путь не может растить окно ("" = может).
//
// Оба запрета — факты, известные ДО перезагрузки, и оба раньше были размазаны:
// выключатель жил только в R385, роль донора — только в preflight. Здесь они
// названы причиной, которую клиент и увидит.
func (p *Proxy) growBlockedReasonR493(backendID, model string) string {
	if p.cannotHostModelR304(backendID) {
		return "бэкенд — донор раскладки, своих копий он не держит"
	}
	if !streamGrowthEnabledR385() {
		return "рост окна на стриминге выключен (LB_NCTX_GROW_FOR_STREAM=0)"
	}
	return ""
}

// windowDecisionForR493 — решение по окну для конкретного запроса.
//
// Возвращает (вердикт, собрали_ли_вход). Второе значение нужно вызывающему,
// чтобы отличить «решено, что делать нечего» от «входа нет, решать нечем».
func (p *Proxy) windowDecisionForR493(backendID, model string, body []byte, path string, streaming bool) (windowDecisionR493, bool) {
	in, ok := p.windowInputForR493(backendID, model, body, path, streaming)
	if !ok {
		return windowDecisionR493{}, false
	}
	return decideWindowR493(in), true
}

// logWindowDecisionR493 — одна INFO-строка на решение: вид, числа, цель, причина.
//
// Без неё живой разбор «почему клиент получил 503 на :18079 и 200 на :18080»
// снова потребовал бы чтения исходников: до этой правки в логе не было вида
// вердикта, только набор чисел у отдельных путей.
// «Ничего не делаем» пишется на Debug ОСОЗНАННО: это большинство запросов (окно
// достаточно), а INFO на каждый запрос вытесняет из кольца логов строки request
// trace — ровно та живая жалоба, из-за которой появился R488.
func logWindowDecisionR493(where, backendID, model, path string, d windowDecisionR493) {
	log := logger.Get().Infow
	if d.Kind == windowNoopR493 {
		log = logger.Get().Debugw
	}
	log("window decision (R493-12n)",
		"where", where, "backend", backendID, "model", model, "path", path,
		"kind", string(d.Kind), "needed_tokens", d.Needed, "loaded_window_tokens", d.Loaded,
		"target_per_client", d.Target, "retry_after_sec", d.RetryAfterSec,
		"estimated_prompt_tokens", d.Estimated, "prompt_fits", d.PromptFits,
		"capped_to_ceiling", d.Capped, "reason", d.Why)
}

// logWindowDivergenceR493 — расхождение «тени»: что решил бы единый путь и что
// делает исторический.
//
// Формат назван так, чтобы его можно было грепать одной строкой на стенде:
// `R493-12n shadow divergence`. Молчаливое расхождение здесь было бы худшим
// исходом: это ровно тот класс, из которого выросли «два мозга».
func logWindowDivergenceR493(where, backendID, model, path string, legacy windowKindR493, d windowDecisionR493) {
	logger.Get().Warnw("R493-12n shadow divergence: единое решение расходится с историческим",
		"where", where, "backend", backendID, "model", model, "path", path,
		"legacy", string(legacy), "unified", string(d.Kind),
		"needed_tokens", d.Needed, "loaded_window_tokens", d.Loaded,
		"target_per_client", d.Target, "reason", d.Why)
}
