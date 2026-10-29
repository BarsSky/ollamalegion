package balancer

// auto_stream_keepalive_r272.go — R272 (2026-10-29): «клиент не получает данных
// и отваливается по таймауту», хотя модель генерирует.
//
// ЖИВАЯ ЖАЛОБА (второй стенд, клиент на OpenAI-порту 18079). Запрос приходит
// НЕ стриминговым (`stream` нет или `false`), поэтому балансер обслуживает его
// через auto-stream workaround: шлём апстриму stream=true, копим чанки и
// отдаём ЦЕЛЬНЫЙ JSON в конце (см. proxy_request_openai_auto_stream.go).
//
// ЧТО НЕ ТАК С ЭТИМ БЫЛО. Заголовки 200 отдавались сразу (chunked), а дальше
// клиент не получал НИ ОДНОГО БАЙТА до конца генерации: 27B-модель на
// CPU-offload пишет ответ минутами, и любой HTTP-клиент с таймаутом на чтение
// (а таких большинство) закрывает соединение раньше, чем мы закончим. Снаружи
// это выглядит как «порт мёртвый, хотя модель работает» — и никакой лог этого
// не объяснял, потому что header-фаза уже прошла успешно.
//
// ЧЕМ ЛЕЧИМ: пока копим ответ, периодически пишем в тело ПРОБЕЛЬНЫЙ байт.
// Это безопасно по контракту: JSON-текст по RFC 8259 — это `ws value ws`,
// то есть ведущие пробелы/переводы строк допустимы, и любой разборщик
// (json.Unmarshal, JSON.parse, SDK) их проглотит. Клиент видит живой поток
// данных и не рвёт соединение, а итоговый JSON остаётся валидным.
//
// ПОЧЕМУ НЕ «просто отдавать SSE»: клиент прислал stream=false и ждёт цельный
// JSON. Подменять контракт нельзя (это ломает строгие SDK и ломало уже —
// см. R258, где SSE отдаётся ТОЛЬКО если клиент сам прислал
// `Accept: text/event-stream`).
//
// ОТКЛЮЧЕНИЕ: LB_OPENAI_AUTO_STREAM_KEEPALIVE_SEC=0. Значение по умолчанию
// 10 с; меньше 2 с не имеет смысла (лишний трафик и шум в логах).

import (
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"ollama-loadbalancer/pkg/logger"
)

// autoStreamKeepaliveDefaultSec — период пробельного keepalive по умолчанию.
//
// 10 секунд выбраны так, чтобы пережить типовые клиентские таймауты на чтение
// (у aiohttp/httpx/undici по умолчанию десятки секунд, у самописных клиентов —
// от 15 с) и при этом не засорять поток: 6 байт в минуту.
const autoStreamKeepaliveDefaultSec = 10

// autoStreamKeepaliveMinSec — нижняя граница осмысленного периода.
const autoStreamKeepaliveMinSec = 2

// autoStreamKeepaliveInterval — период keepalive (0 = выключен).
//
// Мусор в переменной не выключает защиту: неизвестное значение трактуем как
// дефолт, иначе опечатка в .env тихо вернула бы дефект «клиент отваливается».
func autoStreamKeepaliveInterval() time.Duration {
	raw := strings.TrimSpace(os.Getenv("LB_OPENAI_AUTO_STREAM_KEEPALIVE_SEC"))
	if raw == "" {
		return autoStreamKeepaliveDefaultSec * time.Second
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return autoStreamKeepaliveDefaultSec * time.Second
	}
	if n <= 0 {
		return 0
	}
	if n < autoStreamKeepaliveMinSec {
		n = autoStreamKeepaliveMinSec
	}
	return time.Duration(n) * time.Second
}

// bodyKeepalive — периодическая запись пробельного байта в тело ответа.
//
// Зачем отдельная горутина, а не проверка в цикле чтения: цикл блокируется на
// чтении апстрима (scanner.Scan), и если воркер долго молчит (префилл большого
// контекста, тяжёлый первый токен), проверка «пора писать keepalive» просто не
// выполнится — ровно в тот момент, когда keepalive нужнее всего.
//
// ПОТОКОБЕЗОПАСНОСТЬ: net/http разрешает писать в ResponseWriter только из
// одного места одновременно. Поэтому Stop() сначала останавливает горутину и
// ДОЖИДАЕТСЯ её завершения, и только после этого вызывающий пишет финальное
// тело. Нарушение этого порядка дало бы гонку на запись и битый chunked-поток.
type bodyKeepalive struct {
	flusher http.Flusher
	write   func([]byte) (int, error)
	period  time.Duration
	stop    chan struct{}
	done    chan struct{}
	once    sync.Once
	written int
}

// startBodyKeepalive запускает keepalive, если период больше нуля.
//
// Возвращает nil, когда keepalive не нужен или невозможен (у writer'а нет
// http.Flusher — тогда периодическая запись бессмысленна: данные всё равно
// останутся в буфере и клиент их не увидит). Вызывающий обязан вызвать Stop()
// у ненулевого значения ПЕРЕД любой финальной записью.
func startBodyKeepalive(w http.ResponseWriter, period time.Duration, backendID, model string) *bodyKeepalive {
	if period <= 0 || w == nil {
		return nil
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		logger.Get().Debugw("auto-stream keepalive: writer has no Flusher, disabled",
			"backend", backendID, "model", model)
		return nil
	}
	k := &bodyKeepalive{
		flusher: flusher,
		write:   w.Write,
		period:  period,
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
	}
	go k.loop(backendID, model)
	return k
}

// loop пишет пробельный байт до остановки.
func (k *bodyKeepalive) loop(backendID, model string) {
	defer close(k.done)
	ticker := time.NewTicker(k.period)
	defer ticker.Stop()
	for {
		select {
		case <-k.stop:
			return
		case <-ticker.C:
			if _, err := k.write([]byte("\n")); err != nil {
				// Клиент ушёл — дальше писать некуда. Основной цикл узнает об
				// этом сам (контекст отменён), а нам достаточно замолчать.
				logger.Get().Debugw("auto-stream keepalive: write failed, stopping",
					"backend", backendID, "model", model, "error", err)
				return
			}
			k.flusher.Flush()
			k.written++
		}
	}
}

// Stop останавливает keepalive и дожидается горутины. Идемпотентен.
func (k *bodyKeepalive) Stop() {
	if k == nil {
		return
	}
	k.once.Do(func() {
		close(k.stop)
		<-k.done
	})
}

// KeepaliveWrites — сколько пробельных байт отправлено (для логов и тестов).
func (k *bodyKeepalive) KeepaliveWrites() int {
	if k == nil {
		return 0
	}
	return k.written
}
