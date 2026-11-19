// image_resources_gate.go — R633 (2026-11-19): гейт перед генерацией: beforeGeneration, проверка готовности модели и проверка VRAM.
//
// ЗАЧЕМ ВЫНЕСЕНО. `image_resources.go` держал 2001 строку: типы и разбор
// метрик воркера, жизненный цикл опроса, гейт перед генерацией, оценка VRAM
// и замки GPU по хостам. Здесь — один из слоёв; поведение не менялось.
package balancer

import (
	"context"
	"fmt"
	"net/http"
	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
	"strings"
)

// imageGateDecision — результат «VRAM-гейт + лок GPU» для одного запроса.
type imageGateDecision struct {
	// Allowed — можно ли проксировать запрос.
	Allowed bool
	// Status/Code/Message/Hint/RetryAfterSec — готовый HTTP-отказ.
	Status        int
	Code          string
	Message       string
	Hint          string
	RetryAfterSec int
	// Verdict — решение EvaluateImageVRAM (для логов/метрик).
	Verdict types.ImageVRAMVerdict

	res    *imageResources
	holder *imageLockHolder
}

// Release — освободить лок GPU. Обязателен в defer у обработчика запроса;
// при передаче лока сторожу асинхронной генерации ничего не делает.
func (d imageGateDecision) Release() {
	if d.res == nil || d.holder == nil {
		return
	}
	d.res.releaseFromRequest(d.holder)
}

// beforeGeneration — гейт + захват лока перед проксированием генерации.
//
// Порядок шагов важен: сначала состояние модели (иначе гейт считал бы VRAM
// модели, которую воркер не держит), затем VRAM, затем лок.
func (r *imageResources) beforeGeneration(ctx context.Context, backendID string) imageGateDecision {
	allow := imageGateDecision{Allowed: true, Code: types.ImageGateOK}
	if r == nil || r.proxy == nil {
		return allow
	}
	settings := r.settings()
	backend := r.proxy.GetBackend(backendID)
	host := ""
	gpu := unknownGPU()
	if backend != nil {
		host = backend.Host
		// Индекс GPU: явный gpuIndex (в т.ч. 0 — первая карта) →
		// CppWorkerConfig.MainGPU → «неизвестно». При неизвестном индексе лок
		// остаётся хостовым — см. locksConflict.
		idx, known := backend.EffectiveGPUIndex()
		gpu = gpuRef{index: idx, known: known}
	}

	if settings.GateDisabled {
		logger.Get().Debugw("image gate: disabled by config (balancing.image.gateDisabled)",
			"backend", backendID, "host", host)
		return imageGateDecision{Allowed: true, Code: types.ImageGateDisabled}
	}

	snap := r.ensureFreshFor(ctx, backendID)

	if d, denied := r.checkImageModelReady(snap); denied {
		d.res = r
		return d
	}

	if d, denied := r.checkVRAM(snap, host, settings); denied {
		d.res = r
		return d
	}

	// Лок GPU — только в exclusive: offload/dedicated означают, что совместная
	// работа с текстом разрешена (медленнее, но LLM не блокируется).
	if settings.EffectiveCoexistencePolicy() != types.ImageCoexistenceExclusive {
		return imageGateDecision{Allowed: true, Code: types.ImageGateOK}
	}

	holder, ok, waited := r.acquireLock(ctx, backendID, host, gpu, r.loadedModelName(snap))
	if !ok {
		retryAfter := int(r.queueWait().Seconds())
		if retryAfter < 1 {
			retryAfter = 1
		}
		r.gateDeniedTotal.Add(1)
		r.mu.Lock()
		r.gateDenied[ImageGateGPUBusy]++
		r.mu.Unlock()
		logger.Get().Warnw("image GPU lock busy: generation rejected",
			"host", host, "gpu_index", gpu.index, "gpu_known", gpu.known,
			"backend", backendID,
			"waited_ms", waited.Milliseconds(),
			"queue_wait_sec", int(r.queueWait().Seconds()))
		return imageGateDecision{
			Allowed:       false,
			Status:        http.StatusTooManyRequests,
			Code:          ImageGateGPUBusy,
			Message:       fmt.Sprintf("another image generation is using the GPU on host %s", host),
			Hint:          "retry after the current generation finishes, or set balancing.image.coexistence to offload/dedicated if the image worker does not need the whole GPU",
			RetryAfterSec: retryAfter,
			res:           r,
		}
	}
	if waited > 0 {
		r.lockWaitHist.Observe(waited.Seconds())
	}
	return imageGateDecision{Allowed: true, Code: types.ImageGateOK, res: r, holder: holder}
}

// checkImageModelReady — отказ, если модель на бэкенде не готова.
//
// Почему это НЕ дублирование поведения воркера: sdworker на запрос генерации
// без загруженной модели отвечает 409 model_not_loaded (ленивой загрузки нет).
// Наш 503 с подсказкой «что сделать» полезнее клиенту, а поведение воркера при
// этом не меняется.
func (r *imageResources) checkImageModelReady(s *imageBackendMetrics) (imageGateDecision, bool) {
	allow := imageGateDecision{Allowed: true, Code: types.ImageGateOK}
	if s == nil || !s.contractOK || s.lastErr != "" {
		// Данных нет → не судим (fail-open). Сюда попадают: «голый» sd-server,
		// 401/403 (токен воркера балансер не знает), таймаут, битый JSON.
		return allow, false
	}
	if s.loaded() != nil {
		return allow, false
	}

	names := strings.Join(s.modelNames(), ", ")
	known := names
	if known == "" {
		known = "none"
	}

	if loading := s.loading(); loading != nil || s.state == imageStateLoading {
		name := s.currentModel
		if loading != nil {
			name = loading.Name
		}
		return imageGateDecision{
			Allowed:       false,
			Status:        http.StatusServiceUnavailable,
			Code:          ImageGateModelLoading,
			Message:       fmt.Sprintf("image model %q is still loading on the backend", name),
			Hint:          "wait for the load to finish: GET /api/image/models/load/progress",
			RetryAfterSec: 5,
		}, true
	}

	if failed := s.failed(); failed != nil || s.state == imageStateError {
		name := s.currentModel
		detail := ""
		if failed != nil {
			name = failed.Name
			if failed.Error != "" {
				detail = ": " + failed.Error
			}
		}
		return imageGateDecision{
			Allowed:       false,
			Status:        http.StatusServiceUnavailable,
			Code:          ImageGateModelError,
			Message:       fmt.Sprintf("image model %q failed to load%s", name, detail),
			Hint:          "check the bundle and profile (files, quantization, runtime placement), then reload: POST /api/image/models/load",
			RetryAfterSec: 10,
		}, true
	}

	return imageGateDecision{
		Allowed: false,
		Status:  http.StatusServiceUnavailable,
		Code:    ImageGateModelNotLoaded,
		Message: fmt.Sprintf("no image model is loaded on the image backend (known models: %s)", known),
		Hint:    `load a model first: POST /api/image/models/load {"name":"<model>"}`,
	}, true
}

// checkVRAM — VRAM-гейт. Возвращает (решение, отказ).
//
// ГРАНИЦА ОТВЕТСТВЕННОСТИ (после FIX-2): политику «блокировать ли неизвестную
// оценку» задаёт ЗАМОРОЖЕННЫЙ КОНТРАКТ types.EvaluateImageVRAM (по умолчанию
// неизвестная оценка пропускается, строгость — opt-in
// balancing.image.blockOnUnknownVramEstimate). Балансер не добавляет сюда своей
// политики: он лишь решает, ДОСТАТОЧНО ЛИ ДАННЫХ, чтобы вообще судить о модели.
// Если данных нет (воркер без контракта / ошибка опроса / неизвестная модель) —
// гейт молчит (fail-open): это не «неизвестная оценка модели», а отсутствие
// сведений о ней, и превращать это в 503 значило бы ломать рабочие конфигурации
// (в том числе «голый» sd-server на порту image-бэкенда).
func (r *imageResources) checkVRAM(s *imageBackendMetrics, host string, settings types.ImageResourceSettings) (imageGateDecision, bool) {
	allow := imageGateDecision{Allowed: true, Code: types.ImageGateOK}
	est := r.resolveVRAMEstimate(s)
	// R-Image (2026-10-02, найдено ЖИВЫМ прогоном): сюда мы попадаем только когда
	// модель УЖЕ загружена (dataKnown требует loaded() != nil), то есть веса
	// резидентны. Сравнивать с текущим free полный вес модели нельзя: на
	// 8-гиговой карте после загрузки SD1.5 Q4 (пик ~2.6 GB) свободно ~0.9 GB,
	// и КАЖДЫЙ запрос получал 503 insufficient_vram при полностью готовом
	// движке (в логе: required_mb=2600, free_mb=895).
	// Генерации нужны рабочие буферы (латенты, VAE-декод, внимание) — их и
	// проверяем; вопрос «влезут ли веса» решается на этапе загрузки модели
	// (там оценка сравнивается со свободной VRAM до загрузки).
	gateEst := est
	if est.IsKnown() {
		gateEst.RequiredMB = imageWorkingSetMB(est.RequiredMB)
		gateEst.Detail = est.Detail + "; weights already resident -> gate checks the working set only"
	}
	free, freeSource := r.freeVRAMFor(host, s)
	shared := r.hostHasTextNeighbour(host)
	dataKnown := s != nil && s.contractOK && s.lastErr == "" && s.loaded() != nil

	if !dataKnown {
		r.gateSkippedNoData.Add(1)
		logger.Get().Debugw("image VRAM gate skipped: no trustworthy model data",
			"backend", s.backendIDOrEmpty(),
			"host", host,
			"reason", "worker did not report the /api/image/models contract (or the poll failed)",
			"estimate_source", est.Source,
			"text_neighbour", shared)
		return allow, false
	}

	verdict := types.EvaluateImageVRAM(gateEst, free, settings)
	logger.Get().Infow("image VRAM gate decision",
		"backend", s.backendIDOrEmpty(),
		"host", host,
		"model", r.loadedModelName(s),
		"allowed", verdict.Allowed,
		"reason_code", verdict.ReasonCode,
		"required_mb", gateEst.RequiredMB,
		"weights_mb", est.RequiredMB,
		"estimate_source", est.Source,
		"estimate_detail", gateEst.Detail,
		"free_mb", free,
		"free_source", freeSource,
		"headroom_mb", settings.VramHeadroomMB,
		"text_neighbour", shared,
		"policy", string(settings.EffectiveCoexistencePolicy()))

	if !verdict.Allowed {
		r.gateDeniedTotal.Add(1)
		r.mu.Lock()
		r.gateDenied[verdict.ReasonCode]++
		r.mu.Unlock()
		decision := imageGateDecision{
			Allowed: false,
			Status:  http.StatusServiceUnavailable,
			Code:    verdict.ReasonCode,
			Message: verdict.Message,
			Hint:    verdict.Hint,
			Verdict: verdict,
		}
		// R91 (2026-10-09): на общей карте к отказу добавляем КОНТЕКСТ.
		//
		// Живой случай: текстовая модель (gemma-4) занимала карту, image-модели не
		// хватало ~8 МБ сверх резерва, и оператор видел только числа — «нужно 520,
		// свободно 512». Из них не следует ни то, КТО держит память, ни то, что
		// делать. Здесь мы знаем и то, и другое: называем текстовые бэкенды на этой
		// же карте (по host ИЛИ по общему GPU UUID — на стенде имена контейнеров
		// разные, см. textNeighbourIDs) и ведём к ручке политики, которая правится
		// без рестарта.
		if shared && verdict.ReasonCode == types.ImageGateInsufficientVRAM {
			neighbours := r.textNeighbourIDs(host)
			who := strings.Join(neighbours, ", ")
			if who == "" {
				who = "text backend on the same GPU"
			}
			decision.Message = fmt.Sprintf("%s; a text backend (%s) is occupying this GPU", verdict.Message, who)
			decision.Hint = verdict.Hint + ". The text model occupies the same GPU: either free the card " +
				"(unload the text model) or change the coexistence policy — GET/PUT /api/v1/image/resources: " +
				"\"offload\" only if the image worker really runs with offload to RAM, \"dedicated\" only if the " +
				"image worker has its own GPU; reducing vramHeadroomMb lowers the reserve the gate adds"
		}
		return decision, true
	}
	r.gateAllowed.Add(1)
	return imageGateDecision{Allowed: true, Code: verdict.ReasonCode, Verdict: verdict}, false
}
