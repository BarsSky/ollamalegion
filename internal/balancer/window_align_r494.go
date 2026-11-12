// window_align_r494.go — R494 (2026-11-12, решение оператора по архиву стенда
// A10 ×2): ПРЕДЛОЖЕНИЕ ВЫРАВНЯТЬ ОКНА КОПИЙ ОДНОЙ МОДЕЛИ.
//
// ЖИВОЙ ФАКТ ИЗ АРХИВА. Один и тот же файл Qwen3.8-27B был поднят на мастере с
// окном 198656 и на A10-126 с окном 131072. Последствия видны в логах целиком:
//
//	session re-stick: привязанная копия не покрывает окно клиента
//	R459: кандидат на ребалансировку отклонён (окно 131072 < 198656)
//	no available backends with exclusions
//
// То есть вторая копия существовала, была свободна, но для живой сессии
// бесполезна: её окно меньше. Оператор попросил «уведомление с возможностью
// подтвердить увеличение окна» — здесь считается, что именно предлагать.
//
// ПРАВИЛО ОПЕРАТОРА, дословно: «если копия не занята сессией, то принимать
// решение по выравниванию; если же занята, а есть бэкенд с нужным окном — ставить
// в очередь к нему на обработку». Поэтому:
//   - предлагаем выравнивать ТОЛЬКО свободные копии (0 сессий, 0 активных
//     запросов): у занятой копии перезагрузка выбила бы чужую генерацию, и это
//     уже не «выравнивание», а обрыв (правило R287 «отложить, а не ломать»);
//   - целимся в БОЛЬШЕЕ окно среди копий, но не выше потолка оператора
//     (LB_WINDOW_ALIGN_MAX_N_CTX): «поднять до 198k» на 8-гигабайтном узле —
//     заведомо невыполнимое предложение.
//
// ЧЕГО ЗДЕСЬ СОЗНАТЕЛЬНО НЕТ. Автоматической перезагрузки. 27B грузится
// минутами; молчаливый reload по расписанию — это ровно та «умная»
// самодеятельность, из-за которой стенд уже вставал (R287/R299). Балансер
// отдаёт ЧИСЛА и цель, а подтверждает оператор (WebUI «Монитор» → кнопка
// «Выровнять»).
package balancer

import (
	"sort"

	"ollama-loadbalancer/internal/settings"
	"ollama-loadbalancer/pkg/types"
)

// defaultWindowAlignCapR494 — потолок выравнивания, если реестр настроек не
// поднят (юнит-тесты, лёгкие сборки). Дефолт реестра — то же число
// (internal/settings/builtin_r278.go).
const defaultWindowAlignCapR494 = 262144

// WindowAlignCopyR494 — одна копия модели на одном узле, как её видит балансер.
type WindowAlignCopyR494 struct {
	BackendID string `json:"backendId"`
	// Window — окно, доступное ОДНОЙ сессии (per-slot), а не суммарное окно
	// модели: именно его упрётся prompt, и именно его сравнивает R459.
	Window int `json:"window"`
	// Sessions — сколько живых сессий привязано к этому узлу.
	Sessions int `json:"sessions"`
	// Active — активные запросы прямо сейчас (слоты заняты).
	Active int `json:"active"`
	// Free — копию можно перезагрузить, не оборвав чужую работу.
	Free bool `json:"free"`
}

// WindowAlignSuggestionR494 — «эти копии уже, чем самая широкая; выровнять до
// targetWindow можно на alignableBackends».
type WindowAlignSuggestionR494 struct {
	Model        string                `json:"model"`
	Cap          int                   `json:"cap"`
	TargetWindow int                   `json:"targetWindow"`
	Copies       []WindowAlignCopyR494 `json:"copies"`
	// Alignable — узлы со свободной узкой копией: их и предлагаем поднять.
	Alignable []string `json:"alignableBackends"`
	// Busy — узкие копии, занятые работой: их НЕ предлагаем (правило оператора).
	Busy []string `json:"busyBackends,omitempty"`
}

// windowAlignDecideR494 — чистое решение: какие копии и до какого окна
// выравнивать. Отдельная функция (без Proxy и метрик), чтобы правило можно было
// проверить таблицей, не поднимая кластер.
//
// Возвращает (решение, true), если предложение имеет смысл.
func windowAlignDecideR494(model string, copies []WindowAlignCopyR494, cap int) (WindowAlignSuggestionR494, bool) {
	if model == "" || len(copies) < 2 {
		return WindowAlignSuggestionR494{}, false
	}
	// Потолок: 0 (или отрицательный) — предложение выключено оператором.
	// Это осознанное «выключено», а не «без потолка»: без ограничения
	// предложение упиралось бы в физический предел GGUF и выглядело как
	// «поднимите 27B до 262144 на узле с 8 ГБ».
	if cap <= 0 {
		return WindowAlignSuggestionR494{}, false
	}
	wide := 0
	withWindow := make([]WindowAlignCopyR494, 0, len(copies))
	for _, c := range copies {
		if c.Window <= 0 {
			// «Окно неизвестно» — не «узкая копия»: предлагать выравнивание по
			// незнакомому числу нельзя, предложение соврёт.
			continue
		}
		withWindow = append(withWindow, c)
		if c.Window > wide {
			wide = c.Window
		}
	}
	if len(withWindow) < 2 || wide <= 0 {
		return WindowAlignSuggestionR494{}, false
	}
	target := wide
	if target > cap {
		target = cap
	}
	sugg := WindowAlignSuggestionR494{
		Model:        model,
		TargetWindow: target,
		Cap:          cap,
		Copies:       make([]WindowAlignCopyR494, 0, len(withWindow)),
	}
	sort.Slice(withWindow, func(i, j int) bool { return withWindow[i].BackendID < withWindow[j].BackendID })
	for _, c := range withWindow {
		if c.Window >= target {
			continue
		}
		if c.Free {
			sugg.Alignable = append(sugg.Alignable, c.BackendID)
		} else {
			sugg.Busy = append(sugg.Busy, c.BackendID)
		}
	}
	sugg.Copies = withWindow
	if len(sugg.Alignable) == 0 {
		// Все узкие копии заняты: «выровнять» означало бы оборвать чужую работу.
		// Отдаём false — оператор увидит занятость в мониторе, но кнопки не будет.
		return WindowAlignSuggestionR494{}, false
	}
	return sugg, true
}

// windowAlignCapR494 — потолок выравнивания из реестра горячих настроек.
func windowAlignCapR494() int {
	if v, ok := settings.HotInt("LB_WINDOW_ALIGN_MAX_N_CTX"); ok {
		return int(v)
	}
	return defaultWindowAlignCapR494
}

// WindowAlignCapR494 — потолок выравнивания для чтения снаружи (API отдаёт его
// вместе с предложением, чтобы оператору было видно, чем ограничено число).
func WindowAlignCapR494() int { return windowAlignCapR494() }

// WindowAlignSuggestionsR494 — предложения по всем моделям (или одной, если
// onlyModel != ""). Читает ТОЛЬКО снимки: ни перезагрузок, ни долгих блокировок —
// метод вызывается HTTP-ручкой монитора.
func (p *Proxy) WindowAlignSuggestionsR494(onlyModel string) []WindowAlignSuggestionR494 {
	if p == nil || p.metricsMgr == nil {
		return nil
	}
	out := make([]WindowAlignSuggestionR494, 0, 2)
	cap := windowAlignCapR494()
	if cap <= 0 {
		return out
	}

	// Сессии по узлам — один снимок на весь вызов: считать их внутри цикла по
	// каждой модели значило бы читать состояние в разное время и получать
	// несогласованные числа.
	sessPerBackend := make(map[string]int, 8)
	if p.sessionMgr != nil {
		for _, s := range p.sessionMgr.GetAllSnapshots() {
			if s.BackendID == "" {
				continue
			}
			sessPerBackend[s.BackendID]++
		}
	}

	// Копии по имени модели: имя из метрик нормализуем, чтобы .gguf/путь/регистр
	// не делили одну модель на две (та же нормализация, что в modelNameMatches).
	byModel := make(map[string][]WindowAlignCopyR494, 8)
	rawName := make(map[string]string, 8)
	order := make([]string, 0, 8)

	p.metricsMgr.mu.RLock()
	ids := make([]string, 0, len(p.metricsMgr.llamaMetrics))
	for id := range p.metricsMgr.llamaMetrics {
		ids = append(ids, id)
	}
	p.metricsMgr.mu.RUnlock()
	sort.Strings(ids)

	for _, id := range ids {
		lm := p.llamaMetricsSnapshotR494(id)
		if lm == nil {
			continue
		}
		for _, m := range lm.LoadedModels {
			if m.Name == "" || m.ContextLength <= 0 {
				continue
			}
			if onlyModel != "" && !modelNameMatches(m.Name, onlyModel) {
				continue
			}
			key := normalizeModelName(m.Name)
			win := p.loadedPerSeqNCtx(id, m.Name)
			if win <= 0 {
				continue
			}
			if _, seen := byModel[key]; !seen {
				order = append(order, key)
				rawName[key] = m.Name
			}
			sess := sessPerBackend[id]
			active := p.backendActiveReqsR494(id)
			byModel[key] = append(byModel[key], WindowAlignCopyR494{
				BackendID: id,
				Window:    win,
				Sessions:  sess,
				Active:    active,
				// Свободна — только когда никто на ней не работает: иначе
				// перезагрузка = обрыв (R287: «отложить, а не ломать»).
				Free: sess == 0 && active == 0,
			})
		}
	}

	for _, key := range order {
		if s, ok := windowAlignDecideR494(rawName[key], byModel[key], cap); ok {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Model < out[j].Model })
	return out
}

// backendActiveReqsR494 — активные запросы узла под его замком (0, если узел
// уже удалён из состояния).
func (p *Proxy) backendActiveReqsR494(backendID string) int {
	p.mu.RLock()
	st := p.backends[backendID]
	p.mu.RUnlock()
	if st == nil {
		return 0
	}
	st.mu.Lock()
	active := st.ActiveReqs
	st.mu.Unlock()
	return active
}

// llamaMetricsSnapshotR494 — копия метрик бэкенда под RLock (наружу отдаём
// значение, а не живой указатель: см. правило «не сериализуй живое»).
func (p *Proxy) llamaMetricsSnapshotR494(backendID string) *types.LlamaCppMetrics {
	if p == nil || p.metricsMgr == nil {
		return nil
	}
	p.metricsMgr.mu.RLock()
	defer p.metricsMgr.mu.RUnlock()
	lm := p.metricsMgr.llamaMetrics[backendID]
	if lm == nil {
		return nil
	}
	cp := *lm
	return &cp
}
