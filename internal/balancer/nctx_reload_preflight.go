// nctx_reload_preflight.go — R631 (2026-11-19): Предполётная проверка «вырастет ли окно» (preflightNCtxReloadIfNeeded) — решения до очереди и до стрима.
//
// ЗАЧЕМ ВЫНЕСЕНО. `nctx_reload_handlers.go` держал 2316 строк и четыре
// независимых слоя (обработчики, preflight, кэш наблюдённого окна, исполнение
// и метрики). Здесь — один из них; поведение не менялось, код перенесён как есть.
package balancer

import (
	"context"
	"fmt"
	"net/http"
	"ollama-loadbalancer/internal/cppbackend"
	"ollama-loadbalancer/internal/i18n"
	"ollama-loadbalancer/pkg/logger"
	"time"
)

// preflightNCtxReloadIfNeeded — проверяет ПЕРЕД проксированием, что loaded n_ctx
// на бэкенде покрывает запрашиваемый клиентом num_ctx. Если нет —
// АСИНХРОННО запускает POST /api/models/reload с требуемым n_ctx в фоне
// и возвращает клиенту HTTP 503 + Retry-After.
//
// ЗАЧЕМ: при первом запросе от OpenWebUI с tools (system + tool definitions
// ~3000-5000 токенов) клиент посылает options.num_ctx=16384, а модель
// загружена с n_ctx=4096 (default cppworker). Без preflight — cppworker
// сразу возвращает code 2 (ErrNCtxNeedsReload), балансер делает reload
// по факту ошибки, модель выгружается и загружается заново (10-30 сек).
//
// ВАЖНО (2026-06-22): preflight БОЛЬШЕ НЕ ДЕЛАЕТ синхронный reload.
// Раньше здесь был блокирующий POST /api/models/reload, который:
//   - выгружал текущую модель из VRAM на 10-30 сек,
//   - параллельные запросы получали 500 с "handle is nil",
//   - streaming-клиент получал обрыв без полного ответа.
//
// Теперь reload запускается в горутине, а клиент сразу получает
// 503 Service Unavailable с Retry-After: 5. Клиент (OpenWebUI, Cline, curl)
// делает повторный запрос через 5 секунд — к этому моменту reload либо
// уже завершён, либо ещё идёт (тогда клиент получит 503 ещё раз).
// Стрим НЕ обрывается посередине.
//
// Возвращает:
//   - modifiedBody = bodyBuf (если reload не нужен) —
//     рекомендуется проксировать с этим body
//   - needsProxy = true если можно проксировать (reload не нужен)
//   - needsProxy = false если требуется reload (клиенту возвращён 503)
//   - errorMessage = "" если ОК, иначе сообщение об ошибки
//   - statusCode = http.StatusOK если можно проксировать,
//     http.StatusServiceUnavailable если запущен reload
func (p *Proxy) preflightNCtxReloadIfNeeded(
	ctx context.Context,
	backendID, modelName string,
	bodyBuf []byte,
	requestPath string,
) (modifiedBody []byte, needsProxy bool, errMsg string, statusCode int) {
	// R301 (2026-10-29): streaming-вердикт считается ДО проверки координатора.
	//
	// Почему здесь, а не ниже (где раньше стоял безусловный пропуск стриминга):
	//   - координатор (p.nctxReload) может быть выключен конфигом, и тогда
	//     прежний выход `p.nctxReload == nil` возвращал запрос как есть — то есть
	//     «окно меньше нужного» снова уезжало в воркер;
	//   - вердикт не перезагружает модель (это делает не-стриминговый путь), он
	//     только приводит num_ctx к загруженному окну либо отказывает с числами.
	// См. growth_stream_guard.go — там же живой дефект и обоснование.
	// R304 (2026-10-30): перезагрузка окна — это тоже «поднять копию на узле».
	// Донор свою не держит: его VRAM отдана слоям модели мастера. Проверка стоит
	// ПЕРВОЙ, до любого решения об окне, потому что при роли донора окно менять
	// нечего — и лучше сказать это сразу, чем отправить загрузку воркеру.
	//
	// Когда сюда вообще может прийти донор: селектор его больше не выбирает, но
	// сессия могла быть привязана к узлу ДО того, как оператор сменил роль. Такая
	// сессия должна переехать на копию мастера (R303 делает это по отсутствию
	// модели), а не перезагружать донора под живым стримом.
	if backendID != "" && p.cannotHostModelR304(backendID) {
		return bodyBuf, false,
			i18n.T("balancer.donor.reload_refused", backendID), http.StatusConflict
	}
	if len(bodyBuf) == 0 {
		return bodyBuf, true, "", http.StatusOK
	}
	if modelName != "" && isStreamingFromBody("", bodyBuf) {
		v := p.growthVerdictR301(RequestIDFromContext(ctx), backendID, modelName, bodyBuf, requestPath, true)
		logGrowthVerdictR301("transport-preflight", backendID, modelName, requestPath, v)
		// R447: числа вердикта едут к writer'у тела вместе с запросом — иначе
		// тело пересчитает их после перезагрузки и разойдётся с текстом.
		if v.Grow || v.decided() {
			recordGrowthNumbersR447(ctx, v)
		}
		if v.Refuse {
			// R664 (2026-11-20) ШАГ B: данные не влезают, окна клиент не заявил —
			// 413 с МАШИННЫМ кодом и числами вместо 503 «повторите позже».
			// Тело собирает общий сборщик (то же, что у preflight-движка), поэтому
			// клиент видит один формат на всех путях.
			//
			// R679 (2026-11-20, живой прогон матрицы): ПОТОЛКИ тоже обязаны быть
			// настоящими. Здесь стояли три нуля — и клиент получал отказ, в
			// котором `physical_ceiling_tokens`, `gguf_max_context` и
			// `auto_reload_max_n_ctx` равны 0 при реальных 65536/131072/65536.
			// Ноль в этих полях читается как «потолок неизвестен», то есть
			// «заявить больше нельзя», — противоположное смыслу отказа.
			ceilings := p.ceilingNumbersForR664(backendID, modelName)
			return bodyBuf, false,
				contextWindowExhaustedBodyR664(windowRejectNumbersR664{
					BackendID:          backendID,
					Model:              modelName,
					PromptTokens:       v.Estimated,
					LoadedWindow:       v.Loaded,
					RequiredWindow:     v.Required,
					Requested:          ExtractNumCtxFromBody(bodyBuf),
					AvailablePerClient: v.Available,
					Slots:              p.loadedSlotsNCtx(backendID, modelName),
					Ceilings:           ceilings,
				}),
				http.StatusRequestEntityTooLarge
		}
		if v.Grow {
			// R385: окно можно поднять — перезагрузка запущена, клиенту нужно
			// повторить. Тело при этом не меняем: следующий запрос получит уже
			// новое окно (тот же контракт, что у не-стримингового роста).
			p.triggerStreamGrowthR385(backendID, modelName, v.Needed, v.Target)
			return bodyBuf, false, growthStartsMessageR385(modelName, v), http.StatusServiceUnavailable
		}
		if v.Served {
			return v.Body, true, "", http.StatusOK
		}
		if v.Defer {
			return bodyBuf, false, growthDeferMessageR301(modelName, v), http.StatusServiceUnavailable
		}
		return bodyBuf, true, "", http.StatusOK
	}
	if p.nctxReload == nil {
		return bodyBuf, true, "", http.StatusOK
	}

	// R60.23 (2026-09-09): empty-model guard. Без этой проверки OLD preflight
	// отправлял POST /api/models/load с `"name": ""` → cppworker возвращал
	// 400 "name is required" → state-машина coordinator зависала в
	// "reloading" → ВСЕ последующие запросы получали 503 "retry in 30s"
	// даже после того как реальные /api/chat запросы с правильным model
	// приходили (те попадали на IsReloadPending check с пустым model и
	// пропускались, но profile=65536 mismatch detection в новом
	// runInferencePreflight триггерил ещё одну попытку reload — и тоже
	// падал с 400 "name is required").
	//
	// Корень бага: кто-то отправляет POST /api/load на balancer с пустым
	// body (например, OpenWebUI preload probe или webui ping). parseRequestBody
	// возвращает пустой model, контекст пустой, preflight срабатывает,
	// executeAsyncReload отправляет cppworker'у "name": "" → 400.
	//
	// Защита: при пустом modelName — пропускаем preflight, проксируем
	// запрос как есть. cppworker сам ответит 400/404 на /api/load если
	// endpoint не поддерживается, и это правильное поведение.
	if modelName == "" {
		// R60.23: расширенный лог для диагностики — оператор должен видеть
		// что именно прислал клиент (truncated до 200 символов чтобы не
		// раздувать логи при больших body). Это помогает понять источник
		// (OpenWebUI preload probe, custom integration, webui ping).
		bodyPreview := string(bodyBuf)
		if len(bodyPreview) > 200 {
			bodyPreview = bodyPreview[:200] + "...(truncated)"
		}
		logger.Get().Debugw("preflightNCtxReloadIfNeeded: empty modelName, skipping preflight (R60.23)",
			"backend", backendID, "request_path", requestPath, "body_len", len(bodyBuf),
			"body_preview", bodyPreview)
		return bodyBuf, true, "", http.StatusOK
	}

	// R119 (2026-10-15): модели вообще НЕТ на этом бэкенде — перезагрузкой её не
	// создать. Проверяем ЭТО ПЕРВЫМ, до primary/IsReloadPending/streaming-веток:
	// живой цикл выглядел так — запрос → async-загрузка → cppworker ответил 404
	// со списком available_models → балансер проигнорировал, на следующий запрос
	// снова планировал загрузку, а на запрос после этого срабатывал
	// IsReloadPending-guard и клиент получал 503 «reload already in progress» /
	// «is being reloaded to n_ctx=… retry in 30s». Клиент ждал сотни секунд и
	// падал по своему таймауту («Internal Server Error» у OpenWebUI); замер:
	// POST /api/generate через балансер не отвечал более 4 минут.
	//
	// 404 (а не 503) принципиален: 503 означает «повтори, станет лучше», и клиенты
	// уходят в retry-цикл, а модели от повторения не появится. Ollama для
	// неизвестной модели тоже отдаёт 404.
	if reason, available, known := p.modelMissingFor(backendID, modelName); known {
		logger.Get().Warnw(i18n.T("balancer.nctx_reload_handlers.preflight_model_missing_404_warn"),
			"backend", backendID, "model", modelName,
			"request_path", requestPath, "available_models", len(available))
		return bodyBuf, false,
			formatModelMissingMessage(modelName, reason, available),
			http.StatusNotFound
	}

	// R83 (2026-09-29): PRIMARY — настройки модели зафиксированы администратором.
	//
	// Клиентский запрос НЕ меняет параметры загруженной модели:
	//   * хватает зафиксированного контекста — обслуживаем, подменив num_ctx в теле
	//     на загруженный (cppworker не должен видеть чужой num_ctx);
	//   * не хватает — отказ с указанием зафиксированных значений и подсказкой
	//     «править через администратора». Раньше в этом случае балансер молча
	//     перезагружал модель на бОльший контекст, и администратор узнавал об этом
	//     только по расходу VRAM.
	if p.isPrimaryModel(modelName) {
		return p.applyPrimaryModelPolicy(backendID, modelName, bodyBuf, requestPath)
	}

	// R60.6 (2026-09-07): IsReloadPending guard. Если reload для этой модели
	// уже в процессе (через новый coordinator dedup path в RunPreflight),
	// НЕ запускаем второй reload — просто возвращаем 503. Без этого
	// stream-запросы (которые идут через этот OLD path) триггерят cascading
	// reload goroutines, каждая из которых отменяется http.Client.Timeout
	// на 90s. Client видит 503 → retry → 503 → retry → 5 reloads в логе.
	if p.nctxReload.IsReloadPending(backendID, modelName) {
		// R291: в тексте отказа обязаны быть ЧИСЛА. Живой факт: клиент получал
		// «reload already in progress, retry later» без указания, сколько нужно
		// и сколько есть, — оператор не мог понять, вырастет ли окно вообще.
		estimated, loaded, needed := p.contextGrowthNumbers(backendID, modelName, bodyBuf, requestPath)
		// R299: числа берём из общего источника (growth_guard.go), поэтому
		// activeStreams здесь — максимум из учёта по бэкенду и по модели, а не
		// одно наблюдение, которое могло не увидеть стрим под расходящимся именем.
		active, pending := growthBlockersR288(p, backendID, modelName)
		logger.Get().Infow("preflightNCtxReloadIfNeeded: reload already pending (R60.6 dedup)",
			"backend", backendID, "model", modelName, "request_path", requestPath,
			"estimated_prompt_tokens", estimated, "loaded_window_tokens", loaded,
			"required_tokens", needed,
			"active_streams", active, "reload_pending", pending)
		if estimated > 0 && loaded > 0 {
			// R299: «active streams=0» при идущей перезагрузке — не ложь счётчика,
			// а разные факты: запрос, ждущий в preflight, стримом ещё не учтён
			// (beginClientStreamR271 — после admission). Пишем оба факта явно.
			return bodyBuf, false,
				fmt.Sprintf("model %q reload already in progress (required=%d tokens, loaded window=%d tokens, active streams=%d, reload pending=%t), retry later",
					modelName, needed, loaded, active, pending),
				http.StatusServiceUnavailable
		}
		return bodyBuf, false,
			fmt.Sprintf("model %q reload already in progress, retry later", modelName),
			http.StatusServiceUnavailable
	}

	// R299 (2026-10-29): ТРАНСПОРТНЫЙ путь проверяет то же самое ДО reload'а.
	//
	// Живой факт: это ВТОРОЙ вход в preflight (после раунтера), и на нём запрос
	// точно так же проводил десятки секунд в RunPreflight, хотя окно вырасти не
	// могло. Проверка одна на оба входа (growthDeferR299): если бы правило жило
	// только в раунтере, транспортный путь остался бы прежним — ровно так в этой
	// серии уже случалось с другими механизмами (второй путь обходит общий).
	if deferNow, estimated, loaded, needed, active, pending := p.growthDeferR299(
		backendID, modelName, bodyBuf, requestPath); deferNow {
		logger.Get().Infow("preflightNCtxReloadIfNeeded: context growth deferred before reload (R299)",
			"backend", backendID, "model", modelName, "request_path", requestPath,
			"estimated_prompt_tokens", estimated, "required_tokens", needed,
			"loaded_window_tokens", loaded, "active_streams", active,
			"reload_pending", pending)
		return bodyBuf, false,
			fmt.Sprintf("model %q needs %d tokens of context, the loaded window is %d and cannot grow right now (active streams=%d, reload pending=%t), retry later",
				modelName, needed, loaded, active, pending),
			http.StatusServiceUnavailable
	}

	// R301 (2026-10-29): streaming-запросы сюда уже не доходят — вердикт по ним
	// считается в самом начале функции (см. блок сразу после проверки пустого
	// тела), потому что он нужен и при выключенном координаторе. Здесь остаётся
	// только НЕ-стриминговый путь, где рост окна возможен и выполняется ниже.

	// 1. Извлекаем requested n_ctx: body > profile > backend.
	//
	// Round 35c+ (2026-08-13): раньше использовался только ExtractNumCtxFromBody
	// который читал num_ctx ТОЛЬКО из body. Это приводило к reload на 8192 если
	// клиент (Open WebUI) слал num_ctx=8192 по дефолту, даже если profile говорил
	// 32768 — потому что first-load preflight не видел fallback'а на profile.
	//
	// НОВАЯ ЛОГИКА (Round 35c+, исправленная): мы НЕ используем ResolveNumCtx
	// напрямую, потому что он clamping'ит body num_ctx к maxNumCtxForModel.
	// Это ломает smart-skip (см. баг cdae3e4 → 7b50 commit): если body=16384
	// и loaded=4096, resolver возвращает 4096 (clamped), preflight видит
	// loaded >= requested, НЕ patching body — и cppworker получает body с
	// num_ctx=16384 при загруженной модели с 4096 → error code 2 → reload loop.
	//
	// Вместо этого: body имеет АБСОЛЮТНЫЙ приоритет (клиент знает что хочет).
	// Fallback на profile/backend только если body НЕ задал num_ctx — это
	// гарантирует first-load с правильным ctx (32768 из profile, не 8192 из
	// body default клиента).
	requestedNCtx := ExtractNumCtxFromBody(bodyBuf)
	// R83-fix (2026-09-30): клиент вообще не задал окно? Это отдельный случай,
	// см. правило ниже (не перезагружать уже загруженную модель).
	clientSilentAboutNCtx := requestedNCtx <= 0
	profileNCtx := p.GetModelProfileNumCtx(modelName)
	backendDefaultNCtx := 0
	if backendID != "" {
		backendDefaultNCtx = p.GetBackendDefaultNumCtx(backendID)
	}
	// Round 35c+ (2026-08-13): diagnostic logging для отладки "num_ctx downgrade
	// 32768 → 8192". Логируем ВСЕГДА на preflight (Info уровень), чтобы можно
	// было увидеть в balancer-логах что реально приходит от клиента:
	//   - body_num_ctx: что прислал клиент (0 если молчит)
	//   - profile_num_ctx: что в профиле модели
	//   - backend_default_num_ctx: что в default конфиге бэкенда
	//   - chosen_source: откуда взяли requestedNCtx (body/profile/backend/none)
	var chosenSource string
	switch {
	case requestedNCtx > 0:
		chosenSource = "body"
	case profileNCtx > 0:
		chosenSource = "profile"
	case backendDefaultNCtx > 0:
		chosenSource = "backend_default"
	default:
		chosenSource = "none"
	}
	logger.Get().Infow("preflightNCtxReload: num_ctx decision",
		"backend", backendID, "model", modelName,
		"body_num_ctx", requestedNCtx,
		"profile_num_ctx", profileNCtx,
		"backend_default_num_ctx", backendDefaultNCtx,
		"chosen_source", chosenSource)
	if requestedNCtx <= 0 {
		// Body не задал num_ctx → fallback на profile → backend default.
		// Это Round 35c+ fix для "Open WebUI шлёт num_ctx=8192 по дефолту,
		// даже если profile говорит 32768". First-load preflight теперь reload
		// в profile.ContextLength (32768) вместо body num_ctx (8192).
		requestedNCtx = profileNCtx
		if requestedNCtx <= 0 {
			requestedNCtx = backendDefaultNCtx
		}
		if requestedNCtx <= 0 {
			return bodyBuf, true, "", http.StatusOK
		}
		logger.Get().Debugw("preflightNCtxReload: using profile/backend fallback (body had no num_ctx)",
			"backend", backendID, "model", modelName, "requested_n_ctx", requestedNCtx)
	}

	// R545 (F9): отказ по потолку авто-reload перенесён НИЖЕ — после
	// resolveLoadedWindowNCtx (см. блок «ПОТОЛОК auto-reload — ТОЛЬКО для
	// загруженной модели»). Здесь он стоял ДО определения состояния модели и
	// потому бил по ПУСТОМУ узлу: клиент просит 200k, а на бэкенде, где модели
	// ещё нет, он получал 413 «выше потолка», хотя по контракту (R83, R255)
	// потолок ограничивает только АВТО-рост уже загруженной модели.

	// 2. R118 (2026-10-14): ЧТО ИМЕННО известно о модели — «загружена»,
	//    «грузится прямо сейчас», «модели нет» или «состояние неизвестно».
	//
	// ДО R118 все четыре случая приходили сюда как loaded_n_ctx=0, и preflight в
	// каждом из них планировал загрузку/перезагрузку. Живое воспроизведение на
	// стенде (см. шапку loadedWindowState в num_ctx_resolver.go): пока оператор
	// грузил модель с ctx=32768, клиентский запрос с num_ctx=8192 запустил
	// ВТОРУЮ загрузку, cppworker выгрузил первую («reloading model because
	// parameters changed»), она упала с «model load aborted by user» (71 c
	// впустую), а клиент получил 503 «being reloaded ... retry in 30s».
	//
	// ТЕПЕРЬ: «грузится» — ждём свою же загрузку, второй не запускаем;
	// «неизвестно» (опрос /api/models не прошёл) — не принимаем разрушительных
	// решений, запрос идёт как есть; «загружена» — обычная логика ниже;
	// «модели нет» — загрузка с окном из настроек (тоже ниже).
	loadedNCtx, loadedState := p.resolveLoadedWindowNCtx(backendID, modelName, true)
	switch loadedState {
	case loadedWindowLoading:
		logger.Get().Infow(i18n.T("balancer.nctx_reload_handlers.model_loading_skip_second_load_info"),
			"backend", backendID, "model", modelName,
			"requested_n_ctx", requestedNCtx,
			"hint", i18n.T("balancer.nctx_reload_handlers.model_loading_hint"))
		return bodyBuf, false,
			fmt.Sprintf("model %q is loading on backend %s; waiting for the load already in progress "+
				"(no second load started, prepared n_ctx is preserved)", modelName, backendID),
			http.StatusServiceUnavailable
	case loadedWindowUnknown:
		logger.Get().Warnw(i18n.T("balancer.nctx_reload_handlers.model_state_unknown_no_reload_warn"),
			"backend", backendID, "model", modelName,
			"requested_n_ctx", requestedNCtx,
			"hint", i18n.T("balancer.nctx_reload_handlers.model_state_unknown_hint"))
		return bodyBuf, true, "", http.StatusOK
	}

	// R545 (F9): ПОТОЛОК auto-reload — ТОЛЬКО для загруженной модели.
	//
	// Здесь и только здесь он и имеет смысл: R83 завёл его против ТИХОЙ
	// перезагрузки уже работающей модели по клиентскому запросу (клиент поднимал
	// окно выше операторского лимита, и настройка перебивалась молча). Для
	// первичной загрузки потолок по контракту не действует — он «про авто-reload,
	// а не про явную загрузку» (см. комментарий в начале функции).
	//
	// Жалоба №9 плана 2026-11-19-webui-and-balancer-fixes: блок стоял ВЫШЕ
	// resolveLoadedWindowNCtx и потому отвечал 413 клиенту, который просил 200k у
	// СВОБОДНОГО узла (модели там ещё нет). Теперь состояние уже известно:
	// «нет» — идём грузить клиентским окном (физику проверит гейт R522),
	// «грузится»/«неизвестно» — свои ветки выше, «загружена» — отказ по потолку.
	//
	// R140 (2026-10-16): в отказе называем СВЯЗЫВАЮЩИЙ предел, а не только
	// операторский: max_vram_n_ctx считается по фактически свободной VRAM, и
	// совет «поднимите лимит» невыполним, если связывает память.
	// R519: «0» у max_vram_n_ctx означает две разные вещи (см. ниже).
	if loadedState == loadedWindowLoaded {
		// R267: та же ЕДИНАЯ формула потолка, что у решателя (второй не существует).
		// R255: потолок авто-роста учитывает ЗАЯВЛЕННОЕ оператором окно.
		effCeil := 0
		if p.nctxReload != nil {
			if cfg := p.nctxReload.Config(); cfg.AutoReloadMaxNCtx > 0 {
				effCeil = p.effectiveAutoReloadCeiling(modelName, cfg.AutoReloadMaxNCtx)
			}
		}
		if effCeil > 0 && requestedNCtx > effCeil {
			physicalMax, physicalKnown := p.maxVRAMNCtxKnownFromMetrics(backendID)
			binding := effCeil
			if physicalMax > 0 && physicalMax < binding {
				binding = physicalMax
			}
			if physicalKnown && physicalMax == 0 {
				binding = 0
			}
			logger.Get().Warnw(i18n.T("balancer.nctx_reload_handlers.requested_above_limit_no_reload_warn"),
				"backend", backendID, "model", modelName,
				"requested_n_ctx", requestedNCtx,
				"auto_reload_max_n_ctx", effCeil,
				"max_vram_n_ctx", physicalMax,
				"vram_known", physicalKnown,
				"binding_limit", binding,
				"loaded_n_ctx", loadedNCtx,
				"hint", i18n.T("balancer.nctx_reload_handlers.requested_above_limit_hint"))
			if physicalKnown && physicalMax == 0 {
				return bodyBuf, false,
					formatNCtxVRAMExhaustedRefusal(requestedNCtx, effCeil, modelName),
					http.StatusRequestEntityTooLarge
			}
			return bodyBuf, false,
				formatNCtxCeilingRefusal(requestedNCtx, effCeil, physicalMax, modelName),
				http.StatusRequestEntityTooLarge
		}
	}

	// R303 (D4): модель НЕ загружена на этом узле, но жива на другом.
	//
	// ЖИВОЙ ДЕФЕКТ (двухузловой отчёт, D4): запрос пришёл на пустой мастер,
	// донор держал gemma (loaded=1). Балансер запланировал загрузку с нуля на
	// мастере, мастер не получил ни одного /api/models/load, клиент отвисел
	// 300 c и получил 503 «being reloaded to n_ctx=32768 (loaded=0)» — то есть
	// числа, которых нет, при живой модели рядом.
	//
	// ПРАВИЛО: «перезагружать» нечего, если модели здесь нет; грузить её с нуля
	// на пустом узле, когда она уже загружена на здоровом узле, — трата VRAM и
	// минут ожидания. Отказываем СРАЗУ и называем узел и его окно, где модель
	// живая. Клиент повторит и будет обслужен там (выбор узла это уже умеет —
	// R303 в model_copy_window.go спрашивает живой кластер).
	if loadedState == loadedWindowAbsent {
		if alt, altPerSeq, ok := p.modelNotLoadedHereButAliveElsewhereR303(backendID, modelName, requestedNCtx); ok {
			logger.Get().Warnw("R303: модель не загружена на этом узле, но жива на другом — загрузку с нуля не планируем",
				"backend", backendID, "model", modelName,
				"alt_backend", alt, "alt_loaded_per_seq", altPerSeq,
				"requested_n_ctx", requestedNCtx)
			return bodyBuf, false,
				fmt.Sprintf("model %q is not loaded on backend %s, but it is already loaded on backend %s "+
					"(n_ctx=%d per slot); no new load was started on %s — retry and the request will be routed to %s",
					modelName, backendID, alt, altPerSeq, backendID, alt),
				http.StatusServiceUnavailable
		}
	}

	// R83-fix (2026-09-30): сравниваем с ОКНОМ СЛОТА, а не с суммарным.
	//
	// `loadedNCtx` — СУММАРНОЕ окно модели (его же кладём в кэш метрик и
	// сравниваем с памятью). Но клиент просит окно ДЛЯ СЕБЯ: при двух слотах
	// суммарные 65536 дают клиенту 32768, и запрос с num_ctx=65536 формально
	// «покрыт» (65536 ≥ 65536), хотя фактически урезан вдвое — длинный промпт
	// Cline не влезает, клиент получает
	//   "preflight: prompt + n_predict exceeds n_ctx for this backend".
	// Живое воспроизведение: gemma-4-E4B загружена ctx=65536, max_slots=2,
	// клиент просит 65536 → на клиента 32768.
	//
	// Поэтому все сравнения/патчи тела ниже идут по perSeq, а суммарное значение
	// остаётся для reload-таргета (окно клиента × слоты) и для кэша метрик.
	if perSeq := p.loadedPerSeqNCtx(backendID, modelName); perSeq > 0 {
		if perSeq != loadedNCtx {
			logger.Get().Infow(i18n.T("balancer.nctx_reload_handlers.window_total_vs_per_seq_info"),
				"backend", backendID, "model", modelName,
				"total_n_ctx", loadedNCtx, "context_per_seq", perSeq)
		}
		loadedNCtx = perSeq
	}

	// R83-fix (2026-09-30): КЛИЕНТ НЕ ЗАДАЛ ОКНО + МОДЕЛЬ ЗАГРУЖЕНА → НЕ ТРОГАЕМ.
	//
	// ТРЕБОВАНИЕ ЭКСПЛУАТАЦИИ: «если клиент не прислал, с каким окном загружать,
	// то если модель загружена — перезагружать не надо; если не загружена — грузить
	// дефолтными значениями из настроек».
	//
	// ЧТО БЫЛО. При body_num_ctx=0 preflight подставлял contextLength профиля
	// (он задуман как HINT) и, если загруженный n_ctx ему не равнялся, запускал
	// async reload. Живой пример (проверено на стенде): оператор загрузил модель
	// с ctx=8192, первый же клиентский запрос БЕЗ num_ctx перезагрузил её в 16384
	// (в логе: chosen_source=profile, «detected n_ctx mismatch, scheduling async
	// reload»). Для оператора это выглядит как «настройки сбиваются сами».
	//
	// ЧТО СТАЛО. Если модель уже загружена и клиент окно не запрашивал — работаем
	// с тем, что загружено: это и есть настройки администратора. Клиент, который
	// не помещается в текущий контекст, получит от cppworker понятную ошибку, и
	// только тогда сработает реактивный путь (handleNCtxReload /
	// DecideReloadBackend) — то есть перезагрузка происходит по фактической
	// необходимости, а не спекулятивно.
	//
	// Если модель НЕ загружена (loadedNCtx == 0), правило не применяется: ниже
	// работает обычный приоритет «профиль → дефолт бэкенда», то есть загрузка
	// идёт значениями из настроек.
	if clientSilentAboutNCtx && loadedNCtx > 0 {
		logger.Get().Infow(i18n.T("balancer.nctx_reload_handlers.client_silent_loaded_no_reload_info"),
			"backend", backendID, "model", modelName,
			"loaded_n_ctx", loadedNCtx,
			"profile_hint_n_ctx", profileNCtx,
			"note", i18n.T("balancer.nctx_reload_handlers.client_silent_loaded_note"))
		return bodyBuf, true, "", http.StatusOK
	}

	// 3. R60.35: stickiness check с учётом `required` (prompt + n_predict).
	//
	// Симптом (R60.35): OpenWebUI шлёт num_ctx=2048 в body, модель loaded
	// с n_ctx=2048 (default), prompt=33 + n_predict=2048 (cppworker
	// default) = 2082 > 2048 → 400 "prompt too long". Старый preflight
	// видел `loaded (2048) >= requested (2048)` → NoOp. Не учитывал что
	// `required (prompt + n_predict + slack) > loaded` → reload бы
	// помог. Дубликат R60.31 bug, но в OLD preflight path.
	//
	// R60.31 fix был только в DecidePreflight. Этот path — старый
	// "smart reload skip" (lines 586+), он работает для n_predict=0
	// (Ollama streaming с default = unlimited), но НЕ для n_predict>0.
	// Исправляем здесь тоже: если required > loaded, всё равно reload.
	//
	// R60.46 (2026-09-11) FIX: НЕ reload если user не указал n_predict.
	// Pre-R60.46: n_predict=0 → assumed cppworker default 2048 → required=2050
	// > loaded=2048 → trigger reload to 4096. Это FALSE POSITIVE:
	// 1) User не просил 2048 токенов output, может ему хватит 100
	// 2) Reload 2048→4096 занимает 90-180s (cascade timeout)
	// 3) User застрял в reload loop
	// Post-R60.46: n_predict=0 → не assume, skip reload check.
	// Если действительно overflow — cppworker вернёт 400, user retry с proper n_predict.
	if loadedNCtx >= requestedNCtx {
		// R60.35: дополнительно проверяем required (с учётом n_predict).
		// OpenAI /v1/chat/completions: max_tokens=0 → cppworker default 2048.
		// Ollama /api/chat: num_predict=0 → cppworker default 2048.
		// Реальный required = prompt + 2048 + 1 = может превысить loaded.
		//
		// R60.46: только check required если user ЯВНО указал n_predict > 0.
		// Иначе (n_predict=0 = default) — НЕ reload, пусть cppworker сам
		// отдаст 400 если overflow.
		meta := p.extractRequestMeta(bodyBuf, requestPath)
		if meta == nil {
			return bodyBuf, true, "", http.StatusOK
		}
		// R60.46: skip reload check если user не указал n_predict.
		// (Иначе false positive: user не просил 2048 токенов, но R60.35
		// assumes cppworker default и triggers reload 2048→4096.)
		if meta.RequestedNPredict <= 0 {
			return bodyBuf, true, "", http.StatusOK
		}
		// R83-fix (2026-09-30): запас ограничен сверху (см. preflight_nctx.go) —
		// иначе на длинных сессиях 10% превращались в десятки тысяч токенов и
		// запрос, который помещается, отклонялся как «prompt + n_predict exceeds».
		reserveSlack := meta.EstimatedPromptTokens / 10
		if reserveSlack > maxReserveSlackTokens {
			reserveSlack = maxReserveSlackTokens
		}
		nPredict := meta.RequestedNPredict
		required := meta.EstimatedPromptTokens + nPredict + 1 + reserveSlack
		if required > loadedNCtx {
			// R60.35: prompt не влезает → trigger reload на larger n_ctx.
			// Сначала patch'нем body чтобы убрать body num_ctx (чтобы
			// после reload cppworker не получал n_ctx=2048 опять).
			// Новый target n_ctx: max(required, loadedNCtx*2) rounded up.
			newTarget := required
			if newTarget < loadedNCtx*2 {
				newTarget = loadedNCtx * 2
			}
			// Round up to power of 2
			roundedTarget := roundUpPow2(newTarget)
			patchedBody := patchNumCtxInBody(bodyBuf, roundedTarget)
			if patchedBody == nil {
				patchedBody = bodyBuf
			}
			logger.Get().Infow("preflightNCtxReload: R60.35 — required > loaded, triggering reload",
				"backend", backendID, "model", modelName,
				"loaded_n_ctx", loadedNCtx,
				"requested_n_ctx", requestedNCtx,
				"required_n_ctx", required,
				"new_target_n_ctx", roundedTarget,
				"estimated_prompt_tokens", meta.EstimatedPromptTokens,
				"n_predict", nPredict)
			// Fall through to reload path (don't return early)
			bodyBuf = patchedBody
			requestedNCtx = roundedTarget
			// Continue to the reload logic below
		} else {
			// Smart skip: patch body to use loaded_n_ctx, no reload needed.
			patchedBody := patchNumCtxInBody(bodyBuf, loadedNCtx)
			if patchedBody != nil {
				logger.Get().Debugw("preflightNCtxReload: R60.35 — required ≤ loaded, no reload",
					"backend", backendID, "model", modelName,
					"loaded_n_ctx", loadedNCtx,
					"required_n_ctx", required)
				return patchedBody, true, "", http.StatusOK
			}
			return bodyBuf, true, "", http.StatusOK
		}
	}

	// 3.1 Round 23 (2026-08-04): SMART RELOAD SKIP.
	//
	// Проблема (issue #6+#8): клиент (Cline/OpenWebUI) посылает num_ctx=32000
	// "на всякий случай" (default), но реальный prompt — "2+2?" (1 токен).
	// Текущая логика ТУПО триггерит async reload на 50с, клиент получает
	// 503 + Retry-After: 5, retry 10 раз за 50с, потом (когда reload закончен)
	// получает успех. Субъективно для пользователя: "бесконечная перезагрузка".
	//
	// Решение: оценить реальный prompt size. Если estimated_prompt_tokens +
	// n_predict + slack <= loaded_n_ctx, то reload НЕ НУЖЕН — просто
	// patch'нем body (options.num_ctx = loaded_n_ctx) и проксируем дальше.
	// Модель работает с loaded_n_ctx, prompt помещается, всё OK.
	//
	// Стоимость: +1 JSON parse на preflight (≈50µs). Эвристика 1 token ≈ 4 chars
	// даёт ~25% точности; для коротких промптов это перестраховка, для длинных —
	// clamp'нем n_predict в cppworker (clampNPredictToFitContext).
	// R60.44 (2026-09-11) FIX: smart-skip ТОЛЬКО если requested_n_ctx <= loadedNCtx.
	// До фикса: smart-skip firing когда prompt помещается в loaded, даже если
	// user явно просил num_ctx > loaded (например body=16384, loaded=8192).
	// Patch body до loadedNCtx (8192), но X-Cpp-Ctx header ставился от
	// ОРИГИНАЛЬНОГО body (16384). cppworker получал body с num_ctx=8192 +
	// header n_ctx=16384, отвергал 400 "exceeds model's effective n_ctx".
	// Затем balancer auto-reload триггерился → unload → reload → 3+ минуты.
	// User видел "empty response / timeout" (exactly user's complaint).
	//
	// Правильная логика: smart-skip ТОЛЬКО когда requested_n_ctx <= loadedNCtx.
	// В этом случае patch body (downgrade к loadedNCtx — same value anyway,
	// noop) И не триггерим reload.
	//
	// Когда requested_n_ctx > loadedNCtx: skip the smart-skip и fallthrough
	// к reload path (line 692+). Preflight запустит async reload, user
	// получит 503 + Retry-After, через retry получит успех с новым n_ctx.
	if requestedNCtx > 0 && loadedNCtx > 0 && requestedNCtx <= loadedNCtx {
		meta := p.extractRequestMeta(bodyBuf, requestPath)
		if meta != nil {
			required := requiredNCtxForMeta(meta)
			if required <= loadedNCtx {
				// Промпт влезает в текущий n_ctx. Patch body и проксируем.
				patchedBody := patchNumCtxInBody(bodyBuf, loadedNCtx)
				if patchedBody != nil {
					logger.Get().Infow("preflightNCtxReload: smart-skip reload (prompt fits in current n_ctx)",
						"backend", backendID, "model", modelName,
						"loaded_n_ctx", loadedNCtx,
						"requested_n_ctx", requestedNCtx,
						"estimated_prompt_tokens", meta.EstimatedPromptTokens,
						"required_n_ctx", required,
						"n_predict", meta.RequestedNPredict)
					return patchedBody, true, "", http.StatusOK
				}
				// patchNumCtxInBody вернул nil (не смог распарсить) — fallback на reload.
			}
		}
	}

	// 4. Loaded < requested — нужен reload. Запускаем его АСИНХРОННО в горутине.
	//
	// Почему не синхронно (как было раньше):
	//   - reload выгружает текущую модель из VRAM на 10-30 сек,
	//   - параллельные запросы получают HTTP 500 с "handle is nil",
	//   - streaming-клиент теряет середину ответа (обрыв стрима),
	//   - клиент получает context deadline exceeded при медленном reload.
	//
	// Новая логика: запускаем reload в горутине, сразу возвращаем клиенту
	// HTTP 503 Service Unavailable + Retry-After: 5. Клиент повторит
	// запрос через 5 секунд — к этому моменту reload обычно завершён.
	// R83-fix (2026-09-30): клиентское окно — НА КЛИЕНТА, а не суммарное.
	//
	// Семантика клиента (Cline/OpenWebUI/Ollama): `num_ctx=N` — окно, которое
	// нужно ЭТОМУ запросу. llama.cpp при kv_unified=false делит суммарное окно
	// между слотами (n_ctx_seq = PAD(n_ctx / slots, 256)), поэтому при двух
	// слотах запрос с num_ctx=65536 получал бы только 32768 — и длинный промпт
	// Cline (system + tools + история) не влезал: клиент видел
	//   "preflight: prompt + n_predict exceeds n_ctx for this backend".
	// Именно это воспроизведено на стенде: gemma-4 загружена с ctx=65536 и
	// max_slots=2 → на клиента 32768.
	//
	// Теперь суммарное окно, до которого делаем reload, считается как
	// «окно клиента × слоты» (PAD256), то есть каждый клиент получает ровно
	// запрошенное, а memfit проверяет суммарный объём.
	// R493-12h: выравнивание по 256 нужно и при ОДНОМ слоте — ровно так окно
	// округляет воркер (GGML_PAD) и ровно это значение он показывает в
	// /api/models. Невыровненное число здесь означало: воркер загружает 150016,
	// а балансер кэширует 149809 и на следующем ходу просит перезагрузку «до
	// 149809», то есть ВНИЗ на 207 токенов.
	reloadTarget := cppbackend.PadContext(requestedNCtx)
	if slots := p.loadedSlotsNCtx(backendID, modelName); slots > 1 {
		if total, per := cppbackend.EffectiveContextSize(0, requestedNCtx, slots); total > 0 {
			reloadTarget = total
			logger.Get().Infow(i18n.T("balancer.nctx_reload_handlers.client_window_total_recalculated_info"),
				"backend", backendID, "model", modelName,
				"requested_per_seq", per, "slots", slots, "total_n_ctx", total)
		}
	}

	// R118.7 (2026-10-14): cppworker уже отвечал 422 n_ctx_infeasible на такое
	// окно — не планируем эту перезагрузку снова, а объясняем клиенту.
	//
	// ЖИВОЙ ЦИКЛ (лог стенда): запрос → «detected n_ctx mismatch» → async reload
	// target 16384 → 422 n_ctx_infeasible («stage: does_not_fit», «веса не
	// помещаются ни в VRAM, ни в RAM») → запрос снова → снова reload… Клиент при
	// этом ждал до 33 с и получал окно меньше запрошенного без объяснения.
	if ceiling, infeasibleReason, known := p.nctxInfeasibleFor(backendID, modelName); known && reloadTarget > ceiling {
		logger.Get().Warnw(i18n.T("balancer.nctx_reload_handlers.window_infeasible_no_reload_warn"),
			"backend", backendID, "model", modelName,
			"loaded_n_ctx", loadedNCtx, "requested_n_ctx", requestedNCtx,
			"reload_target_total", reloadTarget, "ceiling_n_ctx", ceiling,
			"reason", infeasibleReason)
		return bodyBuf, false,
			fmt.Sprintf("backend %s cannot grow the context to %d tokens (client asked %d per slot): %s",
				backendID, reloadTarget, requestedNCtx, infeasibleReason),
			http.StatusRequestEntityTooLarge
	}

	// R119 (2026-10-15): модели вообще НЕТ на этом бэкенде — перезагрузкой её не
	// создать. Проверка стоит В НАЧАЛЕ функции (см. выше), сюда доходит только
	// случай, когда запись появилась уже во время этого запроса.
	if reason, available, known := p.modelMissingFor(backendID, modelName); known {
		return bodyBuf, false,
			formatModelMissingMessage(modelName, reason, available),
			http.StatusNotFound
	}

	logger.Get().Infow("preflightNCtxReload: detected n_ctx mismatch, scheduling async reload",
		"backend", backendID, "model", modelName,
		"loaded_n_ctx", loadedNCtx,
		"requested_n_ctx", requestedNCtx,
		"reload_target_total", reloadTarget,
		"loaded_n_ctx_is_zero", loadedNCtx == 0)

	// Получаем backend state для URL.
	p.mu.RLock()
	backendState, exists := p.backends[backendID]
	p.mu.RUnlock()
	if !exists {
		// Backend не найден — проксируем как есть, cppworker вернёт свою ошибку.
		return bodyBuf, true, "", http.StatusOK
	}

	// Запускаем reload в горутине. Неблокирующий режим: клиент сразу
	// получит 503 + Retry-After, а reload идёт параллельно.
	//
	// R69 (2026-09-23): через единый gate — если для этой модели reload уже идёт
	// (preflight-путь или обработчик 413), вторую загрузку не стартуем: cppworker
	// отменяет предыдущую («model load aborted by user»), rollback падает и модель
	// остаётся выгруженной.
	if !p.nctxReload.StartModelReloadIfNotPending(backendID, modelName, func(_ *reloadEntry) {
		p.executeAsyncReload(backendID, modelName, reloadTarget, backendState.Backend)
	}) {
		logger.Get().Infow("preflightNCtxReload: reload already pending for this model, reusing it (R69 dedup)",
			"backend", backendID, "model", modelName, "requested_n_ctx", requestedNCtx)
	}

	// Обновляем кэш метрик: ставим ContextLength = requestedNCtx,
	// чтобы следующие preflight-чеки не запускали reload повторно.
	// Реальное значение обновит metrics_poller через ~5 сек после успешного reload.
	p.cacheLoadedContextLength(backendID, modelName, reloadTarget)

	// R68 (2026-09-23): ждём завершения reload'а и обслуживаем ПЕРВЫЙ же запрос.
	// Раньше клиент получал 503 «being reloaded to n_ctx=…; retry in 30s» и
	// (Cline/OpenWebUI) показывал ошибку — жалоба «проходит только повторный
	// запрос». Ожидание ограничено LB_NCTX_PREFLIGHT_WAIT_SEC (default 240 c,
	// 0 = прежнее поведение).
	if wait := nctxPreflightWaitTimeout(); wait > 0 {
		waitStart := time.Now()
		if p.waitForBackendNCtx(context.Background(), backendID, modelName, reloadTarget, wait) {
			p.cacheLoadedContextLength(backendID, modelName, reloadTarget)
			logger.Get().Infow("preflightNCtxReload: reload finished while client waited, serving request",
				"backend", backendID, "model", modelName,
				"new_n_ctx", reloadTarget,
				"waited_ms", time.Since(waitStart).Milliseconds())
			return bodyBuf, true, "", http.StatusOK
		}
		logger.Get().Warnw("preflightNCtxReload: wait for reload timed out, returning 503",
			"backend", backendID, "model", modelName,
			"requested_n_ctx", requestedNCtx,
			"waited_ms", time.Since(waitStart).Milliseconds())
	}

	// R119 (2026-10-15): пока клиент ждал, фоновая загрузка могла получить от
	// cppworker терминальный 404 «модели нет» (executeAsyncReload записал это в
	// реестр). Тогда отдаём 404 сразу, а не 503 «повтори через 30 с»: модели от
	// повторения не появится, а клиент (OpenWebUI) на 503 уходит в retry-цикл и в
	// итоге показывает «Internal Server Error».
	//
	// Гонку «запись появилась на миллисекунды позже решения» здесь НЕ закрываем
	// ожиданием: preflight обязан возвращать управление сразу после планирования
	// перезагрузки (на это опираются тесты роста окна — им важно, что reload ещё
	// в полёте). Короткое ожидание делает вызывающий транспорт перед тем, как
	// отдать клиенту 503 (см. llamacpp_transport*.go).
	if reason, available, known := p.modelMissingFor(backendID, modelName); known {
		logger.Get().Warnw(i18n.T("balancer.nctx_reload_handlers.model_missing_404_warn"),
			"backend", backendID, "model", modelName,
			"available_models", len(available))
		return bodyBuf, false,
			formatModelMissingMessage(modelName, reason, available),
			http.StatusNotFound
	}

	// Возвращаем клиенту 503 + Retry-After: 30. needsProxy=false — вызывающий код
	// должен сам сформировать ответ (status, headers, body).
	//
	// Round 23 (2026-08-04): Retry-After увеличен 5 → 30 секунд.
	// Причина: реальный reload (unload + load с большим n_ctx) занимает 30-60с
	// на типичной GPU. Старое Retry-After: 5 заставляло клиента (Cline/OpenWebUI)
	// повторять 10-12 раз за время reload'а, накапливая 503 в логах и путая
	// retry-логику. С 30с клиент делает 1-2 retry и попадает в окно
	// завершения reload'а.
	// R303 (D4): числа в отказе должны быть правдой.
	//
	// Было: `(loaded=%d)` с loadedNCtx=0 в ЛЮБОМ случае — в том числе когда
	// модель жива на другом узле. Оператор читал «being reloaded to n_ctx=32768
	// (loaded=0)» и не мог понять, что модели здесь вообще нет, а перезагрузки
	// не будет. Теперь формулировка зависит от состояния, которое мы ЗНАЕМ.
	loadedWords := fmt.Sprintf("(loaded=%d per slot)", loadedNCtx)
	switch loadedState {
	case loadedWindowAbsent:
		loadedWords = "(not loaded on this backend — a load was requested)"
	case loadedWindowUnknown:
		loadedWords = "(loaded state unknown — probe failed)"
	}
	return bodyBuf, false,
		fmt.Sprintf("model %q is being reloaded to n_ctx=%d %s; retry in 30s",
			modelName, requestedNCtx, loadedWords),
		http.StatusServiceUnavailable
}
