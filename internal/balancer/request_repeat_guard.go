// request_repeat_r758.go — R758 (2026-11-25): ПОВТОРНЫЙ ЗАПРОС ТОЙ ЖЕ СЕССИИ.
//
// ЖИВАЯ ЖАЛОБА. «Зацикливание не в размышлениях модели, а по ответам: получен
// ответ, что модель разобралась и понимает, что куда, однако через некоторое
// время клиент получает СХОЖИЙ ПО СМЫСЛУ ответ — и так несколько раз. Также было
// замечено повторное исправление файлов».
//
// ЧТО ЭТО НА САМОМ ДЕЛЕ. Ответ модели детерминированно зависит от промпта: если
// клиент присылает ОДИН И ТОТ ЖЕ запрос повторно, модель повторно отвечает тем
// же смыслом — и агент повторно «исправляет» те же файлы. Второго ответа «из
// ниоткуда» не бывает: это ВТОРАЯ генерация по тому же телу запроса.
//
// ЖИВОЕ ПОДТВЕРЖДЕНИЕ ИЗ АРХИВА ВТОРОГО СТЕНДА (диагностика 06:25:56): одни и те
// же события повторялись счётчиками логгера —
//
//	R303: сессия залипла … repeated=6 first=06:11:21 last=06:20:00
//	R696: привязка сессии не создана … repeated=4 first=06:15:03 last=06:20:00
//	no available backends … repeated=7 first=06:19:08 last=06:25:09
//
// то есть ОДНА сессия ходила в кластер много раз подряд, а запросы шли по
// 68–153 с (first_byte_ms=68492 / 128000 / 153428), один из них получил 502.
// При таких задержках клиент (Cline/DSH) отваливается по своему таймауту и
// ПОВТОРЯЕТ ход — оператор видит «модель снова сказала то же самое».
//
// ЧТО ДЕЛАЕТ ЭТОТ ФАЙЛ. Считает повторы и пишет ОДНУ строку в лог с числами
// (сколько раз, за какой интервал, какой ход), чтобы причина была видна сразу.
// Поведение не меняется: ничего не блокируется и не переписывается — догадка
// «это клиент ретраит» должна подтверждаться, а не подменять запрос.
package balancer

import (
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"time"

	"ollama-loadbalancer/pkg/logger"
)

// repeatWindowR758 — в каком окне повтор считается повтором.
//
// 10 минут: живой архив показывает повторы на 06:12 → 06:22, то есть интервалы
// в несколько минут (клиент ждёт ответ 2–3 минуты, потом повторяет).
const repeatWindowR758 = 10 * time.Minute

// repeatLogEveryR758 — как часто повторно писать строку про один и тот же ход.
// Первый повтор и каждый следующий пишутся не чаще раза в минуту на ключ:
// иначе одна зацикленная сессия вытеснит из кольца логов всё остальное.
const repeatLogEveryR758 = time.Minute

// repeatEntryR758 — состояние одного «отпечатка» запроса.
type repeatEntryR758 struct {
	first      time.Time
	last       time.Time
	lastLog    time.Time
	model      string
	backendID  string
	sessionID  string
	count      int
	lastStatus int
}

// repeatTrackerR758 — карта отпечатков. Ноль-значение не используется: трекер
// создаётся лениво (см. proxyRepeatTrackerR758), чтобы не менять конструкторы
// Proxy (их много, в том числе в тестах).
type repeatTrackerR758 struct {
	m    map[string]*repeatEntryR758
	seen int
	mu   sync.Mutex
}

// requestFingerprintR758 — отпечаток «тот же ход»: сессия + модель + путь + тело.
//
// Тело включено намеренно: два РАЗНЫХ вопроса одной сессии не должны считаться
// повтором. Сравнение по отпечатку, а не по «похожести текста», потому что
// вопрос здесь один и тот же байт-в-байт (клиент повторяет ровно тот же запрос).
func requestFingerprintR758(sessionID, model, path string, body []byte) string {
	h := sha256.New()
	_, _ = h.Write([]byte(sessionID))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(model))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(path))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write(body)
	sum := h.Sum(nil)
	return hex.EncodeToString(sum[:12])
}

// repeatTrackersR758 — трекеры по прокси.
//
// ПОЧЕМУ ГЛОБАЛЬНАЯ КАРТА, А НЕ ПОЛЕ Proxy. Поле добавило бы 8 байт в Proxy и
// сдвинуло выравнивание всей структуры (это ловит fieldalignment в CI), а ради
// диагностического счётчика менять размер главного объекта не нужно. Ключ —
// указатель на прокси: в процессе он один, в тестах — свой на каждый Proxy, то
// есть тесты не мешают друг другу.
var repeatTrackersR758 sync.Map // map[*Proxy]*repeatTrackerR758

// proxyRepeatTrackerR758 — трекер этого прокси (создаётся при первом обращении).
func (p *Proxy) proxyRepeatTrackerR758() *repeatTrackerR758 {
	if p == nil {
		return nil
	}
	if v, ok := repeatTrackersR758.Load(p); ok {
		if tr, _ := v.(*repeatTrackerR758); tr != nil {
			return tr
		}
	}
	fresh := &repeatTrackerR758{m: make(map[string]*repeatEntryR758, 32)}
	got, _ := repeatTrackersR758.LoadOrStore(p, fresh)
	if tr, _ := got.(*repeatTrackerR758); tr != nil {
		return tr
	}
	return fresh
}

// noteRequestRepeatR758 — учесть запрос и, если это повтор, написать строку в лог.
//
// Возвращает (count, isRepeat): count — сколько раз этот же ход встречался в
// окне, включая текущий. isRepeat=true со ВТОРОГО раза.
func (p *Proxy) noteRequestRepeatR758(sessionID, model, backendID, path string, body []byte, status int) (int, bool) {
	tr := p.proxyRepeatTrackerR758()
	if tr == nil || len(body) == 0 {
		return 1, false
	}
	key := requestFingerprintR758(sessionID, model, path, body)
	now := time.Now()

	tr.mu.Lock()
	e := tr.m[key]
	if e == nil || now.Sub(e.first) > repeatWindowR758 {
		if tr.seen > 4096 {
			tr.pruneLockedR758(now)
		}
		tr.m[key] = &repeatEntryR758{
			first:      now,
			last:       now,
			count:      1,
			model:      model,
			backendID:  backendID,
			sessionID:  sessionID,
			lastStatus: status,
		}
		tr.seen++
		tr.mu.Unlock()
		return 1, false
	}
	e.count++
	e.last = now
	e.lastStatus = status
	if backendID != "" {
		e.backendID = backendID
	}
	count := e.count
	first, last, lastStatus := e.first, e.last, e.lastStatus
	shouldLog := now.Sub(e.lastLog) >= repeatLogEveryR758
	if shouldLog {
		e.lastLog = now
	}
	tr.mu.Unlock()

	if shouldLog && logger.Get() != nil {
		// Warn, а не Info: это объяснение «модель отвечает одно и то же» —
		// факт, который оператор ищет в логе, когда жалуется на зацикливание.
		logger.Get().Warnw("R758: клиент ПОВТОРЯЕТ тот же запрос (второй и следующий ответ модели — это новая генерация по тому же телу)",
			"session", sessionID, "model", model, "backend", backendID, "path", path,
			"repeat_count", count,
			"first_at", first.UTC().Format(time.RFC3339),
			"last_at", last.UTC().Format(time.RFC3339),
			"elapsed_sec", int(last.Sub(first).Seconds()),
			"last_status", lastStatus,
			"meaning", "клиент не дождался ответа (медленная генерация/очередь) и повторил ход; модель закономерно ответила тем же смыслом",
			"next_step", "проверьте first_byte_ms в request trace и why_queued: сначала убирается причина ожидания, а не «цикл» модели")
	}
	return count, count > 1
}

// pruneLockedR758 — выкинуть протухшие записи (вызывается под tr.mu).
func (tr *repeatTrackerR758) pruneLockedR758(now time.Time) {
	for k, e := range tr.m {
		if now.Sub(e.first) > repeatWindowR758 {
			delete(tr.m, k)
		}
	}
	tr.seen = len(tr.m)
}
