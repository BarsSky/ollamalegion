// degraded_load.go — R83 §9.4 шаг 1б, вариант B+D (2026-09-26): «загрузили, но
// деградировали» — это НЕ провал загрузки.
//
// РЕШЕНИЕ. Единственный случай, когда загрузка физически невозможна, — это
// memfit.Stage == StageDoesNotFit («не влезает даже в CPU-only»): веса не
// помещаются в RAM вместе с KV-кэшем. Только он даёт отказ (HTTP 413,
// insufficient_resources). Вариант B — отказывать по 413 ровно в этом случае,
// не пытаясь грузить в переподписку; вариант D — StageCPUOnly (веса влезли в
// RAM, но ни один слой не влез в VRAM) загрузку НЕ блокирует: модель грузится и
// работает, просто медленно, а оператор получает уведомление и заголовки.
//
// ПОЧЕМУ НЕ ОТКАЗЫВАЕМ НА cpu_only. Аргумент «хватает памяти — грузим несмотря
// ни на что» здесь верен буквально: RAM достаточно, llama.cpp отработает на CPU,
// инференс будет медленным, но корректным. Отказ превратил бы рабочую
// конфигурацию в нерабочую. Симметрично: отказ на does_not_fit не «попытка» —
// она гарантированно упирается в OOM/таймаут, и клиент получает таймаут вместо
// причины (именно этот класс проблем закрывал §9.3).
//
// ЧТО ВИДИТ ОПЕРАТОР (D):
//   - модель загружена (это главное — она работает);
//   - уведомление в WebUI с числами: «загружено в режиме CPU-only ...»;
//   - X-CppWorker-Degraded-Stage: cpu_only в ответе на load/reload.
//
// ЧТО ЗАПИСЫВАЕТСЯ В РЕЕСТР (load_failures). Запись о провале создавать НЕЛЬЗЯ:
// /api/models отдаёт её как load_failure, agent пересылает в метриках, а
// балансер по ней публикует «Не хватило памяти» и позже «Загрузка модели
// восстановлена» (см. internal/balancer/load_failure_events.go). Для cpu_only
// загрузка УДАЛАСЬ — событие «провал» было бы ложью. Поэтому деградация живёт в
// отдельном регистре (Severity=warning, Degraded=true) и попадает в /api/models
// полем load_degraded.
package main

import (
	"net/http"
	"strconv"
	"sync"
	"time"

	"ollama-loadbalancer/internal/cppbackend"
	"ollama-loadbalancer/internal/memfit"
)

// DegradedNotice — запись о том, что модель загружена в деградированном режиме.
//
// Несёт те же числа, что и вердикт memfit: оператор должен видеть, ПОЧЕМУ
// раскладка такая (веса не влезли в VRAM / не хватило RAM на CPU-часть), а не
// только «cpu_only».
type DegradedNotice struct {
	Model   string
	Stage   string
	Reason  string
	Detail  string
	At      time.Time
	Numbers map[string]interface{}
}

// IsEmpty — есть ли что показывать (nil-safe, как LoadFailureInfo.IsEmpty).
func (d *DegradedNotice) IsEmpty() bool {
	return d == nil || d.Model == "" || d.Stage == ""
}

// degradedStageFromVerdict — стадия деградации по вердикту memfit.
//
// Возвращает пустую строку для всех НЕ-деградированных вердиктов (exact_fit,
// partial_offload, does_not_fit, unknown). Только cpu_only означает «загрузим,
// но без GPU».
func degradedStageFromVerdict(v memfit.Verdict) string {
	if v.Stage == memfit.StageCPUOnly {
		return string(memfit.StageCPUOnly)
	}
	return ""
}

// setDegradedHeader — пометить HTTP-ответ как деградированный.
//
// Заголовок, а не только событие: балансер и WebUI уже читают заголовки ответа
// загрузки (X-Model-Capabilities, X-Model-Max-Context), поэтому клиент видит
// режим сразу, не дожидаясь метрик. Caller обязан вызвать ДО WriteHeader —
// иначе заголовок потеряется молча (это главная ловушка).
func setDegradedHeader(w http.ResponseWriter, stage string) {
	if w == nil || stage == "" {
		return
	}
	w.Header().Set("X-CppWorker-Degraded", "insufficient_resources")
	w.Header().Set("X-CppWorker-Degraded-Stage", stage)
}

// forceReloadRequest — «оператор настоял» (escape-hatch для отказа по памяти).
//
// Два равнозначных способа: поле force в JSON-теле (так шлёт балансер) и
// ?force=true в query (так удобнее curl/WebUI). Логика одна и та же, поэтому
// вынесена сюда: расхождение «в теле работает, в query нет» — это баг, который
// иначе всплывает только в инциденте.
func forceReloadRequest(r *http.Request, bodyForce *bool) bool {
	if bodyForce != nil && *bodyForce {
		return true
	}
	if r != nil && r.URL != nil && r.URL.Query().Get("force") == "true" {
		return true
	}
	return false
}

// degradedHeadersFromNotice — заголовки по записи регистра (для путей, где
// стадия известна только после загрузки).
func degradedHeadersFromNotice(w http.ResponseWriter, d *DegradedNotice) {
	if d.IsEmpty() {
		return
	}
	w.Header().Set("X-CppWorker-Degraded", d.Reason)
	w.Header().Set("X-CppWorker-Degraded-Stage", d.Stage)
}

// insErrFromVerdict — InsufficientResourcesError из вердикта memfit.
//
// Числа берутся из того же вердикта, что показан в логе (Verdict.String()), —
// поэтому поле «max_viable_n_ctx» в ответе и строка в логе не могут разойтись.
// Запрошенный n_ctx передаётся отдельно: в Verdict его нет (он есть в Request,
// который вызывающий уже собрал).
func insErrFromVerdict(modelName string, v memfit.Verdict, requestedNCtx int) *InsufficientResourcesError {
	err := &InsufficientResourcesError{
		Model:              modelName,
		RequestedNCtx:      requestedNCtx,
		MaxViableNCtx:      v.MaxHardCtx,
		AvailableVRAMMB:    v.UsableVRAM.MiB(),
		AvailableRAMMB:     v.UsableRAM.MiB(),
		ModelSizeBytes:     int64(v.Weights),
		KVCacheRequiredMB:  int64(v.KVTotal) / (1024 * 1024),
		GPULayersAttempted: v.GPULayers,
	}
	if v.HasReason(memfit.ReasonRAMShort) {
		if detail := v.ReasonDetail(memfit.ReasonRAMShort); detail != "" {
			err.Reason = "not_enough_ram"
			err.Suggestion = "Модель не влезает в RAM целиком: " + detail +
				". Используйте меньшую модель или квантование посильнее."
		}
	} else if v.HasReason(memfit.ReasonWeightsExceedVRAM) {
		if detail := v.ReasonDetail(memfit.ReasonWeightsExceedVRAM); detail != "" {
			err.Reason = "weights_exceed_vram"
			err.Suggestion = "Веса не влезают в VRAM: " + detail +
				". Уменьшите gpuLayers или n_ctx."
		}
	} else if v.HasReason(memfit.ReasonKVQuantLever) {
		err.Reason = "kv_too_large"
		err.Suggestion = "KV-кэш не влезает: " + v.Suggestion +
			". Квантование KV (q8_0/q4_0) может помочь."
	}
	if err.MaxViableNCtx > 0 && err.RequestedNCtx > 0 && err.MaxViableNCtx < err.RequestedNCtx {
		base := "Reduce num_ctx to " + strconv.Itoa(err.MaxViableNCtx) + " or use a smaller model."
		if err.Suggestion != "" {
			err.Suggestion = err.Suggestion + " " + base
		} else {
			err.Suggestion = base
		}
	}
	return err
}

// degradedNoticeFromVerdict — запись регистра из вердикта memfit.
//
// Числа в Numbers — те же, что в логе вердикта и в /api/models, поэтому три
// источника (лог, HTTP, событие) не могут разойтись.
func degradedNoticeFromVerdict(modelName string, v memfit.Verdict, nCtx int) *DegradedNotice {
	stage := degradedStageFromVerdict(v)
	if stage == "" {
		return nil
	}
	d := &DegradedNotice{
		Model:  modelName,
		Stage:  stage,
		Reason: "insufficient_resources",
		At:     time.Now().UTC(),
		Numbers: map[string]interface{}{
			"requested_n_ctx":   nCtx,
			"kv_cache_type":     string(v.KVType),
			"gpu_layers":        v.GPULayers,
			"total_layers":      v.TotalLayers,
			"weights_mb":        v.Weights.MiB(),
			"kv_total_mb":       v.KVTotal.MiB(),
			"usable_vram_mb":    v.UsableVRAM.MiB(),
			"usable_ram_mb":     v.UsableRAM.MiB(),
			"max_hard_n_ctx":    v.MaxHardCtx,
			"max_exact_fit_ctx": v.MaxExactFitCtx,
		},
	}
	if !v.CeilingsKnown {
		d.Numbers["ceilings_known"] = false
	}
	for _, r := range v.Reasons {
		if r.Detail == "" {
			continue
		}
		d.Detail = string(r.Code) + ": " + r.Detail
		break
	}
	return d
}

// noteLoadDegradation — обновить регистр деградации ПОСЛЕ успешной загрузки.
//
// Вызывается из фоновых путей (runAsyncLoad / runAsyncReload) — то есть там,
// где ответ клиенту уже ушёл и заголовок поставить некуда. Запись здесь — это
// то, что позже увидит agent в /api/models (поле load_degraded) и превратит в
// уведомление оператору.
//
// Числа берём тем же путём, что и решение (memfitPlanForModel), а не из opts:
// opts не содержит размера модели и числа слоёв, и «самодельная» запись
// разошлась бы с логом вердикта. Если метаданных нет — записываем минимальную
// достоверную запись (стадия cpu_only + gpu_layers=0), не выдумывая числа.
func noteLoadDegradation(modelName string, opts cppbackend.LoadModelOpts, be *cppbackend.Backend) {
	if modelName == "" || be == nil {
		return
	}
	if opts.GPULayers != 0 {
		// Хотя бы один слой на GPU (или AUTO, который решится при загрузке):
		// модель НЕ деградирована — снимаем прежнюю запись, если была.
		degradedLoads.clear(modelName)
		return
	}
	info, err := be.GetModel(modelName)
	if err != nil || info == nil {
		return
	}
	if v, ok := memfitPlanForModel(*info, opts.KVCacheType); ok {
		if d := degradedNoticeFromVerdict(modelName, v, info.ContextSize); d != nil {
			// Числа из вердикта: те же, что в логе раскладки. Reason оставляем
			// штатный (insufficient_resources) — именно его читает балансер.
			degradedLoads.record(d)
			return
		}
		// Вердикт есть, но он НЕ cpu_only: значит загрузка с нулём слоёв —
		// явное намерение оператора (gpuLayers=0 в теле запроса), а не
		// деградация. Оператор знает, что делает, и предупреждать его не о чем.
		degradedLoads.clear(modelName)
		return
	}
	degradedLoads.record(&DegradedNotice{
		Model:  modelName,
		Stage:  string(memfit.StageCPUOnly),
		Reason: "cpu_only",
		At:     time.Now().UTC(),
		Numbers: map[string]interface{}{
			"gpu_layers": opts.GPULayers,
			"n_ctx":      opts.ContextSize,
		},
	})
}

// degradedLoadTTL — сколько держим запись о деградации.
//
// Дольше, чем провал (10 мин): деградация — это СОСТОЯНИЕ работающей модели, а
// не мгновенное событие. Пока модель работает на CPU, оператор должен видеть,
// почему; запись снимается успешной нормальной загрузкой или unload'ом.
const degradedLoadTTL = 30 * time.Minute

// degradedLoadRegistry — потокобезопасный реестр деградированных загрузок.
//
// Отдельный от loadFailureRegistry осознанно: там «провал», здесь «успех с
// оговоркой». Смешивание привело бы к ложному уведомлению «модель не
// загрузилась» (см. шапку degraded_load.go).
type degradedLoadRegistry struct {
	m   map[string]*DegradedNotice
	now func() time.Time
	ttl time.Duration
	mu  sync.Mutex
}

func newDegradedLoadRegistry(ttl time.Duration) *degradedLoadRegistry {
	return &degradedLoadRegistry{
		m:   make(map[string]*DegradedNotice),
		ttl: ttl,
		now: time.Now,
	}
}

// record — запомнить деградацию модели (перезаписывает прежнюю запись).
func (r *degradedLoadRegistry) record(d *DegradedNotice) {
	if r == nil || d.IsEmpty() {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.m[d.Model] = d
}

// clear — забыть деградацию (успешная нормальная загрузка, unload).
func (r *degradedLoadRegistry) clear(name string) {
	if r == nil || name == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.m, name)
}

// get — деградация модели, если не истекла по TTL.
func (r *degradedLoadRegistry) get(name string) (*DegradedNotice, bool) {
	if r == nil || name == "" {
		return nil, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	d, ok := r.m[name]
	if !ok {
		return nil, false
	}
	if r.ttl > 0 && r.now().Sub(d.At) > r.ttl {
		delete(r.m, name)
		return nil, false
	}
	return d, true
}

// latest — самая свежая деградация (для /api/models: agent пересылает одну).
func (r *degradedLoadRegistry) latest() (*DegradedNotice, bool) {
	if r == nil {
		return nil, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	var best *DegradedNotice
	for name, d := range r.m {
		if r.ttl > 0 && now.Sub(d.At) > r.ttl {
			delete(r.m, name)
			continue
		}
		if best == nil || d.At.After(best.At) {
			best = d
		}
	}
	return best, best != nil
}

// degradedLoads — процесс-глобальный реестр (cppworker обслуживает один backend).
var degradedLoads = newDegradedLoadRegistry(degradedLoadTTL)
