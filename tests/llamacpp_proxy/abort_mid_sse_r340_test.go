// abort_mid_sse_r340_test.go — R340 (2026-10-30).
//
// ДЕТЕРМИНИРОВАННЫЙ ТЕСТ «КЛИЕНТ ОБОРВАЛ SSE-ПОТОК ПОСРЕДИ ОТВЕТА».
//
// ЧТО ПРОВЕРЯЕТСЯ (наблюдаемые инварианты, а не «нет ошибки в логе»):
//  1. балансер не паникует и обслуживает следующий запрос;
//  2. слот admission освобождён (иначе следующий клиент ждёт чужую генерацию);
//  3. учёт активных стримов снят (иначе Shutdown/R271 ждут несуществующий стрим);
//  4. флаг HasActiveStream у сессии снят;
//  5. воркер увидел отмену — балансер закрыл соединение с ним.
//
// ПОЧЕМУ ТЕСТ УСТРОЕН ИМЕННО ТАК (замерено на этом стенде, не предположение):
//
//	На hijack-пути (/v1) уход клиента обнаруживается НЕ горутиной polling'а
//	(proxy_request_hijack.go:346-396), а ОШИБКОЙ ЗАПИСИ в мёртвый сокет. Замер:
//	  - если после обрыва балансеру есть что писать — обнаружение < 2 мс;
//	  - если upstream молчит, polling не срабатывает вовсе, и обрыв
//	    обнаруживается только на ближайшей записи heartbeat: замерено 29.9 с
//	    при HeartbeatIntervalSec=15 (proxy_timeout.go:152-159).
//	Поэтому в детерминированном тесте воркер обязан ПРОДОЛЖАТЬ генерировать
//	после обрыва (как живой: он про ушедшего клиента не знает,
//	cancel_upstream_r184.go:1-15), а кадры выдаются по шагам теста — тогда
//	«первая же запись после обрыва» гарантирована, а не «повезло по таймингу».
//
// ПОЧЕМУ НЕ time.Sleep КАК СИНХРОНИЗАЦИЯ. Все переходы — наблюдаемые события:
// кадр у клиента (значит цикл стрима уже идёт), закрытие соединения воркером
// (канал), счётчики слотов. r340WaitFor ждёт именно «счётчик догнал реальность»
// (освобождение слота — следствие defer'ов в обработчике), а не «подождём».
package llamacpp_proxy

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"ollama-loadbalancer/internal/balancer"
	"ollama-loadbalancer/pkg/types"
)

const (
	// r340BackendID — СВОЙ id бэкенда на этот тест.
	//
	// Почему не "llamacpp-1" из createTestProxyForLLamaCpp
	// (tests/llamacpp_proxy/llamacpp_proxy_test.go:344): кэш метрик, снапшот
	// кластера и координатор перезагрузки ключуются ИМЕННО id, поэтому общий id
	// на весь пакет даёт зависимость от порядка запуска — это уже ловили и
	// лечили в R337 (tests/llamacpp_proxy/client_api_contract_r118_test.go:305-320:
	// «preflight следующего теста видел ЧУЖОЕ окно»).
	r340BackendID = "r340-abort"
	// r340Model — модель, «загруженная» в заглушке.
	r340Model = "test-model"
	// r340Frames — сколько кадров воркер вообще готов отдать в одном стриме
	// (поток он не завершает: ни done, ни EOF — как живой воркер).
	r340Frames = 8
	// r340StepsAfterAbort — сколько кадров воркер отдаёт ПОСЛЕ обрыва, чтобы
	// балансер гарантированно сделал запись в мёртвый сокет. Первая запись после
	// закрытия может «успешно» уйти в полузакрытый сокет, следующие — нет.
	r340StepsAfterAbort = 4
)

// r340Worker — заглушка cppworker, которая отдаёт кадры ПО ШАГАМ теста.
//
// Между шагами она «замерзает»: ни данных, ни EOF. Это принципиально — иначе
// балансер мог бы завершить стрим по EOF upstream, и тест проверял бы не уход
// клиента, а нормальное завершение.
type r340Worker struct {
	server *httptest.Server
	frames int

	// step — разрешение отдать очередной кадр (буфер: тест не блокируется).
	step chan struct{}
	// closedSeen закрывается РОВНО ОДИН раз, когда заглушка увидела отмену:
	// ошибку записи в сокет или отмену контекста запроса.
	closedSeen chan struct{}
	closedOnce sync.Once
	// freeze размораживает зависшие обработчики на выходе из теста.
	freeze chan struct{}

	mu         sync.Mutex
	framesSent int
}

func newR340Worker(t *testing.T, frames int) *r340Worker {
	t.Helper()
	w := &r340Worker{
		frames:     frames,
		step:       make(chan struct{}, 32),
		closedSeen: make(chan struct{}),
		freeze:     make(chan struct{}),
	}
	w.server = httptest.NewServer(http.HandlerFunc(w.handle))
	t.Cleanup(func() {
		// Порядок важен: сначала размораживаем обработчики, потом закрываем
		// сервер (иначе Close ждал бы их).
		close(w.freeze)
		w.server.Close()
	})
	return w
}

func (w *r340Worker) noteClosed() { w.closedOnce.Do(func() { close(w.closedSeen) }) }

// push — разрешить воркеру отдать n кадров. Не блокируется.
func (w *r340Worker) push(n int) {
	for i := 0; i < n; i++ {
		select {
		case w.step <- struct{}{}:
		default:
			return
		}
	}
}

func (w *r340Worker) sentFrames() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.framesSent
}

func (w *r340Worker) handle(rw http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/health":
		_, _ = rw.Write([]byte(`{"status":"ok"}`))

	// Пробы, на которые опирается балансер (доступность узла и готовность
	// модели). Без /api/models гейт автозагрузки ждал бы загрузку, которую
	// заглушка никогда не объявит (R335, llamacpp_proxy_test.go:67-99).
	case "/api/version":
		rw.Header().Set("Content-Type", "application/json")
		_, _ = rw.Write([]byte(`{"version":"0.0.0-r340-stub"}`))

	case "/api/models":
		now := time.Now().UTC().Format(time.RFC3339)
		rw.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(rw, `{"count":1,"models":[{"name":%q,"state":"loaded",`+
			`"context_size":32768,"context_per_seq":32768,"max_slots":2,"loaded_at":%q}]}`,
			r340Model, now)

	case "/api/models/load":
		rw.Header().Set("Content-Type", "application/json")
		_, _ = rw.Write([]byte(`{"success":true,"status":"loaded"}`))

	case "/v1/chat/completions":
		w.streamChatCompletions(rw, r)

	default:
		http.Error(rw, "not found", http.StatusNotFound)
	}
}

// writeFrame — отдать один OpenAI-совместимый SSE-кадр.
func (w *r340Worker) writeFrame(rw http.ResponseWriter, flusher http.Flusher, i int) error {
	chunk := map[string]interface{}{
		"id":      fmt.Sprintf("chatcmpl-r340-%d", i),
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   r340Model,
		"choices": []map[string]interface{}{
			{
				"index": 0,
				"delta": map[string]interface{}{"content": fmt.Sprintf("token-%d ", i)},
			},
		},
	}
	payload, err := json.Marshal(chunk)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(rw, "data: %s\n\n", payload); err != nil {
		return err
	}
	flusher.Flush()
	w.mu.Lock()
	w.framesSent++
	w.mu.Unlock()
	return nil
}

// streamChatCompletions — SSE без завершения: кадры по шагам, между шагами тишина.
func (w *r340Worker) streamChatCompletions(rw http.ResponseWriter, r *http.Request) {
	flusher, ok := rw.(http.Flusher)
	if !ok {
		http.Error(rw, "flusher not supported", http.StatusInternalServerError)
		return
	}
	rw.Header().Set("Content-Type", "text/event-stream")
	rw.Header().Set("Cache-Control", "no-cache")
	rw.WriteHeader(http.StatusOK)
	flusher.Flush()

	for i := 0; i < w.frames; i++ {
		// Замерли: кадр отдаётся только когда тест разрешил, а до этого нет ни
		// данных, ни EOF. Уход клиента виден и здесь (ctx), но балансер узнаёт
		// об отмене РАНЬШЕ нас — по ошибке записи.
		select {
		case <-r.Context().Done():
			w.noteClosed()
			return
		case <-w.freeze:
			return
		case <-w.step:
		}
		if err := w.writeFrame(rw, flusher, i); err != nil {
			// Ошибка записи = балансер закрыл upstream. Это и есть «воркер
			// получил отмену» (вторая половина R184: отмену надо ещё прислать).
			w.noteClosed()
			return
		}
	}
	// Кадры кончились, но поток НЕ завершён: замерли до отмены/выхода из теста.
	select {
	case <-r.Context().Done():
		w.noteClosed()
	case <-w.freeze:
	}
}

// r340TestProxy — прокси с ОДНИМ llama.cpp-бэкендом под своим id.
//
// Копия createTestProxyForLLamaCpp (llamacpp_proxy_test.go:309-363) с единственным
// отличием: ID = r340BackendID. Общий helper не переиспользуем, чтобы не менять
// чужой тест-код и не делить с пакетом один id (см. комментарий к r340BackendID).
// Остальные поля конфига повторены как есть — иначе тест проверял бы другой режим
// (stickiness/TTL/таймауты участвуют в пути стрима).
func r340TestProxy(t *testing.T, cppWorkerURL string) *balancer.Proxy {
	t.Helper()

	hostPort := strings.TrimPrefix(cppWorkerURL, "http://")
	parts := strings.Split(hostPort, ":")
	host := parts[0]
	port := 8080
	if len(parts) > 1 {
		fmt.Sscanf(parts[1], "%d", &port)
	}

	cfg := &types.LoadBalancerConfig{
		Balancing: types.BalancingSettings{
			OperatingMode:     "standard",
			SessionStickiness: true,
			SessionTTL:        60,
			SessionIdleTTL:    60,
			RequestTimeout:    30,
			QueueMaxSize:      100,
			ModelAffinity:     true,
			SyncModelLoad: types.SyncModelLoadConfig{
				Enabled: false,
			},
			Prewarm: types.PrewarmConfig{
				TriggerLoadThreshold: 0.80,
			},
			AdvancedTiming: types.AdvancedTimingConfig{
				ZombieSessionThresholdSec: 120,
				StreamingRetryDelayMs:     500,
				MaxConcurrentWarmups:      3,
				WarmupSemaphoreTimeoutSec: 30,
			},
		},
		Backends: []types.Backend{
			{
				ID:                r340BackendID,
				Name:              "R340 abort backend",
				Host:              host,
				OllamaPort:        port,
				CppWorkerPort:     port,
				Type:              types.BackendTypeLlamaCpp,
				Status:            types.StatusHealthy,
				MaxConcurrentReqs: 10,
				Weight:            100,
			},
		},
		BackendEngine: types.EngineAuto,
		LoadBalancer: types.LoadBalancerSettings{
			StatePath: "",
		},
	}

	return balancer.NewProxy(cfg)
}

// r340RegisterModel — модель «загружена» с окном 32768.
//
// Отдельно от registerLlamaCppModel (llamacpp_proxy_test.go:365) намеренно: тот
// объявляет ContextLength=2048, а заглушка отдаёт context_per_seq=32768. Два
// источника окна расходятся, preflight решает «надо растить окно», уходит в
// асинхронный reload и отвечает 503 (nctx_reload.go:1168-1187). Тест тогда
// проверял бы не обрыв, а preflight. Источники окна обязаны согласовываться.
func r340RegisterModel(proxy *balancer.Proxy, backendID, modelName string) {
	proxy.UpdateMetrics(backendID, &types.BackendMetrics{
		ID:          backendID,
		BackendType: types.BackendTypeLlamaCpp,
		LlamaCpp: types.LlamaCppMetrics{
			LoadedModels: []types.LlamaCppModel{
				{Name: modelName, State: "loaded", ContextLength: 32768},
			},
		},
		Ollama: types.OllamaMetrics{RunningModels: []types.RunningModel{}},
	})
}

// r340ActiveReqs — сколько слотов бэкенда занято прямо сейчас.
//
// Через GetDirectProcessingRequests, а не через BackendState.ActiveReqs: метод
// берёт внутренние локи (cluster_state.go:470-504), прямое чтение поля из теста
// было бы гонкой под -race.
func r340ActiveReqs(proxy *balancer.Proxy, backendID string) int {
	n := 0
	for _, row := range proxy.GetDirectProcessingRequests() {
		if target, _ := row["target"].(string); target == backendID {
			n++
		}
	}
	return n
}

// r340SessionStreaming — помечена ли сессия как «идёт стрим» (R251/R252).
func r340SessionStreaming(proxy *balancer.Proxy, sessionID string) bool {
	for _, s := range proxy.GetSessions() {
		if s.ID == sessionID {
			return s.HasActiveStream
		}
	}
	return false
}

// r340WaitFor — дождаться наблюдаемого условия с дедлайном.
func r340WaitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("не дождались за %s: %s", timeout, what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// r340ReadDataFrames — прочитать n SSE-кадров (строки "data:").
//
// Чтение в горутине: без этого зависший балансер подвесил бы весь тест до
// таймаута пакета, и причина зависания была бы не видна в отчёте.
func r340ReadDataFrames(t *testing.T, body io.Reader, n int) []string {
	t.Helper()
	type lineResult struct {
		line string
		err  error
	}
	lines := make(chan lineResult, 1)
	go func() {
		reader := bufio.NewReader(body)
		for {
			line, err := reader.ReadString('\n')
			lines <- lineResult{line: line, err: err}
			if err != nil {
				return
			}
		}
	}()

	frames := make([]string, 0, n)
	deadline := time.After(10 * time.Second)
	for len(frames) < n {
		select {
		case res := <-lines:
			if res.err != nil {
				t.Fatalf("чтение SSE: %v (прочитано кадров: %d)", res.err, len(frames))
			}
			// Пустые строки-разделители и `: keepalive` — не кадры данных.
			if !strings.HasPrefix(res.line, "data:") {
				continue
			}
			frames = append(frames, res.line)
		case <-deadline:
			t.Fatalf("кадры не пришли за 10 c: прочитано %d из %d", len(frames), n)
		}
	}
	return frames
}

// r340OpenStream — открыть стриминговый запрос через балансер (тело не читаем).
func r340OpenStream(t *testing.T, client *http.Client, baseURL string) *http.Response {
	t.Helper()
	reqBody := `{"model":"` + r340Model + `","stream":true,` +
		`"messages":[{"role":"user","content":"hi"}]}`
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		baseURL+"/v1/chat/completions", strings.NewReader(reqBody))
	if err != nil {
		t.Fatalf("сборка запроса: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("запрос через балансер: %v", err)
	}
	return resp
}

// TestR340_ClientAbortMidSSE — клиент закрывает тело SSE-ответа посреди потока.
// Балансер обязан заметить уход, отпустить слот и учёт стрима, закрыть upstream
// (воркер увидит отмену) и остаться работоспособным.
func TestR340_ClientAbortMidSSE(t *testing.T) {
	// t.Setenv несовместим с t.Parallel — тест последовательный, это осознанно.
	//
	// Сторож тишины ВЫКЛЮЧЕН: заглушка молчит между шагами намеренно, а сторож
	// (stream_stall_r297.go:57, дефолт 300 c) закрыл бы поток по СВОЕЙ причине и
	// прислал воркеру отмену "upstream_stall" — тест проверял бы тишину, а не
	// уход клиента. Второй порог (до первого кадра) выключен по той же причине.
	t.Setenv("LB_STREAM_STALL_ABORT_SEC", "0")
	t.Setenv("LB_STREAM_FIRST_FRAME_ABORT_SEC", "0")
	// Ожидание слота — 1 c: если слот утечёт, повторный запрос обязан быстро
	// получить 503 (admission_queue.go:608-672), а не подвесить пакет на 300 c.
	t.Setenv("LB_ADMISSION_WAIT_SEC", "1")
	// Модель в заглушке уже «загружена» — авто-ожидание загрузки не нужно.
	t.Setenv("LB_AUTO_LOAD_WAIT_SEC", "0")

	worker := newR340Worker(t, r340Frames)

	proxy := r340TestProxy(t, worker.server.URL)
	r340RegisterModel(proxy, r340BackendID, r340Model)
	t.Cleanup(func() {
		// Гасим фоновые петли: иначе они переживут тест и продолжат опрашивать
		// уже закрытую заглушку.
		proxy.StopSessionManager()
		proxy.StopQueue()
	})

	// НАСТОЯЩИЙ сервер, а не httptest.NewRecorder: /v1 уходит в hijack-путь
	// только если ResponseWriter умеет Hijack (proxy_request.go:713-724). В
	// NewRecorder проверялся бы legacy-путь — то есть не основной тракт Cline.
	server := httptest.NewServer(proxy)
	t.Cleanup(server.Close)

	baseline := r340ActiveReqs(proxy, r340BackendID)

	client := &http.Client{
		// Таймаут — только страховка от зависания самого теста. Обрыв делаем
		// ЗАКРЫТИЕМ ТЕЛА (шаг 3), а не таймаутом клиента: иначе нельзя
		// утверждать, что причиной отмены был именно уход клиента.
		Timeout: 20 * time.Second,
	}

	// --- 1. Открываем SSE-запрос ---------------------------------------------
	resp := r340OpenStream(t, client, server.URL)
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("статус %d, want 200; тело: %s", resp.StatusCode, raw)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		resp.Body.Close()
		t.Fatalf("Content-Type = %q, want text/event-stream: клиент не в SSE-ветке", ct)
	}
	// X-Session-ID отдаёт сам балансер (llamacpp_handlers_inference.go:144-152).
	sessionID := resp.Header.Get("X-Session-ID")

	// --- 2. Убеждаемся, что стрим ЖИВОЙ и держит слот ------------------------
	// Кадр, доехавший до клиента, — доказательство, что мы внутри цикла стрима
	// (иначе проверялось бы поведение обрыва ДО заголовков — это другой случай).
	worker.push(1)
	frames := r340ReadDataFrames(t, resp.Body, 1)
	if !strings.Contains(frames[0], "chat.completion.chunk") {
		t.Errorf("первый кадр не в форме OpenAI-чанка: %s", frames[0])
	}
	r340WaitFor(t, 5*time.Second, "запрос занял слот бэкенда", func() bool {
		return r340ActiveReqs(proxy, r340BackendID) == baseline+1
	})
	if sessionID != "" {
		r340WaitFor(t, 5*time.Second, "сессия помечена как стримящая", func() bool {
			return r340SessionStreaming(proxy, sessionID)
		})
	} else {
		t.Logf("X-Session-ID не пришёл — проверка флага сессии пропущена")
	}

	// --- 3. ОБРЫВ: клиент закрывает тело посреди ответа ----------------------
	// Именно это делает Cline/Roo по кнопке Stop и curl по Ctrl+C. Chunked-ответ
	// недочитан, поэтому Go не переиспользует соединение, а закрывает его.
	abortAt := time.Now()
	_ = resp.Body.Close()

	// Воркер продолжает генерировать (он про клиента не знает) — и именно его
	// кадры дают балансеру запись, на которой уход клиента и обнаруживается.
	worker.push(r340StepsAfterAbort)

	// --- 4. Инварианты -------------------------------------------------------
	// 4a. Воркер увидел отмену: балансер закрыл upstream
	// (defer resp.Body.Close(), proxy_request_hijack.go:142). Без этого
	// генерация продолжится и будет держать слот воркера (R184).
	select {
	case <-worker.closedSeen:
		t.Logf("воркер увидел обрыв через %s", time.Since(abortAt))
	case <-time.After(5 * time.Second):
		t.Fatalf("воркер не увидел обрыв upstream за 5 c: балансер не закрыл соединение с воркером (R184)")
	}

	// 4b. Слот admission освобождён. Обработчик отпускает его через defer
	// (llamacpp_handlers_inference.go:272-276) ровно тогда, когда вернулся.
	r340WaitFor(t, 5*time.Second, "слот бэкенда освобождён после обрыва", func() bool {
		return r340ActiveReqs(proxy, r340BackendID) == baseline
	})

	// 4c. Учёт активных стримов снят (active_streams_r271.go:108-153). Иначе
	// Shutdown (R217/R219) и разрушительные операции (R271) будут ждать стрим,
	// которого уже нет.
	r340WaitFor(t, 5*time.Second, "учёт активных стримов снят", func() bool {
		return proxy.ActiveStreamsOnBackend(r340BackendID) == 0
	})

	// 4d. Флаг HasActiveStream снят (stream_session_r252.go:134-158): сессия не
	// остаётся «вечно стримящей» и не мешает зомби-уборке/перепривязке.
	if sessionID != "" {
		r340WaitFor(t, 5*time.Second, "флаг HasActiveStream снят", func() bool {
			return !r340SessionStreaming(proxy, sessionID)
		})
	}

	// --- 5. Утечки нет: следующий запрос проходит и получает свой стрим ------
	resp2 := r340OpenStream(t, client, server.URL)
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp2.Body)
		t.Fatalf("повторный запрос после обрыва: статус %d (слот не освобождён?); тело: %s",
			resp2.StatusCode, raw)
	}
	// Заголовки ожидания в очереди выставляет только путь через admission
	// (admission_queue.go:806-815): их наличие = запрос ждал слот, то есть
	// после обрыва слот не был свободен сразу.
	if waited := resp2.Header.Get("X-Queue-Wait-Ms"); waited != "" {
		t.Errorf("повторный запрос стоял в admission-очереди (X-Queue-Wait-Ms=%s): слот не освободился", waited)
	}
	worker.push(1)
	if frames2 := r340ReadDataFrames(t, resp2.Body, 1); !strings.Contains(frames2[0], "chat.completion.chunk") {
		t.Errorf("повторный стрим отдал не тот кадр: %s", frames2[0])
	}
}
