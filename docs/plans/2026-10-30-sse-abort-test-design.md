Реализовано в тесте: `tests/llamacpp_proxy/abort_mid_sse_r340_test.go` (коммит 52293c29). Текст ниже приведён в соответствие с этим тестом.

# R340: детерминированный тест «клиент оборвал SSE-поток посреди ответа»

Дата: 2026-10-30.
Статус: дизайн + готовый скелет теста. Код проекта не менялся (только чтение).
Целевой файл теста: `tests/llamacpp_proxy/abort_mid_sse_r340_test.go` (пакет
`llamacpp_proxy`, без build-тега — как `tests/llamacpp_proxy/llamacpp_proxy_test.go`).

Что уже есть и почему этого мало:

| Файл | Что проверяет | Чего не проверяет |
|---|---|---|
| `internal/balancer/hijack_keepalive_r219_test.go:21-38` | заголовок `Connection: close` в hack-пути (статический разбор исходника) | поведение при уходе клиента |
| `internal/balancer/stream_abort_r296_test.go:81-164` | кадры-причина/терминатор (`abortFramesR296`) | кто и когда их пишет |
| `internal/balancer/stream_stall_r297_test.go`, `stream_stall_r298_test.go` | сторож ТИШИНЫ upstream | обрыв КЛИЕНТА |
| `tests/streaming_cancel_test.go:19-240` | обрыв клиента на Ollama-пути (`/api/generate`), через `httptest.NewRecorder` | `/v1/chat/completions`, hijack-путь, настоящий TCP-обрыв |

Тест R340 закрывает ровно эту дырку: клиент рвёт соединение посреди SSE на
`/v1/chat/completions`, то есть на основном тракте Cline/Roo.

---

## 1. Текущий путь: что происходит, когда клиент рвёт соединение

### 1.1. Вход в обработчик

1. `Proxy.ServeHTTP` (`internal/balancer/proxy.go:896`) → `routeRequest`
   (`internal/balancer/proxy.go:1158`, реализация `internal/balancer/router.go:71`).
2. `determineRequestBackendType` для `/v1/*` возвращает `llama_cpp`
   (`internal/balancer/proxy.go:766-773`) → `dispatchRouters`
   (`internal/balancer/router.go:129-132`).
3. `LlamaCppRouter.Route` (`internal/balancer/llamacpp_router.go:149-151`) →
   `handleOpenAIChatCompletions` (`internal/balancer/llamacpp_handlers_inference.go:23`).

Важно: `/v1/chat/completions` НЕ идёт через `proxyRequest`/`proxyRequestLlamaCpp`
(трансляция SSE→NDJSON). Он идёт напрямую в
`proxyRequestOpenAIStreaming` — это отдельный тракт со своим поведением.

### 1.2. Сессия, admission, upstream

| Шаг | Место |
|---|---|
| `sessionMgr.SetWithProtocol` + заголовок `X-Session-ID` | `llamacpp_handlers_inference.go:144-152` |
| Взятие слота admission + `defer release()` | `llamacpp_handlers_inference.go:272-276`, `admission_queue.go:775-835` |
| upstream-запрос с **контекстом клиента** (`r.Context()`) | `llamacpp_handlers_inference.go:336-352` |
| передача ответа в SSE-прокси | `llamacpp_handlers_inference.go:421` |

Диспетчер: `proxy_request.go:713-724`. Если `ResponseWriter` умеет
`http.Hijacker` (настоящий HTTP/1.1-сервер) — уходим в
`proxyRequestOpenAIStreamingHijacked` (`proxy_request_hijack.go:135`). Иначе —
legacy-путь `proxyRequestOpenAIStreamingLegacy` (`proxy_request.go:740`).

### 1.3. Hijack-путь: где живёт состояние стрима

| Что | Место |
|---|---|
| `defer resp.Body.Close()` (закрытие upstream на выходе) | `proxy_request_hijack.go:142` |
| `Hijack()` + `defer clientConn.Close()` | `proxy_request_hijack.go:173-185` |
| Заголовки вручную: `Transfer-Encoding: chunked`, `Connection: close`, `X-*` из `w.Header()` | `proxy_request_hijack.go:269-292` |
| `defer` chunked-терминатор `0\r\n\r\n` на ЛЮБОЙ исход | `proxy_request_hijack.go:294-312` |
| Учёт активного стрима (`beginClientStreamR271`) | `proxy_request_hijack.go:327-328` |
| Сессия на время стрима (`startStreamSession`) | `proxy_request_hijack.go:343-344` |
| `clientCtx, cancel := context.WithCancel(r.Context())` | `proxy_request_hijack.go:350-351` |
| Горутина polling'а (SetReadDeadline + Read) | `proxy_request_hijack.go:354-396` |
| Главный цикл: `clientCtx.Done()` / heartbeat / сторож / данные | `proxy_request_hijack.go:460-616` |
| Ветка «клиент ушёл» → `return nil` | `proxy_request_hijack.go:462-466` |
| Ветка «ошибка записи клиенту» → `return nil` | `proxy_request_hijack.go:586-590` |

### 1.4. Что именно замечает обрыв (замерено, не предположение)

Клиент закрывает тело → Go-транспорт закрывает TCP-соединение. У балансера три
потенциальных сигнала, и они НЕ равнозначны:

1. **Ошибка записи в мёртвый сокет** — `proxy_request_hijack.go:586-590`
   (warn `hijack streaming: write to client failed`) или запись heartbeat
   `proxy_request_hijack.go:468-475`. Это единственный надёжный сигнал.
   Замер на этом стенде: если после обрыва балансеру есть что писать —
   обнаружение **< 2 мс** (в прогонах 553 µs … 1.2 мс).
2. **Polling-горутина** (`proxy_request_hijack.go:354-396`): ставит
   `SetReadDeadline(time.Unix(0,0))` (`:375-376`) и делает `Read`. На этом
   стенде (Windows, Go 1.25) такой `Read` возвращает `i/o timeout`, а эта ветка
   трактуется как «клиент жив» (`:379-382` → `continue`), поэтому EOF/RST
   практически не наблюдаются. Замер: при молчащем upstream за 5 с не
   сработала ни разу, лог `hijack polling: client disconnected` не появился.
3. **Ближайшая запись heartbeat**: при молчащем upstream обрыв обнаруживается
   только ею. Замер: **29.9 с** после обрыва при `HeartbeatIntervalSec=15`
   (`proxy_timeout.go:152-159`) — первая запись keepalive ушла «в никуда»,
   упала вторая.

**Вывод, который диктует дизайн теста:** ожидать «polling обнаружит за 100 мс»
нельзя. Детерминированный тест обязан обеспечить ЗАПИСЬ после обрыва, а не
надеяться на тайминг.

### 1.5. Что балансер пишет в поток, «если успевает»

Ничего содержательного: клиента уже нет.

* Кадры-причины из `abortFramesR296` (`stream_abort_frames_r296.go:37-98`) на
  этот путь **не вызываются** — они принадлежат обрыву upstream
  (`proxy_request_hijack.go:478-496`, `proxy_request.go:510-511`) и сторожу тишины.
* `ensureChunkedSSEDone` (`data: [DONE]`) не выполняется — ранний `return nil`.
* В `defer` делается попытка записать chunked-терминатор
  (`proxy_request_hijack.go:307-312`); она падает, и это логируется на Debug
  (`:309-310`) — то есть терминатор в мёртвый сокет тоже не доезжает.

Практическое следствие: «клиент увидел кадр-причину» на этом пути проверить
нельзя и не нужно (клиента нет). Проверяем ресурсы балансера и факт отмены
upstream.

### 1.6. Сессия, слот, счётчики

* **Сессия.** `startStreamSession` (`stream_session_r252.go:80-122`) ставит
  `SetStreamActive(true)` (`:92-97`) и поднимает heartbeat-горутину. На выходе
  `defer streamSess.Stop()` (`proxy_request_hijack.go:344`) →
  `SetStreamActive(false)` (`stream_session_r252.go:152-154`) + снятие записи
  из `recentClients`. Сама сессия из менеджера НЕ удаляется — «сессия закрыта»
  означает «флаг `HasActiveStream` снят» (`pkg/types/session.go:20`).
* **Слот admission.** `defer release()`
  (`llamacpp_handlers_inference.go:272-276`) → `admissionLease.Release`
  (`admission_queue.go:545-551`) → `markInflight(-1)` + `releaseSlot`
  (`slot_manager.go:79-96`: `ActiveReqs--` и `broadcast()` очереди). Пока слот
  занят, следующий запрос при исчерпанных слотах ждёт до
  `LB_ADMISSION_WAIT_SEC` (`admission_queue.go:87-100`, цикл ожидания `:608-672`).
* **Счётчики стримов.** `beginClientStreamR271`/`endClientStream`
  (`proxy_request_hijack.go:327-328`) ведут глобальный `activeStreams` для
  `Shutdown` (`proxy.go:868-883`) и локальный учёт по backend/model
  (`active_streams_r271.go:108-153`), который читает `ActiveStreamsOnBackend`
  (`active_streams_r281.go:50-52`) и разрушительные операции R271.

### 1.7. Что получает воркер

На hijack-пути воркеру **не приходит** `POST /api/cancel`: единственный сигнал —
закрытие соединения (`defer resp.Body.Close()`, `proxy_request_hijack.go:142`).
Заглушка видит это как отмену `r.Context()` своего обработчика (тот же приём,
что в `tests/streaming_cancel_test.go:33-37`) или как ошибку следующей записи.

Расхождение с другими путями (важно для будущих правок):

* блокирующий NDJSON-путь **присылает** отмену:
  `p.cancelUpstreamGeneration(backendID, model, "client_cancel")`,
  `llamacpp_transport.go:856-866`;
* сторож тишины присылает `upstream_stall`:
  `proxy_request_hijack.go:495`, `llamacpp_transport.go:914`;
* сама отмена — `cancel_upstream_r184.go:36-83` (`POST /api/cancel`, асинхронно,
  таймаут 3 с).

Значит: воркер, который не замечает закрытия сокета (долгий prefill, R184),
после обрыва клиента на `/v1` может продолжать генерацию. Это риск, а не
инвариант сегодняшнего кода; тест его не «чинит», но фиксирует, что делает
балансер.

### 1.8. Legacy-путь (без `http.Hijacker`)

`proxy_request.go:909-915`: `clientCtx.Done()` → `return nil`. Ни терминатора,
ни `[DONE]`, ни кадра-причины. Это корректно (клиента нет) и это ДРУГОЕ
поведение — скелет его не покрывает (см. §5).

---

## 2. Скелет теста

Файл: `tests/llamacpp_proxy/abort_mid_sse_r340_test.go`.

Ключевые решения (почему именно так — иначе тест флакает, см. §4):

1. **Настоящий `httptest.NewServer(proxy)`**, а не `httptest.NewRecorder`:
   в hijack-путь уходим только если `ResponseWriter` реализует `http.Hijacker`
   (`proxy_request.go:713-724`). С `NewRecorder` проверялся бы legacy-путь
   (`proxy_request.go:740`), то есть не тракт Cline.
2. **Заглушка отдаёт кадры ПО ШАГАМ** и между шагами «замерзает» (ни данных, ни
   EOF). Шаги нужны, чтобы после обрыва гарантированно была ЗАПИСЬ — именно на
   ней балансер и обнаруживает уход (§1.4). Никакого «подождём, вдруг polling
   сработает».
3. **Сторож тишины выключен** (`LB_STREAM_STALL_ABORT_SEC=0`,
   `LB_STREAM_FIRST_FRAME_ABORT_SEC=0`): иначе поток закрыл бы сторож
   (`stream_stall_r297.go:57-78`) и прислал воркеру отмену `upstream_stall`
   (`proxy_request_hijack.go:495`) — тест проверял бы тишину, а не уход клиента.
4. **`LB_ADMISSION_WAIT_SEC=1`**: утечка слота обязана проявиться как быстрый
   503 (`admission_queue.go:847-879`), а не как 300-секундное зависание пакета.
5. **Своё объявление модели** (`r340RegisterModel`) с окном 32768: см. §4.2.
6. Все ожидания — «дождаться наблюдаемого факта с дедлайном»
   (`r340WaitFor`) или каналы; `time.Sleep` присутствует только как 5-мс шаг
   опроса счётчика и как страховка в тесте, а не как синхронизация.

```go
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
```

Запуск:

```
go vet -tags llama_stub ./tests/llamacpp_proxy/
go test -tags llama_stub -run TestR340_ClientAbortMidSSE -count=10 ./tests/llamacpp_proxy/
go test -tags llama_stub -race -count=1 ./tests/llamacpp_proxy/
```

Проверено локально (2026-10-30, Windows/Go 1.25.4): `gofmt -l` — чисто, `go vet`
— чисто, `-count=10` одиночного теста — ok, весь пакет
`./tests/llamacpp_proxy/` — ok (3 прогона + 1 прогон с `-race`). Замер
обнаружения обрыва в прогонах: 553 µs … 1.2 мс. Файл добавлен во временную
копию рабочего дерева только для проверки и затем удалён — код выше является
источником истины.

---

## 3. Инварианты

| № | Инвариант | Чем наблюдается | Что было бы при дефекте |
|---|---|---|---|
| 1 | Балансер не падает и продолжает обслуживать | второй запрос через тот же `httptest.NewServer` получает 200 + кадр SSE | паника в обработчике (в т.ч. `superfluous WriteHeader`) роняет соединение, второй запрос не получит 200 |
| 2 | Слот admission освобождён | `GetDirectProcessingRequests` (`cluster_state.go:470-504`) возвращает `baseline` записей; в моменте стрима было `baseline+1` | `ActiveReqs` остаётся +1 → при исчерпанных слотах следующий клиент ждёт до `LB_ADMISSION_WAIT_SEC`; при `LB_ADMISSION_WAIT_SEC=1` тест получит 503 |
| 3 | Учёт активных стримов снят | `ActiveStreamsOnBackend` (`active_streams_r281.go:50-52`) == 0 | `Shutdown` (`proxy.go:868-883`) ждёт несуществующий стрим; R271 навсегда считает бэкенд занятым и откладывает reload |
| 4 | Флаг сессии снят | `GetSessions` (`cluster_state.go:522-525`) по `X-Session-ID`: `HasActiveStream=false` | сессия навсегда «стримящая»: зомби-детект её не тронет, `SetStreamActive` рассинхронизирован |
| 5 | Воркер увидел отмену | канал `closedSeen` (заглушка: `r.Context().Done()` или ошибка записи) | upstream не закрыт → воркер продолжает генерацию, слот воркера занят (класс дефекта R184: следующий клиент ждал 100 с) |
| 6 | Стрим был живым в момент обрыва | кадр, прочитанный клиентом до `Body.Close()` | иначе «обрыв» случился до заголовков, и проверки относились бы к другому пути |
| 7 | Стрим прерван ПОСРЕДИ, а не по EOF | заглушка не завершает поток (нет `done`/EOF), между шагами тишина | если бы upstream закрывался сам, тест проверял бы штатное завершение |

Инварианты 2–5 — то, что нельзя увидеть глазами на стенде и что ломается
молча; именно они и есть ценность теста.

---

## 4. Что может флакать и как это исключено

### 4.1. Ожидание, что обрыв заметят за «100 мс polling»

Самая опасная иллюзия: комментарий в коде обещает `<1s detection`
(`proxy_request_hijack.go:8-11,193-194`), но замер показал, что на этом стенде
polling не срабатывает вовсе, и всё решает ближайшая ЗАПИСЬ (§1.4). Наивная
версия теста («заглушка замерла → клиент закрыл тело → ждём») проходит или
падает в зависимости от того, успел ли балансер дописать кадры в сокет:
наблюдалось и 553 µs, и «не обнаружено за 5 с».

**Мера:** заглушка выдаёт кадры по шагам, и после обрыва тест разрешает ещё
`r340StepsAfterAbort = 4` кадра. Первая запись может «успешно» уйти в
полузакрытый сокет — вторая-третья падают. Дедлайн 5 с — только страховка.

### 4.2. Расхождение «загруженное окно» между метриками и заглушкой

`registerLlamaCppModel` (`llamacpp_proxy_test.go:365-378`) объявляет
`ContextLength: 2048`, а заглушка в примерах отдаёт `context_per_seq: 32768`.
Preflight видит два разных окна, решает «надо растить», уходит в асинхронный
reload и отдаёт 503 `preflight reload failed ... reload skipped: effective target
n_ctx=4162 does not exceed currently loaded n_ctx=32768` (`nctx_reload.go:1168-1187`).
Именно это и случилось при прогоне всего пакета, хотя одиночный тест проходил.

**Мера:** `r340RegisterModel` объявляет окно 32768, заглушка отдаёт
`context_size`/`context_per_seq` = 32768. Оба источника согласованы.

### 4.3. Общий id бэкенда «llamacpp-1» на весь пакет

`createTestProxyForLLamaCpp` жёстко задаёт `ID: "llamacpp-1"`
(`llamacpp_proxy_test.go:344`) и кэш метрик/координатор перезагрузки ключуются
именно id. Живой флейк этого класса уже описан и вылечен в R337
(`client_api_contract_r118_test.go:305-320`: «preflight следующего теста видел
ЧУЖОЕ окно»).

**Мера сегодня:** §4.2 снимает самый частый симптом. **Мера на будущее:** если
пакет снова начнёт флакать — завести свой id: собрать конфиг в тесте (копия
`createTestProxyForLLamaCpp` с `ID: "r340-abort"`), а не править общий helper.

### 4.4. Сторож тишины перехватит причину

Заглушка молчит между шагами, а `LB_STREAM_STALL_ABORT_SEC` по умолчанию 300 с
(`stream_stall_r297.go:50-62`). При срабатывании сторож сам пришлёт кадры-причину
и отмену `upstream_stall` (`proxy_request_hijack.go:478-496`) — тест «пройдёт» по
чужой причине.

**Мера:** `LB_STREAM_STALL_ABORT_SEC=0`, `LB_STREAM_FIRST_FRAME_ABORT_SEC=0`
(второй — на случай будущих правок дефолта).

### 4.5. Утечка слота превращается в зависание пакета

`LB_ADMISSION_WAIT_SEC` по умолчанию 300 с (`admission_queue.go:59,87-100`), и
второй запрос молча ждал бы слот 5 минут (а потом пакет упал бы по таймауту без
внятной причины).

**Мера:** `LB_ADMISSION_WAIT_SEC=1` → 503 с телом «waited in queue and timed out»
(`admission_queue.go:847-879`) за ~1 с; плюс проверка `X-Queue-Wait-Ms`
(выставляется только при реальном ожидании, `admission_queue.go:806-815`).

### 4.6. Изоляция окружения

`t.Setenv` несовместим с `t.Parallel` (Go паникует), а переменные процесса
глобальны. **Мера:** тест не параллелится; в пакете нет `TestMain`, который
перебивал бы эти переменные (в `./tests` есть `r67a_testmain_test.go:32-37`, в
`./tests/llamacpp_proxy` — нет).

### 4.7. Платформенные тексты ошибок

Windows: `wsasend: An established connection was aborted by the software in your
host machine`; Linux: `broken pipe`/`connection reset`. **Мера:** тест нигде не
сравнивает текст ошибки — только факт из канала (`closedSeen`) и коды ответов.

### 4.8. Тайминги

Ни одно утверждение не зависит от «успело ли за N мс»: все ожидания —
наблюдаемые факты с дедлайном 5 с (запас 2000× от замеренных 1 мс). `time.Sleep`
есть только в `r340WaitFor` (шаг опроса 5 мс) и как `client.Timeout`/дедлайны
чтения — то есть как предохранитель, а не как синхронизация.

### 4.9. Шум фоновых петель и state file

`NewProxy` поднимает поллеры (метрики llama.cpp, image-resources, agent-checker);
в логе прогона видно `autosave error: open : The file cannot be found`
(`LoadBalancer.StatePath == ""`) — это безвредный шум, тест на него не опирается.
**Мера:** `t.Cleanup` гасит `SessionManager` и `QueueManager`; заглушка сначала
размораживается, потом закрывается (иначе `Close` ждёт зависшие обработчики).

### 4.10. Остаточные шаги между запросами

`step` — буферизованный канал (32). Лишние шаги, не потреблённые первым
запросом, может забрать второй — это безопасно: тесту нужен ровно один кадр.

---

## 5. Не проверено (и что нужно, чтобы проверить)

1. **Что клиент увидел в потоке после обрыва.** Клиента уже нет; байты после
   `Body.Close()` никто не читает. Кадры-причины `abortFramesR296` на этот путь
   не вызываются (§1.5) — если политика изменится («клиенту всё же нужен
   кадр-причина»), нужен отдельный тест с полу-живым клиентом (raw TCP, читаем
   до RST).
2. **Отсутствие `POST /api/cancel` на hijack-пути.** Сейчас воркеру уходит
   только закрытие сокета (§1.7). Зафиксировать это как контракт (или как
   дефект) можно тестом, который считает обращения к `/api/cancel` в заглушке;
   в скелете такого счётчика нет намеренно: утверждать «отмены нет» — значит
   закреплять текущее расхождение с NDJSON-путём (`llamacpp_transport.go:866`).
3. **Реальный воркер действительно прекращает генерацию.** Заглушка видит
   закрытие сокета; живой cppworker во время prefill может его игнорировать
   (ровно это и породило R184). Нужен тест на `cancelUpstreamGeneration` /
   `POST /api/cancel` против живой заглушки воркера.
4. **Обрыв ДО заголовков** (клиент ушёл, пока модель считала prefill): другой
   путь — `llamacpp_handlers_inference.go:352-400` и проверка, что не остаётся
   pending-строки admission (`admission_queue.go:796-803`).
5. **Legacy-путь без `Hijacker`** (`proxy_request.go:740`, HTTP/2, обёрнутый
   `ResponseWriter`): поведение иное (`proxy_request.go:909-915`), нужен тест с
   `httptest.NewRecorder`.
6. **Удаление сессии по TTL.** Проверяется только снятие флага; сама запись
   живёт до TTL (`SessionIdleTTL` в конфиге теста = 60 с).
7. **Дренаж при `Shutdown`.** Проверяется счётчик
   (`ActiveStreamsOnBackend == 0`), но не то, что `Shutdown`
   (`proxy.go:868-883`) возвращается быстро. Для этого нужен отдельный тест с
   `context.WithTimeout` вокруг `p.Shutdown`.
8. **Учёт токенов и метрики после обрыва.** `recordTokenUsage` вызывается на
   usage-чанке/финале; при обрыве его нет — проверять нечего, кроме «не выросли
   счётчики», что само по себе не инвариант.
9. **Поведение на живом стенде** (nginx/HTTP2, реальный cppworker): дизайн
   герметичен, стенд не требуется.
