// preflight_stream_dialog.go — Round 34 (2026-08-12) Phase 1: stream dialog
// with heartbeats during reload.
//
// Проблема: при async preflight reload (LB_NCTX_PREFLIGHT_ASYNC_RELOAD=true)
// balancer отдаёт 503+Retry-After для non-streaming клиентов, но для
// streaming клиентов (Cline, OpenWebUI) это плохой UX — клиент должен
// прервать stream и ретраить, ломая любой ongoing conversation.
//
// Решение: при PreflightAsyncReload И streaming клиент, balancer:
//  1. Открывает chunked SSE response (200, Content-Type: text/event-stream)
//  2. Spawn'ит goroutine которая пишет keepalive каждые 5 сек:
//     `data: {"object":"chat.completion.chunk","choices":[],"created":N,"model":M}\n\n`
//     Клиент получает "alive" сигнал, connection не таймаутится.
//  3. Sync ждёт завершения reload через NCtxReloadCoordinator.WaitReloadDone
//     (есть уже, dedup по backendID+modelName — параллельные reload'ы
//     коалесцируются).
//  4. После reload success — пишет [DONE] и закрывает connection. Клиент
//     может ретраить, увидит [DONE], и retry уже попадёт в NoOp preflight
//     (state fits после reload).
//
// Env: LB_PREFLIGHT_STREAM_DIALOG (default true) — opt-out для тестов.
//
//	LB_PREFLIGHT_STREAM_DIALOG_KEEPALIVE_SEC (default 5) — период keepalive.
package balancer

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"ollama-loadbalancer/pkg/logger"
)

// streamDialogConfig — настройки stream dialog mode. Читаются из env при
// каждом вызове (Round 34 не вводит config.json поле — env-only для
// быстрого rollback).
type streamDialogConfig struct {
	enabled         bool
	keepalivePeriod time.Duration
	waitTimeout     time.Duration
}

// getStreamDialogCfg — читает env каждый раз (env-only config).
func getStreamDialogCfg() streamDialogConfig {
	cfg := streamDialogConfig{
		enabled:         true, // default ON
		keepalivePeriod: 5 * time.Second,
		// R83/v62 (2026-10-02): кап 5 минут на ожидание перезагрузки убран.
		//
		// 0 = ждать ТЕРМИНАЛЬНОГО состояния reload (WaitReloadDone(...,0) ждёт
		// <-found.done). Раньше по истечении 5 минут диалог закрывался БЕЗ ответа
		// модели — ровно тот случай, который в preflight_nctx.go описан как
		// «Cline показывал пустой ответ, и проходил только повторный запрос».
		// Числовое значение можно вернуть через env (см. ниже), и тогда оно
		// печатается в лог как осознанный кап оператора.
		waitTimeout: 0,
	}
	if v, ok := os.LookupEnv("LB_PREFLIGHT_STREAM_DIALOG"); ok {
		switch v {
		case "false", "0", "no":
			cfg.enabled = false
		case "true", "1", "yes":
			cfg.enabled = true
		}
	}
	if v, ok := os.LookupEnv("LB_PREFLIGHT_STREAM_DIALOG_KEEPALIVE_SEC"); ok {
		var sec int
		if _, err := fmt.Sscanf(v, "%d", &sec); err == nil && sec > 0 {
			cfg.keepalivePeriod = time.Duration(sec) * time.Second
		}
	}
	return cfg
}

// IsStreamDialogEnabled — геттер для тестов / opt-out проверки.
func IsStreamDialogEnabled() bool {
	return getStreamDialogCfg().enabled
}

// runInferencePreflightStreamDialog — для streaming клиента при
// PreflightAsyncReload decision'е, держит connection open и пишет
// keepalive каждые 5 сек пока async reload в фоне завершается.
//
// Returns true если stream dialog был использован (handled=true).
//
// ОГРАНИЧЕНИЯ:
//   - Только streaming clients (isStreamingFromBody).
//   - lb_preflight_async_reload=true.
//   - Не пытается рекурсивно stream-dialog (если после reload params
//     снова не match — fallback на обычный 503+Retry-After через preflight).
func (lr *LlamaCppRouter) runInferencePreflightStreamDialog(
	args runInferencePreflightArgs,
	decision PreflightDecision,
	res *PreflightResult,
) bool {
	cfg := getStreamDialogCfg()
	if !cfg.enabled {
		return false
	}
	// R253.1: запрос уже прошёл диалог (наш же self-proxy) — второй раз не входим.
	// Без этого self-proxy попадал бы в собственный диалог и получался бы цикл.
	if isInternalPreflightDialogRequest(args.r) {
		logger.Get().Debugw("preflight stream dialog: served-маркер уже стоит, пропускаем",
			"backend", args.backendID)
		return false
	}
	if decision != PreflightAsyncReload {
		return false
	}
	if !isStreamingFromBody(args.r.URL.Path, args.body) {
		logger.Get().Debugw("preflight stream dialog: client is not streaming, skipping (will return 503)",
			"backend", args.backendID, "path", args.r.URL.Path, "body_len", len(args.body))
		return false
	}
	if lr == nil || lr.proxy == nil || lr.proxy.nctxReload == nil {
		return false
	}
	if res == nil || res.TargetNCtx <= 0 {
		return false
	}

	ctx := args.r.Context()
	waitTimeout := cfg.waitTimeout
	if h, ok := ctx.Deadline(); ok {
		if remaining := time.Until(h); remaining > 0 && remaining < waitTimeout {
			waitTimeout = remaining
		}
	}

	modelName := args.model

	logger.Get().Infow("preflight stream dialog: starting keepalive during async reload",
		"backend", args.backendID, "model", modelName,
		"target_n_ctx", res.TargetNCtx, "keepalive_period", cfg.keepalivePeriod,
		"wait_timeout", waitTimeout)

	// Open stream headers via hijack (manual HTTP/1.1 chunked response).
	hijacker, ok := args.w.(http.Hijacker)
	if !ok {
		// Can't hijack (HTTP/2, etc) — fallback to plain flush.
		return lr.runInferencePreflightStreamDialogPlain(args, res, modelName, cfg)
	}
	conn, bufrw, err := hijacker.Hijack()
	if err != nil {
		logger.Get().Warnw("preflight stream dialog: hijack failed",
			"backend", args.backendID, "error", err)
		return false
	}
	defer conn.Close()

	// Write 200 OK + headers manually.
	if err := writeStreamDialogHeaders(bufrw, args.r); err != nil {
		logger.Get().Warnw("preflight stream dialog: write headers failed",
			"backend", args.backendID, "error", err)
		return false
	}

	// Keepalive-горутина: живёт до ПЕРВОГО БАЙТА ответа модели (R253.1), а не до
	// конца reload. Между reload и первым токеном клиент тоже не должен молчать:
	// первый байт может прийти через минуты ожидания слота в admission-очереди.
	keepaliveCtx, cancelKeepalive := context.WithCancel(ctx)
	defer cancelKeepalive()
	var keepaliveWG sync.WaitGroup
	keepaliveWG.Add(1)
	go func() {
		defer keepaliveWG.Done()
		streamDialogKeepaliveLoop(bufrw, keepaliveCtx, args.backendID, modelName, res.TargetNCtx, cfg)
	}()

	// Wait for async reload to complete (or context canceled).
	reloadErr := lr.proxy.nctxReload.WaitReloadDone(args.backendID, modelName, waitTimeout)
	if reloadErr != nil {
		logger.Get().Warnw("preflight stream dialog: reload wait failed/timed out",
			"backend", args.backendID, "model", modelName, "error", reloadErr,
			"wait_timeout", waitTimeout)
		// R253.1: error-кадр + ровно один [DONE] + chunked-терминатор. Раньше здесь
		// оставался только error-кадр, и strict-клиенты получали оборванный поток.
		writeChunkedStreamError(bufrw, fmt.Sprintf(
			"reload timed out after %s, please retry", waitTimeout))
		_ = writeChunkedFrame(bufrw, nil)
		return true
	}

	logger.Get().Infow("preflight stream dialog: reload done, serving the same request",
		"backend", args.backendID, "model", modelName, "target_n_ctx", res.TargetNCtx)

	// R253 (2026-10-28): НЕ закрываем поток пустым [DONE].
	//
	// ЖИВОЙ ДЕФЕКТ. Раньше диалог писал keepalive-JSON-чанки, затем `data: [DONE]`
	// и закрывал соединение — БЕЗ ответа модели. Клиент (Cline) считал ход
	// завершённым пустым сообщением и останавливал задачу: «после порога сессия
	// закрывается». Теперь после reload тот же запрос дообслуживается через
	// собственную поверхность (self-proxy), кадры ответа уходят клиенту в то же
	// соединение, и ровно один [DONE] закрывает поток.
	// Keepalive гасим на первом байте ответа (stopKeepalive), а после возврата —
	// добиваем гарантированно, чтобы горутина не пережила соединение.
	streamSelfProxyChunked(ctx, args.r, bufrw, args.body, res.TargetNCtx, func() {
		cancelKeepalive()
		keepaliveWG.Wait()
	})
	cancelKeepalive()
	keepaliveWG.Wait()
	return true
}

// preflightDialogServedHeader — маркер «запрос уже прошёл диалог».
//
// Внутренний (self-proxy) запрос несёт его, чтобы не запускать диалог второй раз
// (после reload preflight и так должен дать NoOp, но полагаться на это нельзя:
// метрики поллера обновляются позже, и тогда получился бы цикл перезагрузок).
//
// R253.1: значение — ПЕР-ПРОЦЕССНЫЙ случайный токен, а не константа. С константой
// клиент мог прислать её сам и отключить рост окна (и любой служебный механизм
// диалога). Заголовок снаружи вырезается в ServeHTTP (stripExternalDialogMarker),
// поэтому подделать его нельзя даже угадав имя.
const preflightDialogServedHeader = "X-LB-Preflight-Dialog-Served"

// preflightDialogToken — случайный токен процесса (генерируется один раз).
var preflightDialogToken = newPreflightDialogToken()

func newPreflightDialogToken() string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		// crypto/rand недоступен — берём время+адрес как минимум энтропии.
		return fmt.Sprintf("%d-%d", time.Now().UnixNano(), os.Getpid())
	}
	return hex.EncodeToString(buf)
}

// isLoopbackRemote — запрос пришёл с этой же машины (наш self-proxy).
func isLoopbackRemote(r *http.Request) bool {
	if r == nil {
		return false
	}
	host, _, err := net.SplitHostPort(strings.TrimSpace(r.RemoteAddr))
	if err != nil {
		host = strings.TrimSpace(r.RemoteAddr)
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// isInternalPreflightDialogRequest — это наш собственный self-proxy-запрос?
//
// Требуется И правильный токен, И loopback-источник: одно лишь совпадение
// заголовка не должно открывать служебную ветку.
func isInternalPreflightDialogRequest(r *http.Request) bool {
	if r == nil {
		return false
	}
	got := r.Header.Get(preflightDialogServedHeader)
	if got == "" || got != preflightDialogToken {
		return false
	}
	return isLoopbackRemote(r)
}

// stripExternalDialogMarker — убрать маркер из ВНЕШНЕГО запроса.
//
// Вызывается в ServeHTTP до любой обработки: клиент не должен уметь ни отключить
// рост окна, ни притвориться внутренним запросом. Наш self-proxy приходит с
// loopback и правильным токеном — его не трогаем.
func stripExternalDialogMarker(r *http.Request) {
	if r == nil || r.Header.Get(preflightDialogServedHeader) == "" {
		return
	}
	if isInternalPreflightDialogRequest(r) {
		return
	}
	r.Header.Del(preflightDialogServedHeader)
}

// streamSelfProxyChunked — дообслуживание запроса после reload для hijack-пути.
//
// ПОЧЕМУ self-proxy, а не «продолжить обработчик». Диалог уже hijack'нул
// соединение, поэтому вернуть управление обычному пути нельзя (он писал бы
// заголовки в занятый conn). Единственный хирургический вариант — прогнать ТОТ ЖЕ
// запрос через собственную OpenAI-поверхность (тот же pipeline: выбор бэкенда,
// очередь, нормализация) и перелить кадры в уже открытое соединение.
//
// stopKeepalive вызывается на ПЕРВОМ записанном кадре (R253.1): до этого клиент
// видит `: keepalive`, после — настоящие кадры ответа.
func streamSelfProxyChunked(ctx context.Context, incoming *http.Request, bufrw *bufio.ReadWriter, body []byte, targetNCtx int, stopKeepalive func()) {
	if incoming == nil || bufrw == nil {
		return
	}
	// R253.1 / БЛОКЕР 1: chunked-ТЕРМИНАТОР обязателен на ЛЮБОМ исходе.
	//
	// Заголовки объявляют Transfer-Encoding: chunked (writeStreamDialogHeaders), а
	// `0\r\n\r\n` писала только штатная SSE-петля. Без терминатора strict-клиенты
	// (Cline/undici/aiohttp) получают «Not enough data to satisfy transfer length
	// header» — ровно класс обрыва, который R253 и лечит.
	defer func() {
		if err := writeChunkedFrame(bufrw, nil); err != nil {
			logger.Get().Debugw("preflight stream dialog: chunked terminator write failed",
				"path", incoming.URL.Path, "error", err)
		}
	}()

	var firstByte sync.Once
	onFirstByte := func() {
		firstByte.Do(func() {
			if stopKeepalive != nil {
				stopKeepalive()
			}
		})
	}

	resp, err := selfProxyPostWithMarker(ctx, incoming, incoming.URL.RequestURI(), body)
	if err != nil {
		logger.Get().Warnw("preflight stream dialog: self-proxy after reload failed",
			"path", incoming.URL.Path, "target_n_ctx", targetNCtx, "error", err)
		writeChunkedStreamError(bufrw, "reload done, but serving the request failed: "+err.Error())
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg := fmt.Sprintf("reload done, but upstream answered HTTP %d", resp.StatusCode)
		logger.Get().Warnw("preflight stream dialog: self-proxy non-2xx after reload",
			"path", incoming.URL.Path, "status", resp.StatusCode)
		writeChunkedStreamError(bufrw, msg)
		return
	}

	// R256: кадры внутреннего ответа проходят через ТОТ ЖЕ фильтр reasoning, что и
	// обычный путь. Раньше они переливались как есть — размышления и маркеры
	// уезжали клиенту мимо фильтра (живая жалоба «мешаются размышления»).
	//
	// R264: здесь же ЗАКРЫВАЕМ выход фильтра (finalize). Без этого на этом пути не
	// срабатывало объяснение пустого ответа (R261) и мог теряться удержанный хвост,
	// хотя на трёх остальных путях R261 уже работал: стрим, целиком ушедший в
	// reasoning, отдавался клиенту как молчаливая пустота.
	egress := &reasoningEgressState{}
	last, copyErr := copySSELines(resp.Body, func(raw []byte) error {
		onFirstByte()
		line := raw
		if isOpenAISSEDataLine(line) {
			line = append(line, '\n')
		}
		return selfProxyEgressLine(line, egress, func(fr []byte) error {
			return writeChunkedFrame(bufrw, fr)
		})
	})
	if copyErr != nil {
		logger.Get().Warnw("preflight stream dialog: copy after reload failed",
			"path", incoming.URL.Path, "error", copyErr)
		writeChunkedStreamError(bufrw, "stream copy failed: "+copyErr.Error())
		return
	}
	onFirstByte()
	// R264: закрываем выход фильтра и здесь — на случай потока, кончившегося без
	// [DONE] (обрыв/край finish_reason): хвост и объяснение не должны потеряться.
	if err := writeEgressFinalizeFrames(egress, &last, func(fr []byte) error {
		return writeChunkedFrame(bufrw, fr)
	}); err != nil {
		logger.Get().Warnw("preflight stream dialog: finalize frame write failed",
			"path", incoming.URL.Path, "error", err)
		return
	}
	ensureChunkedSSEDone(bufrw, last)
}

// streamSelfProxyPlain — то же для пути без hijack (обычный ResponseWriter).
func streamSelfProxyPlain(ctx context.Context, incoming *http.Request, w http.ResponseWriter, body []byte, targetNCtx int, stopKeepalive func()) {
	if incoming == nil || w == nil {
		return
	}
	var firstByte sync.Once
	onFirstByte := func() {
		firstByte.Do(func() {
			if stopKeepalive != nil {
				stopKeepalive()
			}
		})
	}
	resp, err := selfProxyPostWithMarker(ctx, incoming, incoming.URL.RequestURI(), body)
	if err != nil {
		logger.Get().Warnw("preflight stream dialog (plain): self-proxy after reload failed",
			"path", incoming.URL.Path, "target_n_ctx", targetNCtx, "error", err)
		writePlainStreamError(w, "reload done, but serving the request failed: "+err.Error())
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		writePlainStreamError(w, fmt.Sprintf("reload done, but upstream answered HTTP %d", resp.StatusCode))
		return
	}
	// R256: тот же фильтр reasoning, что и на остальных путях.
	// R264: и то же закрытие выхода фильтра (finalize) — см. комментарий в
	// streamSelfProxyChunked: без него объяснение пустого ответа (R261) терялось.
	egress := &reasoningEgressState{}
	last, copyErr := copySSELines(resp.Body, func(raw []byte) error {
		onFirstByte()
		return selfProxyEgressLine(raw, egress, func(fr []byte) error {
			if _, werr := w.Write(fr); werr != nil {
				return werr
			}
			Flush(w)
			return nil
		})
	})
	if copyErr != nil {
		writePlainStreamError(w, "stream copy failed: "+copyErr.Error())
		return
	}
	onFirstByte()
	if err := writeEgressFinalizeFrames(egress, &last, func(fr []byte) error {
		if _, werr := w.Write(fr); werr != nil {
			return werr
		}
		Flush(w)
		return nil
	}); err != nil {
		logger.Get().Warnw("preflight stream dialog (plain): finalize frame write failed",
			"path", incoming.URL.Path, "error", err)
		return
	}
	ensurePlainSSEDone(w, last)
}

// selfProxyEgressLine — R264: обработка ОДНОЙ строки внутреннего ответа на путях
// self-proxy (chunked и plain).
//
// Одна функция, потому что раньше эти два пути дублировали друг друга копипастой,
// и оба забыли закрытие выхода фильтра: объяснение пустого ответа (R261) не
// срабатывало, а удержанный хвост мог потеряться. Правило порядка то же, что и на
// основном пути: объяснение → придержанный finish → [DONE].
func selfProxyEgressLine(line []byte, egress *reasoningEgressState, write func([]byte) error) error {
	if write == nil {
		return nil
	}
	if len(line) == 0 || (len(line) == 1 && line[0] == '\n') {
		return nil
	}
	if filtered, _ := filterAssistantEgressForClient(line, egress); filtered != nil {
		line = filtered
	}
	// R261: finish-кадр придерживаем — объяснение обязано идти ПЕРЕД ним.
	if egress.tryHoldFinishFrame(line) {
		return nil
	}
	// R261: [DONE] — последняя точка, где ещё можно отдать придержанное. Иначе
	// объяснение и finish окажутся после [DONE], и клиент увидит два конца потока.
	if isSSEDoneLine(line) {
		if err := writeEgressFinalizeFrames(egress, nil, write); err != nil {
			return err
		}
	}
	return write(line)
}

// writeEgressFinalizeFrames — R264: общий способ отдать кадры закрытия выхода
// фильтра размышлений (сброс удержанного хвоста → объяснение пустого ответа →
// придержанный finish).
//
// Зачем отдельная функция: пути, которые переливают поток «как есть» (self-proxy
// диалога preflight'а), обязаны закрывать выход фильтра так же, как основной
// путь. Пока это делалось копиями кода, один из путей забыл вызов — и R261 на нём
// не работал (живой провал на стенде r261: кадр-объяснение не приходил).
// last обновляется, чтобы вызывающий корректно решил, нужен ли синтетический
// [DONE].
func writeEgressFinalizeFrames(egress *reasoningEgressState, last *[]byte, write func([]byte) error) error {
	if egress == nil || write == nil {
		return nil
	}
	for _, fr := range egress.finalizeFrames() {
		// R277: та же граница SSE-события, что и на остальных путях.
		fr = withSSEEventBoundary(fr)
		if err := write(fr); err != nil {
			return err
		}
		if last != nil {
			*last = append((*last)[:0], fr...)
		}
	}
	return nil
}

// copySSELines — построчное копирование SSE-потока через write.
//
// Возвращает последнюю УСПЕШНО записанную строку: по ней вызывающий решает, нужен
// ли синтетический [DONE] (ensure*SSEDone).
func copySSELines(src io.Reader, write func([]byte) error) ([]byte, error) {
	if src == nil || write == nil {
		return nil, nil
	}
	var last []byte
	scanner := bufio.NewScanner(src)
	// R264: лимит строки берём общий (R259) — в этом файле оставался свой,
	// жёсткий 4 МиБ, и большой кадр обрывал копирование молча.
	scanner.Buffer(make([]byte, 0), streamMaxLineBytes())
	for scanner.Scan() {
		line := append([]byte(nil), scanner.Bytes()...)
		line = append(line, '\n')
		if err := write(line); err != nil {
			return last, err
		}
		last = line
	}
	return last, scanner.Err()
}

// selfProxyPostWithMarker — self-proxy с маркером «диалог уже был».
func selfProxyPostWithMarker(ctx context.Context, incoming *http.Request, requestURI string, body []byte) (*http.Response, error) {
	return selfProxyPostRequest(ctx, incoming, requestURI, body, preflightDialogToken)
}

// selfProxyForwardHeaders — заголовки, которые ОБЯЗАНЫ доехать до внутреннего
// запроса.
//
// R253.1 / БЛОКЕР 2: раньше пробрасывались только токен и User-Agent, поэтому
// внутренний запрос получал sessionID от 127.0.0.1: в панели появлялась ВТОРАЯ
// сессия, кошелёк admission-очереди делился, монитор показывал loopback, а
// X-Session-ID/X-Client-ID клиента терялись (регресс R252/R118.9). Cookie нужен
// для клиентов, которые идентифицируют сессию печенькой.
var selfProxyForwardHeaders = []string{
	"X-API-Token", "Authorization",
	"X-Client-ID", "X-Session-ID", "X-Tab-ID", "X-Request-ID",
	"X-User-Id", "X-OpenWebUI-User-Id",
	"Cookie",
	"X-Forwarded-For", "X-Real-IP",
	"User-Agent", "Accept",
}

// selfProxyPostRequest — общий self-proxy: проброс заголовков + опциональный
// служебный маркер ("" = без маркера).
func selfProxyPostRequest(ctx context.Context, incoming *http.Request, requestURI string, body []byte, marker string) (*http.Response, error) {
	url := selfProxyBaseURL(incoming) + requestURI
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	for _, header := range selfProxyForwardHeaders {
		if v := incoming.Header.Get(header); v != "" {
			req.Header.Set(header, v)
		}
	}
	if marker != "" {
		req.Header.Set(preflightDialogServedHeader, marker)
	}
	return selfProxyClient.Do(req)
}

// runInferencePreflightStreamDialogPlain — fallback когда hijack недоступен.
// Использует Go's standard ResponseWriter с chunked transfer encoding.
func (lr *LlamaCppRouter) runInferencePreflightStreamDialogPlain(
	args runInferencePreflightArgs,
	res *PreflightResult,
	modelName string,
	cfg streamDialogConfig,
) bool {
	w := args.w
	ctx := args.r.Context()

	// R253.1 / ПРАВКА 4: тот же маркер, что в hijack-ветке — иначе self-proxy мог
	// повторно войти в plain-диалог (self-proxy → plain-диалог → self-proxy).
	if isInternalPreflightDialogRequest(args.r) {
		logger.Get().Debugw("preflight stream dialog (plain): served-маркер уже стоит, пропускаем",
			"backend", args.backendID)
		return false
	}

	// Set headers for SSE.
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher, ok := w.(http.Flusher)
	if !ok {
		// No flusher — close connection.
		return false
	}

	// Keepalive-тикер живёт до ПЕРВОГО БАЙТА ответа модели (R253.1): между reload и
	// первым токеном клиент тоже не должен молчать.
	keepaliveCtx, cancelKeepalive := context.WithCancel(ctx)
	defer cancelKeepalive()
	var keepaliveWG sync.WaitGroup
	keepaliveWG.Add(1)
	go func() {
		defer keepaliveWG.Done()
		ticker := time.NewTicker(cfg.keepalivePeriod)
		defer ticker.Stop()
		for {
			select {
			case <-keepaliveCtx.Done():
				return
			case <-ticker.C:
				if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
					return
				}
				flusher.Flush()
			}
		}
	}()
	stopKeepalive := func() {
		cancelKeepalive()
		keepaliveWG.Wait()
	}

	// Wait for reload OR keepalive fires.
	reloadDone := make(chan struct{})
	go func() {
		if err := lr.proxy.nctxReload.WaitReloadDone(args.backendID, modelName, cfg.waitTimeout); err == nil {
			close(reloadDone)
		}
	}()

	for {
		select {
		case <-reloadDone:
			// R253 (2026-10-28): после reload дообслуживаем тот же запрос, а не
			// закрываем поток пустым маркером (клиент считал ход завершённым без
			// ответа модели).
			logger.Get().Infow("preflight stream dialog (plain): reload done, serving the same request",
				"backend", args.backendID, "model", modelName, "target_n_ctx", res.TargetNCtx)
			streamSelfProxyPlain(ctx, args.r, w, args.body, res.TargetNCtx, stopKeepalive)
			stopKeepalive()
			return true
		case <-ctx.Done():
			stopKeepalive()
			fmt.Fprintf(w, ": client_canceled\n\n")
			flusher.Flush()
			return true
		}
	}
}

// writeStreamDialogHeaders — write HTTP/1.1 200 OK + SSE headers.
func writeStreamDialogHeaders(bufrw *bufio.ReadWriter, r *http.Request) error {
	proto := "HTTP/1.1"
	if r.ProtoMajor == 2 {
		proto = "HTTP/2.0"
	}
	headers := []string{
		proto + " 200 OK",
		"Content-Type: text/event-stream",
		"Cache-Control: no-cache",
		"Connection: keep-alive",
		"X-Accel-Buffering: no",
		"X-Round-34-Stream-Dialog: keepalive-during-reload",
		"",
		"",
	}
	for _, h := range headers {
		if _, err := bufrw.WriteString(h + "\r\n"); err != nil {
			return err
		}
	}
	return bufrw.Flush()
}

// streamDialogKeepaliveLoop — пишет keepalive каждые keepalivePeriod до cancel.
func streamDialogKeepaliveLoop(
	bufrw *bufio.ReadWriter,
	ctx context.Context,
	backendID, modelName string,
	targetNCtx int,
	cfg streamDialogConfig,
) {
	start := time.Now()
	ticker := time.NewTicker(cfg.keepalivePeriod)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// R253: SSE-комментарий вместо JSON-чанка (см. выше).
			if _, err := bufrw.WriteString(": keepalive\n\n"); err != nil {
				return
			}
			if err := bufrw.Flush(); err != nil {
				return
			}
			logger.Get().Debugw("preflight stream dialog: keepalive sent",
				"backend", backendID, "elapsed_ms", time.Since(start).Milliseconds())
		}
	}
}

// suppress unused import warnings if conn is referenced in future refactor.
var _ net.Conn
