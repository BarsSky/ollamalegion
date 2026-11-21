package balancer

// window_truth_r294_test.go — R294 (2026-10-29): окно не уменьшается, бюджет
// вывода осмыслен, кадр ошибки воркера закрывает стрим.
//
// ЖИВЫЕ ДЕФЕКТЫ (стенд r294), которые эти тесты фиксируют:
//   - клиент просил num_ctx=16384 при загруженных 8192, а балансер
//     перезагружал модель в 4165 (промпт маленький, бюджет посчитан нулём);
//   - при num_predict=8 бюджет уходил в 8/0 вместо осмысленного минимума;
//   - /v1 зависал: кадр ошибки от воркера приходил, а терминатора не было.

import (
	"testing"

	"ollama-loadbalancer/pkg/types"
)

// TestR294_RequestedWindowIsLowerBound — выведенное окно НИКОГДА не меньше
// запрошенного клиентом, даже если промпт короткий.
func TestR294_RequestedWindowIsLowerBound(t *testing.T) {
	meta := &RequestMeta{
		ModelName:             "qwen",
		EstimatedPromptTokens: 100,   // короткий промпт
		RequestedNPredict:     8,     // маленький бюджет от клиента
		RequestedNCtxOverride: 16384, // ЯВНО просит больше загруженного
	}
	got := agentRequiredWindowAtLeast(meta, 0)
	if got < 16384 {
		t.Fatalf("derived window = %d, want >= 16384 (запрошенное клиентом окно)", got)
	}
	if got > 16384 {
		t.Fatalf("derived window = %d, want 16384: больше запрошенного не выдумываем", got)
	}
}

// TestR294_LoadedWindowIsFloor — перезагрузка не может быть уменьшением окна:
// «пол» (загруженное окно слота) — нижняя граница.
func TestR294_LoadedWindowIsFloor(t *testing.T) {
	meta := &RequestMeta{
		ModelName:             "qwen",
		EstimatedPromptTokens: 100,
		RequestedNPredict:     8,
	}
	if got := agentRequiredWindowAtLeast(meta, 8192); got < 8192 {
		t.Fatalf("derived window = %d, want >= 8192 (загруженное окно как пол)", got)
	}
	// и без запроса клиента, но с загруженным окном — тоже не меньше пола
	if got := agentRequiredWindowAtLeast(meta, 32768); got != 32768 {
		t.Fatalf("derived window = %d, want 32768", got)
	}
}

// TestR294_OutputBudgetNeverZero — при маленьком num_predict бюджет не ноль и не
// равен 8: иначе окно считалось «промпт + 0».
func TestR294_OutputBudgetNeverZero(t *testing.T) {
	cases := []struct {
		name string
		np   int
		want int
	}{
		{"ноль от клиента", 0, agentOutputReserveTokens()},
		{"маленький num_predict", 8, agentOutputReserveMinTokens},
		{"осмысленный бюджет", 2048, 2048},
		{"огромный", agentOutputReserveMaxTokens + 1000, agentOutputReserveMaxTokens},
	}
	for _, c := range cases {
		meta := &RequestMeta{RequestedNPredict: c.np}
		if got := agentOutputBudget(meta); got != c.want {
			t.Errorf("%s: agentOutputBudget(%d) = %d, want %d", c.name, c.np, got, c.want)
		}
	}
	if got := agentOutputBudget(nil); got != agentOutputReserveTokens() {
		t.Errorf("nil meta: %d, want %d", got, agentOutputReserveTokens())
	}
}

// TestR294_ContextGrowthBlocksOnRequestedWindow — блокировка (ожидание роста)
// остаётся ТОЛЬКО там, где без роста запрос физически не обслужить.
//
// ЖИВОЕ ПРАВИЛО R294 НЕ МЕНЯЛОСЬ: окно никогда не уменьшается (пол — загруженное
// окно слота), см. TestR294_LoadedWindowIsFloor и
// TestR294_RequestedWindowIsLowerBound. Меняется ДРУГОЕ — цена заявки клиента.
//
// До R664 (шаг A) «клиент просит окно больше загруженного» при чужом стриме
// означало отказ до старта потока. R664 + R447: если данные влезают в текущее
// окно, запрос обслуживается СРАЗУ, а окно растёт в фоне до заявленного
// (growthBackgroundR664 → R385) — держать его в очереди 900 с не за что.
// Ожидание (блокировка) оправдано только когда данные НЕ влезают: тогда без
// роста воркер ответил бы кадром ошибки, и запрос должен ждать освобождения
// бэкенда, как и раньше (R288/R299/R301).
func TestR294_ContextGrowthBlocksOnRequestedWindow(t *testing.T) {
	p := &Proxy{
		metricsMgr: NewMetricsManager(),
		config:     &types.LoadBalancerConfig{},
	}
	const backendID, model = "bk-1", "qwen"
	p.metricsMgr.mu.Lock()
	p.metricsMgr.llamaMetrics[backendID] = &types.LlamaCppMetrics{
		LoadedModels: []types.LlamaCppModel{{Name: model, ContextLength: 8192}},
	}
	p.metricsMgr.mu.Unlock()

	// Без активных стримов — не вмешиваемся: preflight может вырасти.
	meta := &RequestMeta{ModelName: model, EstimatedPromptTokens: 20, RequestedNCtxOverride: 16384}
	if blocked, _, _ := p.contextGrowthBlocker(backendID, model, meta); blocked {
		t.Fatal("без активных стримов блокировать нельзя: рост окна возможен")
	}

	stop := beginClientStreamR271(p, backendID, model)
	defer stop()

	// R664: данные влезают (20 токенов при окне 8192) — не блокируем: обслуживаем
	// сразу, окно поднимаем в фоне. Числа для лога/отказа остаются прежними.
	blocked, needed, loaded := p.contextGrowthBlocker(backendID, model, meta)
	if blocked {
		t.Fatal("R664: активный стрим + влезающие данные — запрос обслуживается сразу, блокировки нет")
	}
	if needed != 16384 {
		t.Errorf("needed = %d, want 16384 (запрошенное клиентом окно)", needed)
	}
	if loaded != 8192 {
		t.Errorf("loaded = %d, want 8192", loaded)
	}

	// А вот данные, которые НЕ влезают, по-прежнему ждут: без роста обслужить
	// физически нельзя (воркер ответит кадром ошибки — R301), и занимать слот
	// под заведомо невыполнимый запрос нельзя (R288/R299).
	big := &RequestMeta{ModelName: model, EstimatedPromptTokens: 20000, RequestedNCtxOverride: 0}
	blocked, needed, loaded = p.contextGrowthBlocker(backendID, model, big)
	if !blocked {
		t.Fatal("данные не влезают и рост отложен стримом: ожидалась блокировка до старта потока")
	}
	if needed <= loaded {
		t.Errorf("needed = %d, loaded = %d: без роста обслужить нельзя, число обязано быть больше окна", needed, loaded)
	}
	if loaded != 8192 {
		t.Errorf("loaded = %d, want 8192", loaded)
	}
}

// TestR294_ContextGrowthIgnoresShortPromptWithoutRequest — прежнее поведение не
// сломано: короткий промпт без явного окна и без стримов не блокируется.
func TestR294_ContextGrowthIgnoresShortPromptWithoutRequest(t *testing.T) {
	p := &Proxy{metricsMgr: NewMetricsManager(), config: &types.LoadBalancerConfig{}}
	const backendID, model = "bk-2", "qwen"
	p.metricsMgr.mu.Lock()
	p.metricsMgr.llamaMetrics[backendID] = &types.LlamaCppMetrics{
		LoadedModels: []types.LlamaCppModel{{Name: model, ContextLength: 8192}},
	}
	p.metricsMgr.mu.Unlock()

	stop := beginClientStreamR271(p, backendID, model)
	defer stop()
	meta := &RequestMeta{ModelName: model, EstimatedPromptTokens: 20}
	if blocked, _, _ := p.contextGrowthBlocker(backendID, model, meta); blocked {
		t.Fatal("короткий промпт без явного окна блокировать нельзя")
	}
}

// TestR294_UpstreamErrorPayload — кадр ошибки распознаётся только по
// верхнеуровневому "error", а не по слову в тексте ответа.
func TestR294_UpstreamErrorPayload(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		want    bool
	}{
		{"объект ошибки", `{"error":{"message":"bridge_code 2: requested n_ctx=16384 exceeds"}}`, true},
		{"строка ошибки", `{"error":"model not loaded"}`, true},
		{"null", `{"error":null}`, false},
		{"пустая строка", `{"error":""}`, false},
		{"обычный кадр", `{"choices":[{"delta":{"content":"the error is in line 12"}}]}`, false},
		{"не json", `plain text with error`, false},
		{"пусто", ``, false},
	}
	for _, c := range cases {
		if got := isUpstreamErrorPayload([]byte(c.payload)); got != c.want {
			t.Errorf("%s: isUpstreamErrorPayload = %v, want %v", c.name, got, c.want)
		}
	}
}
