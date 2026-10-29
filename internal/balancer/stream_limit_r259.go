package balancer

// stream_limit_r259.go — R259 (2026-10-28): не обвинять upstream в наших лимитах
// и отменах.
//
// ЖИВАЯ ЖАЛОБА (Cline, периодически): «stream truncated by upstream before
// completion». Этот текст — наш собственный (truncation-detection в
// llamacpp_transport.go), то есть балансер сам решил, что upstream не прислал
// терминальный маркер. Часть таких срабатываний — ЛОЖНЫЕ:
//
//  1. ЛИМИТ СТРОКИ. Сканер объявлен как `scanner.Buffer(make([]byte,0), 1 МиБ)`.
//     Если один SSE-кадр больше лимита (большой tool-call аргумент, длинная
//     строка кода), Scan() возвращает bufio.ErrTooLong, цикл выходит,
//     streamCompleted остаётся false — и клиент получает «upstream truncated»,
//     хотя upstream жив и данные просто не влезли в НАШ буфер.
//  2. НАШИ ОТМЕНЫ. Отмена клиента или наша перезагрузка посреди стрима — это не
//     «upstream оборвался».
//
// Здесь: конфигурируемый лимит (LB_STREAM_MAX_LINE_BYTES, дефолт 8 МиБ) и
// классификация причины обрыва, чтобы текст клиенту и лог говорили правду.

import (
	"context"
	"errors"
	"strings"

	"ollama-loadbalancer/internal/settings"
)

// streamMaxLineBytesDefault — дефолтный лимит одной строки SSE.
//
// 8 МиБ: один кадр с аргументами tool-call реально бывает в сотни килобайт
// (правка файла, base64), а прежний 1 МиБ рвался на них.
const streamMaxLineBytesDefault = 8 << 20

// streamMaxLineBytes — лимит одной строки SSE.
//
// R278: значение берётся из реестра настроек (override из WebUI → env →
// дефолт), поэтому меняется без рестарта. Лояльность к мусору осталась в
// settings.NormalizeEnv: меньше 64 КиБ и нечисловое значение → дефолт.
func streamMaxLineBytes() int {
	if v, ok := settings.HotInt("LB_STREAM_MAX_LINE_BYTES"); ok {
		return int(v)
	}
	return streamMaxLineBytesDefault
}

// Причины завершения стрима (R259). Значения идут в лог и в текст клиенту,
// поэтому «upstream_truncated» больше не приписывается нашим лимитам/отменам.
const (
	streamAbortNone           = ""                // стрим завершён штатно
	streamAbortOurLimit       = "our_limit"       // наш буфер строки
	streamAbortClientCancel   = "client_cancel"   // клиент ушёл
	streamAbortBalancerCancel = "balancer_cancel" // наша отмена/таймаут
	streamAbortUpstreamEOF    = "upstream_eof"    // upstream закрыл поток без маркера
	streamAbortUpstreamError  = "upstream_error"  // ошибка чтения upstream
	// R271 (2026-10-29): поток оборвался потому, что МЫ выгрузили модель под ним
	// (перезагрузка по запросу другого клиента с другим окном n_ctx). Это не
	// «upstream оборвался» — воркер жив, модель выгрузил балансер.
	streamAbortBalancerReload = "balancer_reload"
	// R280 (2026-10-29): то же, но поводом была авто-перезагрузка AutoTune
	// (kv-cache / контекст / слои). Отдельная причина, чтобы оператор видел
	// РЕАЛЬНЫЙ повод, а не «reload for another request».
	streamAbortBalancerAutotune = "balancer_autotune"
)

// isLineTooLongErr — ошибка «строка не влезла в наш буфер»?
//
// bufio не экспортирует ErrTooLong (у него нет такого имени), поэтому надёжен
// только текст ошибки: "bufio.Scanner: token too long". Ошибка не оборачивается
// нами, но текст-проверка переживает и обёртку.
func isLineTooLongErr(err error) bool {
	if err == nil {
		return false
	}

	return strings.Contains(err.Error(), "token too long")
}

// streamAbortReasonR259 — КТО завершил стрим. Порядок проверок важен:
// наш лимит → наша/клиентская отмена → ошибка чтения → чистый EOF.
func streamAbortReasonR259(scannerErr error, reqCtxErr error) string {
	if isLineTooLongErr(scannerErr) {
		return streamAbortOurLimit
	}
	if reqCtxErr != nil {
		if errors.Is(reqCtxErr, context.Canceled) {
			return streamAbortClientCancel
		}
		// Deadline/cancel нашего контекста (таймаут стрима, перезагрузка).
		return streamAbortBalancerCancel
	}
	if scannerErr != nil {
		return streamAbortUpstreamError
	}
	return streamAbortUpstreamEOF
}

// streamAbortBlamesUpstream — виноват ли upstream в этом исходе.
func streamAbortBlamesUpstream(reason string) bool {
	return reason == streamAbortUpstreamEOF || reason == streamAbortUpstreamError
}

// streamAbortClientMessage — что показать клиенту (не врём про upstream).
func streamAbortClientMessage(reason string) string {
	switch reason {
	case streamAbortOurLimit:
		return "stream line exceeded the balancer buffer limit; the response was cut here"
	case streamAbortBalancerCancel:
		return "stream cancelled by the balancer (reload/timeout); please retry"
	case streamAbortBalancerReload:
		// R271: честная причина — модель под этим стримом выгрузил балансер,
		// потому что другой клиент запросил другое окно контекста.
		return "stream stopped because the balancer reloaded the model for another request; please retry"
	case streamAbortBalancerAutotune:
		// R280: тот же обрыв, но повод — авто-перезагрузка AutoTune с новыми
		// параметрами. Клиент не должен думать, что виноват upstream/модель.
		return "stream stopped because the balancer reloaded the model (autotune); please retry"
	case streamAbortClientCancel:
		return "stream cancelled by the client"
	default:
		return "stream ended by upstream before completion"
	}
}
