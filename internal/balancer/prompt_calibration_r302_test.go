// prompt_calibration_r302_test.go — R302 (2026-10-29): калибровка оценки
// промпта по фактическим токенам воркера.
//
// ЖИВОЙ ДЕФЕКТ, который здесь зафиксирован: на русском диалоге балансер
// оценивал промпт в 66001 токен, воркер считал 17616 (prompt_eval_count), окно
// было 32768 — и запрос, который физически помещался, получал отказ. Тесты
// ниже проверяют не «формулу», а исход: с калибровкой тот же запрос проходит.
package balancer

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"ollama-loadbalancer/pkg/types"
)

// newPromptCalibProxyR302 — прокси с загруженным окном loaded для модели "m" и
// заданным хранилищем калибровки.
func newPromptCalibProxyR302(loaded int, calib *promptCalibration) *Proxy {
	p := &Proxy{
		backends: map[string]*BackendState{
			"bk-1": {Backend: &types.Backend{ID: "bk-1", Host: "127.0.0.1", Status: types.StatusHealthy}},
		},
		metricsMgr:    NewMetricsManager(),
		config:        &types.LoadBalancerConfig{},
		nctxReload:    NewNCtxReloadCoordinator(NCtxReloadConfig{PreflightEnabled: true}),
		tokensByModel: make(map[string]*ModelTokenUsage),
		promptCalib:   calib,
	}
	p.metricsMgr.mu.Lock()
	p.metricsMgr.llamaMetrics["bk-1"] = &types.LlamaCppMetrics{
		LoadedModels: []types.LlamaCppModel{{Name: "m", ContextLength: loaded, State: "loaded"}},
	}
	p.metricsMgr.mu.Unlock()
	p.nctxReload.SetLastKnownNCtx("bk-1", loaded)
	return p
}

// r302RussianBody — тело /api/chat с русским промптом. pkg/tokencount считает
// каждую не-ASCII руну и каждый пробел за токен, поэтому raw-оценка ≈ 28*repeats.
func r302RussianBody(repeats int) []byte {
	const phrase = "подробности предыдущего шага "
	return []byte(`{"model":"m","messages":[{"role":"user","content":"` +
		strings.Repeat(phrase, repeats) + `"}],"stream":true}`)
}

// (а) Оценка учится на факте: после двух фактических замеров отношение
// actual/estimated применяется к следующей оценке.
func TestR302_EstimateLearnsFromActual(t *testing.T) {
	p := newPromptCalibProxyR302(32768, newPromptCalibration(""))
	body := r302RussianBody(1000)

	meta := p.extractRequestMeta(body, "/api/chat")
	if meta == nil {
		t.Fatal("extractRequestMeta не разобрал тело")
	}
	raw := meta.EstimatedPromptTokensRaw
	if raw < 20000 {
		t.Fatalf("raw-оценка слишком мала для проверки: %d", raw)
	}
	// До калибровки работает запасной коэффициент: текст почти целиком
	// не-ASCII, значит оценка = raw/2.5.
	if want := int(math.Round(float64(raw) / promptCalibNonASCIIDivisor)); meta.EstimatedPromptTokens != want {
		t.Fatalf("запасной коэффициент: got %d, want %d (raw=%d)", meta.EstimatedPromptTokens, want, raw)
	}

	// Факт воркера: 17616/66001 = 0.2669 — ровно живое отношение стенда.
	ratio := 17616.0 / 66001.0
	actual := int(math.Round(float64(raw) * ratio))
	for i := 0; i < 2; i++ {
		// Свежая raw-оценка перед каждым фактом: именно она связывается с actual.
		p.extractRequestMeta(body, "/api/chat")
		p.recordTokenUsage("m", int64(actual), 0)
	}

	final := p.extractRequestMeta(body, "/api/chat")
	got := float64(final.EstimatedPromptTokens) / float64(raw)
	if math.Abs(got-ratio) > 0.02 {
		t.Fatalf("калиброванная оценка = %.3f*raw, want %.3f*raw (raw=%d est=%d actual=%d)",
			got, ratio, raw, final.EstimatedPromptTokens, actual)
	}

	snap := p.PromptCalibrationSnapshot()
	if len(snap) != 1 {
		t.Fatalf("снимок калибровки: got %d моделей, want 1", len(snap))
	}
	if snap[0].Samples != promptCalibMinSamples {
		t.Fatalf("samples = %d, want %d", snap[0].Samples, promptCalibMinSamples)
	}
	if math.Abs(snap[0].Ratio-ratio) > 0.02 {
		t.Fatalf("ratio в снимке = %.4f, want ≈%.4f", snap[0].Ratio, ratio)
	}
}

// (б) Калибровка переживает перезапуск: тот же путь файла → то же отношение.
func TestR302_CalibrationSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), promptCalibFileName)
	now := time.Now()

	c1 := newPromptCalibration(path)
	for i := 0; i < 2; i++ {
		c1.note("m", 66001, now)
		c1.observe("m", 17616, now)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("файл калибровки не создан: %v", err)
	}

	c2 := newPromptCalibration(path)
	got, source := c2.calibrate("m", 66001, 0)
	if source != "calibrated" {
		t.Fatalf("источник оценки после перезапуска = %q, want calibrated", source)
	}
	want := int(math.Round(66001 * 17616.0 / 66001.0))
	if got < want-100 || got > want+100 {
		t.Fatalf("после перезапуска оценка = %d, want ≈%d", got, want)
	}
	if ratio, samples, ok := c2.observe("m", 1, now); ok {
		t.Fatalf("ожиданий после перезапуска быть не должно, а observe вернул ratio=%.3f samples=%d", ratio, samples)
	}
}

// (в) Пока калибровки нет: не-ASCII текст дешевеет не более чем в 2.5 раза,
// латиница не меняется вовсе.
func TestR302_NonASCIIFallback(t *testing.T) {
	c := newPromptCalibration("")

	if got, source := c.calibrate("m", 1000, 0.9); source != "fallback_nonascii" || got != 400 {
		t.Fatalf("не-ASCII текст: got (%d, %q), want (400, fallback_nonascii)", got, source)
	}
	if got, source := c.calibrate("m", 1000, 0.5); source != "raw" || got != 1000 {
		t.Fatalf("доля 0.5 не должна включать запасной коэффициент: got (%d, %q)", got, source)
	}
	if got, source := c.calibrate("m", 1000, 0.0); source != "raw" || got != 1000 {
		t.Fatalf("латиница: got (%d, %q), want (1000, raw)", got, source)
	}

	// Та же картина на реальном теле запроса: считаем долю из сообщения.
	rus := r302RussianBody(10)
	metaRus := ExtractRequestMeta(rus, "/api/chat")
	if metaRus.PromptNonASCIIShare < 0.5 {
		t.Fatalf("доля не-ASCII в русском теле = %.3f, want >= 0.5", metaRus.PromptNonASCIIShare)
	}
	p := newPromptCalibProxyR302(32768, c)
	if got, want := p.extractRequestMeta(rus, "/api/chat").EstimatedPromptTokens,
		int(math.Round(float64(metaRus.EstimatedPromptTokens)/promptCalibNonASCIIDivisor)); got != want {
		t.Fatalf("оценка русского тела = %d, want %d", got, want)
	}

	lat := []byte(`{"model":"m","messages":[{"role":"user","content":"` +
		strings.Repeat("hello world ", 100) + `"}]}`)
	metaLat := p.extractRequestMeta(lat, "/api/chat")
	if metaLat.PromptNonASCIIShare != 0 {
		t.Fatalf("доля не-ASCII в латинском теле = %.3f, want 0", metaLat.PromptNonASCIIShare)
	}
	if metaLat.EstimatedPromptTokens != metaLat.EstimatedPromptTokensRaw {
		t.Fatalf("латиница изменилась без калибровки: %d -> %d",
			metaLat.EstimatedPromptTokensRaw, metaLat.EstimatedPromptTokens)
	}
}

// (г) Пол отношения: 0.9 от лучшего наблюдения. И — важно — короткий промпт
// после длинного не раздувается (иначе пол в абсолютных токенах дал бы ложный
// отказ короткому запросу).
func TestR302_RatioFloor(t *testing.T) {
	c := newPromptCalibration("")
	now := time.Now()
	for i := 0; i < 2; i++ {
		c.note("m", 66001, now)
		c.observe("m", 17616, now)
	}
	// Один аномальный замер на коротком промпте тянет EMA вниз.
	c.note("m", 100, now)
	c.observe("m", 5, now)

	maxRatio := 17616.0 / 66001.0
	floor := promptCalibFloorFraction * maxRatio

	got, source := c.calibrate("m", 66001, 0)
	if source != "calibrated" {
		t.Fatalf("источник = %q, want calibrated", source)
	}
	want := int(math.Round(66001 * floor))
	if got != want {
		t.Fatalf("оценка = %d, want %d (пол = %.4f от raw)", got, want, floor)
	}
	if got < int(math.Round(66001*17616.0/66001.0*0.8)) {
		t.Fatalf("оценка %d упала ниже 80%% от наблюдённого отношения — пол не работает", got)
	}

	// Короткий промпт не должен получить оценку, раздутую прошлым максимумом:
	// при 17616 фактических токенов абсолютный пол дал бы >= 15854.
	short, _ := c.calibrate("m", 20, 0)
	if short > 20 {
		t.Fatalf("короткий промпт раздут калибровкой: %d > 20 (raw)", short)
	}
}

// (д) ЖИВОЙ СЦЕНАРИЙ: raw 66001, факт 17616, окно 32768. До калибровки guard
// отказывал (дефект), после — запрос проходит.
func TestR302_AcceptanceNoFalseRefusalOnRussian(t *testing.T) {
	c := newPromptCalibration("")
	p := newPromptCalibProxyR302(32768, c)
	body := r302RussianBody(2357)

	meta := p.extractRequestMeta(body, "/api/chat")
	raw := meta.EstimatedPromptTokensRaw
	if raw < 60000 || raw > 72000 {
		t.Fatalf("raw-оценка = %d, want ≈66001: тело не воспроизводит живой случай", raw)
	}
	if raw <= 32768 {
		t.Fatalf("raw = %d должно превышать окно 32768, иначе дефект не воспроизводится", raw)
	}

	// ДО R302 (калибровки нет вообще): сырой heuristic даёт 66001 при окне
	// 32768 — ровно та картина, в которой балансер отказывал живому запросу.
	rawOnly := newPromptCalibProxyR302(32768, nil)
	if v := rawOnly.growthVerdictR301("bk-1", "m", body, "/api/chat", true); !v.Defer {
		t.Fatalf("до R302 отказа нет: estimated=%d loaded=%d — сценарий потерян", v.Estimated, v.Loaded)
	}

	// Запасного коэффициента одного уже достаточно, чтобы ЭТОТ отказ не
	// случился (66001/2.5 = 26400 < 32768), но число всё ещё завышено в 1.5
	// раза против факта. Калибровка нужна именно чтобы решение опиралось на
	// фактический размер промпта, а не на «повезло с окном».
	fallbackOnly := newPromptCalibProxyR302(32768, newPromptCalibration(""))
	if est := fallbackOnly.extractRequestMeta(body, "/api/chat").EstimatedPromptTokens; est < 25000 || est > 28000 {
		t.Fatalf("запасной коэффициент дал %d, want ≈26400 (raw/2.5)", est)
	}

	// Факт воркера: 17616 токенов промпта. Два замера — порог доверия.
	for i := 0; i < 2; i++ {
		p.extractRequestMeta(body, "/api/chat")
		p.recordTokenUsage("m", 17616, 0)
	}

	v := p.growthVerdictR301("bk-1", "m", body, "/api/chat", true)
	if v.Loaded != 32768 {
		t.Fatalf("загруженное окно в вердикте = %d, want 32768", v.Loaded)
	}
	if v.Defer {
		t.Fatalf("ЛОЖНЫЙ ОТКАЗ: estimated=%d raw=%d loaded=%d — запрос с фактическими 17616 не влезает по оценке",
			v.Estimated, raw, v.Loaded)
	}
	if v.Estimated < 16000 || v.Estimated > 19500 {
		t.Fatalf("калиброванная оценка = %d, want ≈17616", v.Estimated)
	}

	// Нестриминговый путь считает те же числа тем же входом.
	deferNow, est, loaded, needed, _, _ := p.growthDeferR299("bk-1", "m", body, "/api/chat")
	if deferNow {
		t.Fatalf("growthDeferR299 отказал без активных стримов: est=%d loaded=%d needed=%d", est, loaded, needed)
	}
	if est != v.Estimated || loaded != v.Loaded {
		t.Fatalf("два opinion об одном числе: verdict=(%d,%d) deferred=(%d,%d)",
			v.Estimated, v.Loaded, est, loaded)
	}
}

// (з) Имя модели в запросе и в учёте usage может отличаться (алиас против
// загруженного имени). Факт привязывается к единственному свежему ожиданию и
// учит ТОТ ключ, по которому потом читает calibrate; если ожиданий несколько —
// привязки нет (лучше не учиться, чем учиться не на той модели).
func TestR302_LearnByUniquePendingWhenModelNameDiffers(t *testing.T) {
	c := newPromptCalibration("")
	p := newPromptCalibProxyR302(32768, c)
	body := r302RussianBody(100)

	meta := p.extractRequestMeta(body, "/api/chat") // model "m"
	raw := meta.EstimatedPromptTokensRaw
	for i := 0; i < 2; i++ {
		p.extractRequestMeta(body, "/api/chat")
		// usage приходит под другим именем: алиас против загруженного имени.
		p.recordTokenUsage("m-loaded", int64(raw/2), 0)
	}
	if _, source := c.calibrate("m", raw, 0); source != "calibrated" {
		t.Fatalf("факт по чужому имени модели не выучил ключ запроса: source=%q", source)
	}

	// Два ожидания одновременно — привязка была бы догадкой, поэтому её нет.
	now := time.Now()
	c2 := newPromptCalibration("")
	c2.note("a", 1000, now)
	c2.note("b", 1000, now)
	if _, _, ok := c2.observe("c", 500, now); ok {
		t.Fatal("при двух ожиданиях факт нельзя приписывать одной модели")
	}
}

// (ж) Конкурентность: оценка читается в каждом запросе, факт пишется из
// горутин ответа — под -race это должно быть чисто (мьютекс калибровки
// листовой, p.mu под ним не берётся).
func TestR302_ConcurrentLearnNoRace(t *testing.T) {
	p := newPromptCalibProxyR302(32768, newPromptCalibration(""))
	body := r302RussianBody(200)

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Пара «оценка → факт» в одной горутине: к моменту observe
			// соответствующая note уже сделана, поэтому каждый замер учтён.
			// (Две независимые горутины могли бы прогнать все observe раньше
			// всех note, и замеров не осталось бы вовсе.)
			_ = p.extractRequestMeta(body, "/api/chat").EstimatedPromptTokens
			p.recordTokenUsage("m", 5000, 10)
		}()
	}
	wg.Wait()

	if snap := p.PromptCalibrationSnapshot(); len(snap) == 0 || snap[0].Samples == 0 {
		t.Fatal("ни одного замера после конкурентных запросов — факты теряются")
	}
}

// (е) Стерегущий тест: ни один боевой путь не читает оценку промпта в обход
// калибровки. Разрешены только определение (preflight_nctx.go) и сам
// калибрующий вход (prompt_calibration_r302.go) — иначе снова появятся «две
// правды» об одном числе (дефект R301.3).
func TestR302_NoPathBypassesCalibration(t *testing.T) {
	allowed := map[string]bool{
		"preflight_nctx.go":          true, // определение ExtractRequestMeta
		"prompt_calibration_r302.go": true, // обёртка extractRequestMeta/calibratedEstimateOnly
	}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	var offenders []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		if allowed[name] {
			continue
		}
		b, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("ReadFile %s: %v", name, err)
		}
		for i, line := range strings.Split(string(b), "\n") {
			if strings.Contains(line, "ExtractRequestMeta(") {
				offenders = append(offenders, name+":"+itoaSmall(i+1))
			}
		}
	}
	if len(offenders) > 0 {
		t.Fatalf("оценка промпта читается в обход калибровки: %v\n"+
			"используйте p.extractRequestMeta (или p.calibratedEstimateOnly для текста без обучения)", offenders)
	}
}

// itoaSmall — маленький локальный int→string без импорта strconv в тесте
// (strconv здесь не нужен ни для чего другого).
func itoaSmall(v int) string {
	if v == 0 {
		return "0"
	}
	var buf [8]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[i:])
}
