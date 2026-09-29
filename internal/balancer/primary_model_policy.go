// primary_model_policy.go — R83 (2026-09-29).
//
// ПРИМАРИ-РЕЖИМ: администратор фиксирует параметры модели, клиентский запрос их не
// меняет.
//
// ЗАЧЕМ. До этой правки любой клиент (Cline, OpenWebUI) мог прислать свой num_ctx
// больше загруженного — и балансер МОЛЧА перезагружал модель на этот контекст:
//
//	preflightNCtxReload: detected n_ctx mismatch, scheduling async reload,
//	  loaded_n_ctx: 16384, requested_n_ctx: 32768
//
// Администратор выставил 16K в WebUI, получил 32K и узнал об этом только по
// расходу VRAM. Ответ клиенту был 200, то есть «всё хорошо».
//
// Теперь у профиля модели есть флаг primary. При primary=true:
//   - запрос, которому хватает зафиксированного контекста, обслуживается —
//     num_ctx в теле подменяется на фактически загруженный;
//   - запрос, которому нужно больше, получает 413 с явным объяснением
//     «настройки зафиксированы администратором» и цифрами (загружено / нужно);
//   - AutoTune для такой модели не запускается (см. IsAutoTuneEnabled).
//
// ВАЖНО: primary не блокирует ЯВНУЮ загрузку модели из WebUI
// (/api/models/load-with-params) — администратор по-прежнему может поменять
// параметры, это и есть «правка через администратора» из текста отказа.
package balancer

import (
	"fmt"
	"net/http"

	"ollama-loadbalancer/pkg/logger"
)

// isPrimaryModel — true, если у модели выставлен флаг primary в профиле.
//
// Сравнение имён — тем же помощником, что и в остальных местах (модель может
// прийти как «qwen3.8:latest», а профиль называться «Qwen3.8-27B»).
func (p *Proxy) isPrimaryModel(modelName string) bool {
	if p == nil || modelName == "" {
		return false
	}
	prof, ok := p.GetModelProfile(modelName)
	if !ok {
		return false
	}
	return prof.Primary
}

// primaryFixedNCtx — зафиксированный администратором контекст модели.
//
// Источники по приоритету:
//  1. фактически загруженный n_ctx (это и есть то, что «зафиксировано» сейчас);
//  2. profile.contextLength — если модель ещё не загружена, но профиль задаёт ctx.
//
// 0 = неизвестно (тогда политика не вмешивается и запрос идёт обычным путём).
func (p *Proxy) primaryFixedNCtx(backendID, modelName string) int {
	if p == nil {
		return 0
	}
	if p.nctxReload != nil {
		if n := p.nctxReload.LastKnownNCtx(backendID); n > 0 {
			return n
		}
	}
	if prof, ok := p.GetModelProfile(modelName); ok && prof.ContextLength > 0 {
		return prof.ContextLength
	}
	return 0
}

// applyPrimaryModelPolicy — решает судьбу клиентского запроса при primary=true.
//
// Возвращает тот же контракт, что и preflightNCtxReloadIfNeeded:
// (body, needsProxy, errMsg, statusCode).
func (p *Proxy) applyPrimaryModelPolicy(backendID, modelName string, bodyBuf []byte, requestPath string) ([]byte, bool, string, int) {
	fixed := p.primaryFixedNCtx(backendID, modelName)
	if fixed <= 0 {
		// Не знаем зафиксированного значения — не мешаем обычному пути.
		return bodyBuf, true, "", http.StatusOK
	}

	meta := ExtractRequestMeta(bodyBuf, requestPath)
	if meta == nil {
		// Тело не разобрали — пропускаем как есть (cppworker сам разберётся).
		return bodyBuf, true, "", http.StatusOK
	}

	nPredict := meta.RequestedNPredict
	if nPredict <= 0 {
		nPredict = 2048
	}
	// Тот же расчёт, что в preflight: prompt + генерация + 10% запаса + EOS.
	reserve := meta.EstimatedPromptTokens / 10
	required := meta.EstimatedPromptTokens + nPredict + 1 + reserve

	if required <= fixed {
		// Хватает зафиксированного контекста — обслуживаем, но num_ctx в теле
		// подменяем на загруженный: cppworker не должен видеть чужой num_ctx,
		// иначе он ответит ошибкой code 2 (n_ctx exceeds loaded model).
		patched := patchNumCtxInBody(bodyBuf, fixed)
		if patched == nil {
			patched = bodyBuf
		}
		logger.Get().Infow("primary: запрос обслужен с зафиксированным n_ctx (настройки модели не меняются)",
			"backend", backendID, "model", modelName,
			"fixed_n_ctx", fixed,
			"client_num_ctx", meta.RequestedNCtxOverride,
			"estimated_prompt_tokens", meta.EstimatedPromptTokens,
			"n_predict", nPredict,
			"required_n_ctx", required)
		return patched, true, "", http.StatusOK
	}

	// Не хватает — отказ с понятным объяснением вместо тихой перезагрузки.
	logger.Get().Warnw("primary: запрос требует контекста больше зафиксированного — отказ",
		"backend", backendID, "model", modelName,
		"fixed_n_ctx", fixed,
		"client_num_ctx", meta.RequestedNCtxOverride,
		"required_n_ctx", required,
		"estimated_prompt_tokens", meta.EstimatedPromptTokens,
		"n_predict", nPredict)

	msg := fmt.Sprintf(
		"model %q имеет ЗАФИКСИРОВАННЫЕ настройки: n_ctx=%d, и его нельзя изменить "+
			"клиентским запросом. Этому запросу нужно %d токенов "+
			"(prompt≈%d + generation=%d + reserve). Уменьшите num_predict/размер промпта "+
			"или попросите администратора увеличить контекст модели в WebUI "+
			"(GGUF → настройки бэкенда) и перезагрузить её.",
		modelName, fixed, required, meta.EstimatedPromptTokens, nPredict)

	// Формат ответа зависит от клиента: streaming-клиенты (OpenWebUI/Cline)
	// ждут NDJSON-чанк с done=true, иначе видят «обрыв» вместо ошибки.
	if isStreamingFromBody("", bodyBuf) {
		w := &primaryResponseWriter{header: http.Header{}}
		writeStreamingRejectNDJSON(w, http.StatusRequestEntityTooLarge, modelName,
			&ReloadPlan{Decision: DecisionReject, Reason: "primary model: settings are fixed by administrator"},
			&NCtxBridgeError{Code: NCtxErrCodeNCtxNeedsReload, CurrentNCtx: fixed, RequiredNCtx: required, Message: msg})
		return w.body, false, "", w.status
	}

	return bodyBuf, false, msg, http.StatusRequestEntityTooLarge
}

// primaryResponseWriter — минимальный ResponseWriter в память: нужен, чтобы
// сформировать NDJSON-ответ об отказе, не отдавая его напрямую (вызывающий сам
// решает, как записать body — тот же контракт, что у preflight).
type primaryResponseWriter struct {
	header http.Header
	body   []byte
	status int
}

func (w *primaryResponseWriter) Header() http.Header {
	if w.header == nil {
		w.header = http.Header{}
	}
	return w.header
}

func (w *primaryResponseWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	w.body = append(w.body, p...)
	return len(p), nil
}

func (w *primaryResponseWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
}
