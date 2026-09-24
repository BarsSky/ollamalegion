package balancer

// admission_queue_r67b_test.go — R67b (2026-09-23): admission-очередь вместо
// «резкого 503» + per-session (per-user) справедливость.
//
// Жалоба R67 (дословно): «балансер резко отбивает повторный запрос от клиента
// выдавая 503, а не поставляя в очередь, скорей всего причина в том что не
// различает разных пользователей клиента OpenWebUI, что может предоставлять
// модель сразу нескольким пользователям одновременно» + «даже если от одного
// пользователя идет новый запрос, балансер его не ставит в очередь на отработку
// и не делает как новую сессию».
//
// Что проверяем:
//  1. занятый слот → запрос ЖДЁТ и получает слот после освобождения (не 503);
//  2. LB_ADMISSION_WAIT_SEC=0 → прежнее поведение (сразу ошибка/503);
//  3. исчерпанное ожидание → errAdmissionTimeout (хендлер отдаёт 503 +
//     Retry-After + X-Queue-Position), статистика таймаутов растёт;
//  4. повторные запросы уже обслуживаемой сессии УСТУПАЮТ новой сессии
//     (один пользователь не занимает очередь целиком);
//  5. идентичность пользователя/чата: разные пользователи OpenWebUI (один IP,
//     один Authorization-токен) больше не слипаются в одну сессию.

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"ollama-loadbalancer/pkg/types"
)

// TestHandleChat_QueuesWhenSlotsBusy_R67b — HTTP-уровень: пока слот бэкенда
// занят, холодный /api/chat НЕ получает 503, а ждёт освобождения слота и
// обслуживается (в ответе — X-Queue-Wait-Ms). До R67b такой запрос отбивался
// «all backends busy»/«no free slot».
//
// R70: keepalive для streaming-ожидающих выключаем явно — при нём заголовки
// коммитятся ДО выдачи слота (X-Queue-Keepalive вместо X-Queue-*), а этот тест
// проверяет именно «очередь видна в заголовках». Поведение с keepalive —
// admission_keepalive_r70_test.go.
func TestHandleChat_QueuesWhenSlotsBusy_R67b(t *testing.T) {
	t.Setenv("LB_ADMISSION_KEEPALIVE_SEC", "0")

	var received []byte
	var mu sync.Mutex
	upstream := makeUpstreamForOllamaTest(t, &received, &mu)
	defer upstream.Close()

	p, router, cleanup := makeTestLlamaProxy(t, upstream.URL)
	defer cleanup()
	p.setAdmissionWait(3 * time.Second)

	// Лимит параллелизма: 1 слот (как cppworker с n_parallel=1). В конфиге
	// тестового бэкенда maxConcurrentReqs=0 («без лимита»), поэтому выставляем явно.
	p.mu.RLock()
	state := p.backends["llama_test"]
	p.mu.RUnlock()
	state.mu.Lock()
	state.Backend.MaxConcurrentReqs = 1
	state.mu.Unlock()

	// Занимаем единственный слот бэкенда (как другая активная генерация).
	if !p.tryAcquireSlot("llama_test") {
		t.Fatal("не удалось занять слот llama_test")
	}

	body := `{"model":"qwen2.5-coder","messages":[{"role":"user","content":"hi"}],"stream":true}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/chat", bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-User-Id", "openwebui-user-1")

	done := make(chan struct{})
	go func() {
		defer close(done)
		router.handleChat(rec, req)
	}()

	// Держим слот, затем освобождаем — запрос обязан дождаться и пройти.
	time.Sleep(300 * time.Millisecond)
	p.releaseSlot("llama_test")

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("handleChat не завершился: запрос завис в admission-очереди")
	}

	if rec.Code != http.StatusOK {
		t.Fatalf("ожидался 200 (запрос дождался слота), получено %d: %s", rec.Code, rec.Body.String())
	}
	if wait := rec.Header().Get("X-Queue-Wait-Ms"); wait == "" {
		t.Error("отсутствует заголовок X-Queue-Wait-Ms (клиент не видит ожидание в очереди)")
	}
	if pos := rec.Header().Get("X-Queue-Position"); pos != "1" {
		t.Errorf("X-Queue-Position = %q, ожидалось \"1\"", pos)
	}
	mu.Lock()
	gotBody := len(received)
	mu.Unlock()
	if gotBody == 0 {
		t.Error("upstream не получил inference-запрос после освобождения слота")
	}
	t.Logf("✅ HTTP: /api/chat прождал слот (X-Queue-Wait-Ms=%s) и получил %d",
		rec.Header().Get("X-Queue-Wait-Ms"), rec.Code)
}

// admissionTestProxy — Proxy с одним healthy llama.cpp-бэкендом и заданным
// maxConcurrentReqs.
func admissionTestProxy(t *testing.T, maxReqs int) *Proxy {
	t.Helper()
	cfg := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{Host: "localhost", Port: 8080, APIPort: 8081},
		Backends: []types.Backend{
			{
				ID:                "llama_adm",
				Name:              "test llama.cpp admission",
				Host:              "127.0.0.1",
				OllamaPort:        11434,
				CppWorkerPort:     18092,
				Type:              types.BackendTypeLlamaCpp,
				Weight:            1,
				Status:            types.StatusHealthy,
				MaxConcurrentReqs: maxReqs,
			},
		},
		Balancing: types.BalancingSettings{
			Algorithm:           types.AlgorithmResourceAware,
			HealthCheckInterval: 60,
			MetricsInterval:     60,
		},
		API:  types.APISettings{RateLimit: 100, RateBurst: 200},
		Auth: types.AuthConfig{Enabled: false},
	}
	p := newProxyWithCleanup(t, cfg)
	if p.GetBackend("llama_adm") == nil {
		t.Fatal("backend llama_adm not registered")
	}
	return p
}

// waitForAdmissionWaiters — ждём, пока в очереди окажется want ожидающих.
func waitForAdmissionWaiters(t *testing.T, p *Proxy, want int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if p.admission.stats(p.admissionWaitTimeout()).Waiting >= want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("в очереди не появилось %d ожидающих (stats=%+v)",
		want, p.admission.stats(p.admissionWaitTimeout()))
}

type admissionResult struct {
	err   error
	lease *admissionLease
	pos   int
}

// TestAdmission_WaitsInsteadOfRejecting_R67b — главный сценарий жалобы:
// слот занят → запрос встаёт в очередь и обслуживается ПОСЛЕ освобождения слота
// (до R67b такой запрос мгновенно получал 503).
func TestAdmission_WaitsInsteadOfRejecting_R67b(t *testing.T) {
	p := admissionTestProxy(t, 1)
	p.setAdmissionWait(5 * time.Second)

	// Занимаем единственный слот — как будто другой пользователь уже генерирует.
	if !p.tryAcquireSlot("llama_adm") {
		t.Fatal("не удалось занять единственный слот")
	}

	results := make(chan admissionResult, 1)
	go func() {
		lease, pos, err := p.acquireInferenceSlot(context.Background(), "llama_adm", "X-User-Id:u1")
		results <- admissionResult{lease: lease, pos: pos, err: err}
	}()

	waitForAdmissionWaiters(t, p, 1)
	if got := p.admission.stats(p.admissionWaitTimeout()).Waiting; got != 1 {
		t.Fatalf("ожидался 1 ожидающий, получено %d", got)
	}

	// Держим слот 300 мс — запрос обязан ждать, а не падать.
	time.Sleep(300 * time.Millisecond)
	p.releaseSlot("llama_adm")

	var res admissionResult
	select {
	case res = <-results:
	case <-time.After(3 * time.Second):
		t.Fatal("ожидающий запрос не получил слот после освобождения (broadcast не сработал)")
	}

	if res.err != nil {
		t.Fatalf("ожидалось успешное получение слота, получено: %v", res.err)
	}
	if res.lease == nil {
		t.Fatal("lease == nil при успешном получении слота")
	}
	if res.lease.Waited < 250*time.Millisecond {
		t.Errorf("waited=%v: запрос не ждал слот (ожидалось >=250ms)", res.lease.Waited)
	}
	if res.pos != 1 {
		t.Errorf("позиция в очереди = %d, ожидалась 1", res.pos)
	}
	if st := p.admission.stats(p.admissionWaitTimeout()); st.Served != 1 || st.WaitedTotal != 1 {
		t.Errorf("статистика очереди: served=%d waited=%d (ожидалось 1/1)", st.Served, st.WaitedTotal)
	}
	t.Logf("✅ admission: запрос прождал %v и получил слот (без 503)", res.lease.Waited)

	// После Release слот снова свободен.
	res.lease.Release()
	if st := p.admission.stats(p.admissionWaitTimeout()); st.ActiveByUser != 0 {
		t.Errorf("после Release активных сессий %d, ожидалось 0", st.ActiveByUser)
	}
	if !p.tryAcquireSlot("llama_adm") {
		t.Error("после Release слот не освободился")
	}
	p.releaseSlot("llama_adm")
}

// TestAdmission_DisabledKeepsLegacyBehaviour_R67b — LB_ADMISSION_WAIT_SEC=0
// сохраняет поведение до R67b: слотов нет → вызывающий код отдаёт быстрый 503.
func TestAdmission_DisabledKeepsLegacyBehaviour_R67b(t *testing.T) {
	p := admissionTestProxy(t, 1)
	p.setAdmissionWait(0)

	if !p.tryAcquireSlot("llama_adm") {
		t.Fatal("не удалось занять единственный слот")
	}

	lease, _, err := p.acquireInferenceSlot(context.Background(), "llama_adm", "X-User-Id:u1")
	if !errors.Is(err, errAdmissionDisabled) {
		t.Fatalf("ожидался errAdmissionDisabled, получено: %v", err)
	}
	if lease != nil {
		t.Fatal("lease должен быть nil при выключенном ожидании")
	}
	if st := p.admission.stats(p.admissionWaitTimeout()); st.Enabled {
		t.Error("stats.Enabled должен быть false при LB_ADMISSION_WAIT_SEC=0")
	}
}

// TestAdmission_WaitTimeout_R67b — ожидание исчерпано → errAdmissionTimeout
// (хендлер превращает это в 503 + Retry-After + X-Queue-Position), счётчик
// таймаутов растёт, слот не «утекает».
func TestAdmission_WaitTimeout_R67b(t *testing.T) {
	p := admissionTestProxy(t, 1)
	p.setAdmissionWait(250 * time.Millisecond)

	if !p.tryAcquireSlot("llama_adm") {
		t.Fatal("не удалось занять единственный слот")
	}

	start := time.Now()
	lease, pos, err := p.acquireInferenceSlot(context.Background(), "llama_adm", "X-User-Id:u1")
	elapsed := time.Since(start)

	if !errors.Is(err, errAdmissionTimeout) {
		t.Fatalf("ожидался errAdmissionTimeout, получено: %v", err)
	}
	if lease != nil {
		t.Fatal("lease должен быть nil при таймауте")
	}
	if pos != 1 {
		t.Errorf("позиция в очереди = %d, ожидалась 1", pos)
	}
	if elapsed < 200*time.Millisecond {
		t.Errorf("таймаут наступил за %v — ожидание не отработало", elapsed)
	}
	if st := p.admission.stats(p.admissionWaitTimeout()); st.Timeouts != 1 {
		t.Errorf("stats.Timeouts = %d, ожидалось 1", st.Timeouts)
	}
	if st := p.admission.stats(p.admissionWaitTimeout()); st.Waiting != 0 {
		t.Errorf("после таймаута в очереди осталось %d ожидающих", st.Waiting)
	}
	// Слот по-прежнему занят ровно один раз (таймаут не «съел» чужой слот).
	if st := p.admission.stats(p.admissionWaitTimeout()); st.ActiveByUser != 0 {
		t.Errorf("таймаут не должен создавать активную сессию, получено %d", st.ActiveByUser)
	}
	p.releaseSlot("llama_adm")
	p.mu.RLock()
	state := p.backends["llama_adm"]
	p.mu.RUnlock()
	state.mu.Lock()
	active := state.ActiveReqs
	state.mu.Unlock()
	if active != 0 {
		t.Errorf("ActiveReqs=%d после освобождения, ожидалось 0", active)
	}
}

// TestAdmission_SameSessionYieldsToNewSession_R67b — пункт 4 жалобы: пока
// пользователь A обслуживается, его повторные запросы уступают пользователю B,
// который ещё не обслуживался («не забивает очередь целиком»).
func TestAdmission_SameSessionYieldsToNewSession_R67b(t *testing.T) {
	p := admissionTestProxy(t, 1)
	p.setAdmissionWait(5 * time.Second)

	// A1 уже обслуживается (держит единственный слот).
	a1, _, err := p.acquireInferenceSlot(context.Background(), "llama_adm", "X-User-Id:A")
	if err != nil {
		t.Fatalf("A1 не получил слот: %v", err)
	}

	// A2 (повторный запрос того же пользователя) и B1 (новый пользователь).
	resA2 := make(chan admissionResult, 1)
	resB1 := make(chan admissionResult, 1)
	go func() {
		lease, pos, err := p.acquireInferenceSlot(context.Background(), "llama_adm", "X-User-Id:A")
		resA2 <- admissionResult{lease: lease, pos: pos, err: err}
	}()
	waitForAdmissionWaiters(t, p, 1)
	go func() {
		lease, pos, err := p.acquireInferenceSlot(context.Background(), "llama_adm", "X-User-Id:B")
		resB1 <- admissionResult{lease: lease, pos: pos, err: err}
	}()
	waitForAdmissionWaiters(t, p, 2)

	// Самый приоритетный ожидающий — B (сессия без активных запросов).
	p.admission.mu.Lock()
	best := p.admission.bestLocked("llama_adm")
	p.admission.mu.Unlock()
	if best == nil || best.session != "X-User-Id:B" {
		got := "<nil>"
		if best != nil {
			got = best.session
		}
		t.Fatalf("приоритет отдан сессии %q, ожидалась X-User-Id:B", got)
	}

	a1.Release()

	var b1 admissionResult
	select {
	case b1 = <-resB1:
	case <-time.After(3 * time.Second):
		t.Fatal("B1 не получил слот после освобождения A1 (справедливость не работает)")
	}
	if b1.err != nil {
		t.Fatalf("B1: %v", b1.err)
	}
	if b1.lease.Session != "X-User-Id:B" {
		t.Errorf("слот получила сессия %q, ожидалась X-User-Id:B", b1.lease.Session)
	}

	// A2 получает слот только после B1.
	select {
	case res := <-resA2:
		if res.err == nil {
			res.lease.Release()
			t.Fatal("A2 получил слот раньше B1 — повторные запросы одной сессии не уступают")
		}
	case <-time.After(200 * time.Millisecond):
		// ожидаемо: A2 всё ещё ждёт
	}

	b1.lease.Release()
	select {
	case a2 := <-resA2:
		if a2.err != nil {
			t.Fatalf("A2 не получил слот после B1: %v", a2.err)
		}
		a2.lease.Release()
	case <-time.After(3 * time.Second):
		t.Fatal("A2 так и не получил слот после освобождения B1")
	}
	t.Log("✅ admission: повторный запрос сессии A уступил новой сессии B")
}

// TestRequestSessionKey_Sources_R67b — идентификатор пользователя/чата для
// admission-очереди: заголовки, тело, вложенный metadata (OpenWebUI).
func TestRequestSessionKey_Sources_R67b(t *testing.T) {
	cases := []struct {
		name   string
		header map[string]string
		body   string
		want   string
	}{
		{
			name:   "X-User-Id имеет приоритет",
			header: map[string]string{"X-User-Id": "u-1"},
			body:   `{"user":"other"}`,
			want:   "X-User-Id:u-1",
		},
		{
			name:   "X-OpenWebUI-User-Id",
			header: map[string]string{"X-OpenWebUI-User-Id": "owui-7"},
			want:   "X-OpenWebUI-User-Id:owui-7",
		},
		{
			name:   "X-Chat-Id (сессия/чат)",
			header: map[string]string{"X-Chat-Id": "chat-9"},
			want:   "X-Chat-Id:chat-9",
		},
		{
			name: "user в теле (OpenAI)",
			body: `{"model":"m","user":"user@example.com"}`,
			want: "user:user@example.com",
		},
		{
			name: "metadata.user_id в теле (OpenWebUI)",
			body: `{"model":"m","metadata":{"user_id":"owui-42","chat_id":"c-1"}}`,
			want: "user:owui-42",
		},
		{
			name: "chat_id из тела, если пользователя и session_id нет",
			body: `{"model":"m","chat_id":"c-77"}`,
			want: "chat:c-77",
		},
		{
			name: "session_id из тела имеет приоритет над chat_id",
			body: `{"model":"m","metadata":{"chat_id":"c-88","session_id":"s-1"}}`,
			want: "session:s-1",
		},
		{
			name: "metadata.chat_id из тела (OpenWebUI)",
			body: `{"model":"m","metadata":{"chat_id":"c-88"}}`,
			want: "chat:c-88",
		},
		{
			name: "анонимный клиент",
			body: `{"model":"m","messages":[]}`,
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/chat", bytes.NewReader([]byte(tc.body)))
			for k, v := range tc.header {
				req.Header.Set(k, v)
			}
			if got := RequestSessionKey(req, []byte(tc.body)); got != tc.want {
				t.Errorf("RequestSessionKey = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestSessionIdentity_SeparatesOpenWebUIUsers_R67b — до R67b все пользователи
// OpenWebUI (один IP, один Authorization-токен, одинаковый User-Agent) получали
// ОДНУ сессию: на живом стенде 3 запроса от 3 пользователей дали одну сессию с
// requestCount=3. Теперь каждый пользователь (и каждый его чат) — отдельная
// сессия, то есть «новый запрос = новая сессия».
func TestSessionIdentity_SeparatesOpenWebUIUsers_R67b(t *testing.T) {
	p := admissionTestProxy(t, 4)

	mk := func(user, chat string) *http.Request {
		body := `{"model":"gemma","messages":[]}`
		if chat != "" {
			body = `{"model":"gemma","messages":[],"metadata":{"chat_id":"` + chat + `"}}`
		}
		req := httptest.NewRequest(http.MethodPost, "/api/chat", bytes.NewReader([]byte(body)))
		req.RemoteAddr = "172.18.0.1:5555"
		req.Header.Set("User-Agent", "openwebui/1.0")
		// Общий токен OpenWebUI: именно из-за него пользователи слипались.
		req.Header.Set("Authorization", "Bearer shared-openwebui-key")
		if user != "" {
			req.Header.Set("X-User-Id", user)
		}
		return req
	}

	alice := p.getSessionIDWithModel(mk("alice", ""), "openwebui", "gemma")
	bob := p.getSessionIDWithModel(mk("bob", ""), "openwebui", "gemma")
	if alice == bob {
		t.Fatalf("разные пользователи получили одну сессию %q (регрессия R67b)", alice)
	}

	aliceChat1 := p.getSessionIDWithModel(mk("alice", "chat-1"), "openwebui", "gemma")
	aliceChat2 := p.getSessionIDWithModel(mk("alice", "chat-2"), "openwebui", "gemma")
	if aliceChat1 == aliceChat2 {
		t.Fatalf("разные чаты одного пользователя получили одну сессию %q", aliceChat1)
	}

	anon1 := p.getSessionIDWithModel(mk("", ""), "openwebui", "gemma")
	anon2 := p.getSessionIDWithModel(mk("", ""), "openwebui", "gemma")
	if anon1 != anon2 {
		t.Errorf("анонимные запросы одного клиента должны иметь общую сессию: %q vs %q", anon1, anon2)
	}
	t.Logf("✅ сессии разделены: alice=%s bob=%s alice/chat-1=%s", alice, bob, aliceChat1)

	// Тело запроса не должно пострадать от разбора идентичности.
	req := mk("alice", "chat-1")
	_ = p.getSessionIDWithModel(req, "openwebui", "gemma")
	restored, err := io.ReadAll(req.Body)
	if err != nil || len(restored) == 0 {
		t.Fatalf("тело запроса потеряно после разбора идентичности: err=%v len=%d", err, len(restored))
	}
}
