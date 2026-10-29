// growth_stream_guard_r301_test.go — R301 (2026-10-29): стриминговый запрос,
// которому нужно больше окна, чем загружено, больше не уезжает в воркер.
//
// ЖИВОЙ ДЕФЕКТ (стенд 573a03a3, нативный POST /api/chat на :18080, gemma
// загружена 8192 на слот, клиент прислал options.num_ctx=16384):
//
//	HTTP 200 за 0.12 с
//	{"done":false}
//	{"bridge_code":2,"bridge_message":"... requested n_ctx=16384 exceeds model's
//	 effective n_ctx=8192 ..."}
//
// Ни одного числа, ни причины, ни терминального done:true. Эти проверки
// фиксируют три исхода вердикта и то, что обходного пути не осталось.
package balancer

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// r301StreamBody — живое тело клиента: стриминг, окно «на всякий случай»
// больше загруженного, промпт крошечный.
const r301StreamBody = `{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":true,"options":{"num_ctx":16384,"num_predict":8}}`

// r301LongStreamBody — тот же стриминг, но промпт физически не влезает в 8192:
// окно нужно вырастить, а стриминговый путь этого не делает.
func r301LongStreamBody() string {
	long := strings.Repeat("подробности предыдущего шага ", 4000) // ~110k символов
	return `{"model":"m","messages":[{"role":"user","content":"` + long + `"}],"stream":true}`
}

// TestR301_StreamingServedWithLoadedWindow — окно растить НЕ нужно: промпт
// влезает, клиент просто попросил больше. Обслуживаем, приведя num_ctx к окну
// слота (та же логика, что у политики primary, R83).
func TestR301_StreamingServedWithLoadedWindow(t *testing.T) {
	p := newGrowthGuardProxyR299()

	v := p.growthVerdictR301("bk-1", "m", []byte(r301StreamBody), "/api/chat", true)
	if v.Defer {
		t.Fatalf("промпт влезает в окно: отказ недопустим (needed=%d loaded=%d)", v.Needed, v.Loaded)
	}
	if !v.Served {
		t.Fatal("ожидалось «обслужить с загруженным окном», а не пропуск запроса как есть")
	}
	if !strings.Contains(string(v.Body), `"num_ctx":8192`) {
		t.Fatalf("тело не приведено к окну слота: %s", string(v.Body))
	}
	if strings.Contains(string(v.Body), `"num_ctx":16384`) {
		t.Fatalf("в теле осталось запрошенное клиентом окно: %s", string(v.Body))
	}
}

// TestR301_TransportServesStreamingInsteadOfWorkerError — тот же случай через
// настоящий вход транспорта: preflight обязан вернуть подменённое тело и НЕ
// пропустить запрос дальше с num_ctx больше загруженного.
func TestR301_TransportServesStreamingInsteadOfWorkerError(t *testing.T) {
	p := newGrowthGuardProxyR299()

	body, needsProxy, msg, status := p.preflightNCtxReloadIfNeeded(
		httptest.NewRequest(http.MethodPost, "/api/chat", nil).Context(),
		"bk-1", "m", []byte(r301StreamBody), "/api/chat")
	if !needsProxy || status != http.StatusOK || msg != "" {
		t.Fatalf("needsProxy=%v status=%d msg=%q: стриминг с влезающим промптом должен обслуживаться", needsProxy, status, msg)
	}
	if !strings.Contains(string(body), `"num_ctx":8192`) {
		t.Fatalf("preflight вернул тело без приведения к окну слота: %s", string(body))
	}
}

// TestR301_StreamingGrowthIsRefusedBeforeNetwork — промпт не влезает: отказ с
// числами (нужно/загружено), а не кадр ошибки воркера внутри 200.
func TestR301_StreamingGrowthIsRefusedBeforeNetwork(t *testing.T) {
	p := newGrowthGuardProxyR299()

	body, needsProxy, msg, status := p.preflightNCtxReloadIfNeeded(
		httptest.NewRequest(http.MethodPost, "/api/chat", nil).Context(),
		"bk-1", "m", []byte(r301LongStreamBody()), "/api/chat")
	if needsProxy {
		t.Fatal("нужно отказать: стриминговый путь окно не растит")
	}
	if status != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", status)
	}
	if len(body) == 0 {
		t.Fatal("preflight вернул пустое тело: вызывающему нечего отдать клиенту")
	}
	if !strings.Contains(msg, "8192") || !strings.Contains(msg, "streaming request does not reload") {
		t.Fatalf("в причине нет чисел и объяснения: %q", msg)
	}
}

// TestR301_RefusalBodyKeepsR299Format — тело отказа то же, что у остальных
// «отложено» (R291/R299): новый формат не выдумываем, иначе клиент не сможет
// обработать отказ одним правилом.
func TestR301_RefusalBodyKeepsR299Format(t *testing.T) {
	p := newGrowthGuardProxyR299()
	body := []byte(r301LongStreamBody())

	v := p.growthVerdictR301("bk-1", "m", body, "/api/chat", true)
	if !v.Defer {
		t.Fatal("ожидался отказ")
	}
	rec := httptest.NewRecorder()
	writePreflightDeferredR299(rec, p, "bk-1", "m", body, "/api/chat",
		growthDeferMessageR301("m", v), 30)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	out := rec.Body.String()
	for _, want := range []string{
		`"deferredReason":"context_growth_pending"`,
		`"requiredTokens"`,
		`"loadedWindowTokens":8192`,
		`"activeStreams"`,
		`"reloadPending"`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("в теле отказа нет %s: %s", want, out)
		}
	}
}

// TestR301_NonStreamingPathUnchanged — не-стриминговый путь по-прежнему даёт
// preflight'у попробовать вырасти, когда никто не мешает (R299 не сломан).
func TestR301_NonStreamingPathUnchanged(t *testing.T) {
	p := newGrowthGuardProxyR299()

	body := []byte(strings.Replace(r301StreamBody, `"stream":true`, `"stream":false`, 1))
	if v := p.growthVerdictR301("bk-1", "m", body, "/api/chat", false); v.decided() {
		t.Fatalf("без стримов и без перезагрузки рост возможен: verdict=%+v", v)
	}

	stop := beginClientStreamR271(p, "bk-1", "m")
	defer stop()
	v := p.growthVerdictR301("bk-1", "m", body, "/api/chat", false)
	if !v.Defer {
		t.Fatal("при активном стриме рост отложен guard'ом R271 — нужен отказ (правило R299)")
	}
	if v.Active < 1 {
		t.Fatalf("activeStreams = %d, want >= 1", v.Active)
	}
}

// TestR301_BackendWithLoadedModel — проверка ДО очереди не имеет sticky-бэкенда,
// поэтому окно берётся у того, где модель реально загружена (только чтение).
func TestR301_BackendWithLoadedModel(t *testing.T) {
	p := newGrowthGuardProxyR299()
	if got := p.backendWithLoadedModelR301("m"); got != "bk-1" {
		t.Fatalf("backendWithLoadedModelR301 = %q, want bk-1", got)
	}
	if got := p.backendWithLoadedModelR301("не-загружена"); got != "" {
		t.Fatalf("для незагруженной модели ожидалось пусто, получили %q", got)
	}
}

// TestR301_StewardNoStreamingBypass — сторож: обход не должен вернуться.
//
// Проверяет ровно то, что было дефектом:
//   - в preflightNCtxReloadIfNeeded стриминговый пропуск стоит ПОСЛЕ вердикта
//     R301 и после проверки "координатор выключен" он уже не срабатывает
//     (вердикт считается первым);
//   - на поверхности :18080 проверка стоит ДО admission-очереди.
func TestR301_StewardNoStreamingBypass(t *testing.T) {
	src, err := os.ReadFile("nctx_reload_handlers.go")
	if err != nil {
		t.Fatalf("read nctx_reload_handlers.go: %v", err)
	}
	text := string(src)
	// Область поиска — сама функция: в файле есть другие «p.nctxReload == nil»
	// (preflightNCtxReloadIfNeededSync и др.), и глобальный Index показал бы не ту.
	fn := strings.Index(text, "func (p *Proxy) preflightNCtxReloadIfNeeded(")
	if fn < 0 {
		t.Fatal("не найдена функция preflightNCtxReloadIfNeeded: разбор сторожевого теста устарел")
	}
	body := text[fn:]
	verdictAt := strings.Index(body, "growthVerdictR301(backendID, modelName, bodyBuf, requestPath, true)")
	if verdictAt < 0 {
		t.Fatal("в preflightNCtxReloadIfNeeded нет стримингового вердикта R301 — обход вернулся")
	}
	nilCoordAt := strings.Index(body, "if p.nctxReload == nil {")
	if nilCoordAt < 0 {
		t.Fatal("не найден выход «координатор выключен»: разбор сторожевого теста устарел")
	}
	if verdictAt > nilCoordAt {
		t.Fatal("стриминговый вердикт стоит ПОСЛЕ выхода при выключенном координаторе: часть путей снова обходит guard")
	}

	proxySrc, err := os.ReadFile("proxy.go")
	if err != nil {
		t.Fatalf("read proxy.go: %v", err)
	}
	if !bytes.Contains(proxySrc, []byte("servehttp-before-admission")) {
		t.Fatal("нет проверки ДО admission-очереди на поверхности :18080")
	}
	if strings.Index(string(proxySrc), "servehttp-before-admission") > strings.Index(string(proxySrc), "waitForInferenceBackend(") {
		t.Fatal("проверка стоит ПОСЛЕ ожидания слота: клиент снова простоит в очереди впустую")
	}
}
