// lazyload.go — Lazy model loading, loading progress response, and auto-load on startup.
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"ollama-loadbalancer/internal/cppbackend"
	"ollama-loadbalancer/internal/i18n"
	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

// Round 10 (2026-07-28) BUGFIX: env-var overrides для hardcoded fallback-параметров.
// До фикса: lazy-load fallback path (когда GGUF header read failed) использовал
// захардкоженные значения estimatedLayers=80, kvReserve=2GB, overhead=1.5GB,
// safetyFactor=0.85. Для MoE/35B+ моделей с <80 слоями (qwen3.6 35B = 40 слоёв)
// partial offload math давал неверный gpuLayers → OOM risk.
//
// Теперь эти параметры конфигурируются через env-vars:
//
//	CPPWORKER_FALLBACK_ESTIMATED_LAYERS — default 80 (типичная LLM, диапазон 20-200)
//	CPPWORKER_FALLBACK_KV_RESERVE_MB    — default 2048 (диапазон 256-16384)
//	CPPWORKER_FALLBACK_OVERHEAD_MB      — default 1536 (CUDA + activations)
//	CPPWORKER_FALLBACK_SAFETY_FACTOR   — default 0.85 (диапазон 0.5-1.0)
//
// Out-of-range или невалидные значения → fallback на default (без fatal).
var (
	fallbackEstimatedLayers int     = 80
	fallbackKVReserveMB     int64   = 2048
	fallbackOverheadMB      int64   = 1536
	fallbackSafetyFactor    float64 = 0.85
)

// ErrModelFileNotFound — R119 (2026-10-15): «модели нет на диске» как
// ТИПИЗИРОВАННАЯ ошибка, а не просто текст.
//
// ЗАЧЕМ. Раньше lazy-load возвращал строку «model file not found: <path>», и
// HTTP-слой не мог отличить «модель не найдена» от «внутренняя поломка»: все
// ошибки догрузки превращались в 500 Internal Server Error с ПУСТЫМ телом.
//
// ЖИВОЙ СЛУЧАЙ (стенд, 2026-10-15): OpenWebUI просил модель `llama-3-8b`,
// которой нет в каталоге, получал 500 без объяснения и повторял запрос (в логе —
// три 500 подряд с интервалом 150 мс). Оператор видел «Internal Server Error» и
// не понимал, что модель просто не найдена; ollama-совместимый клиент не мог
// показать причину, потому что тела не было вовсе.
//
// Теперь это 404 + машиночитаемое тело (см. writeModelLoadError).
var ErrModelFileNotFound = errors.New("model file not found")

// lazyLoadDetachedTimeout — R83 (2026-09-29): сколько ждать загрузку модели,
// отвязанную от клиентского запроса.
//
// Загрузка 27B Q4 на CPU-offload занимает минуты; 30 минут — верхняя граница,
// чтобы «повисшая» загрузка не держала слот вечно. Переопределяется
// CPPWORKER_LAZY_LOAD_TIMEOUT_SEC (секунды).
// lazyLoadDetachedTimeout — duration-кап на РАЗДЕЛЯЕМУЮ загрузку модели.
//
// R83/v67 (2026-10-02): по умолчанию 0 = БЕЗ капа. Раньше дефолт 30 минут
// обрывал загрузку большой модели ровно по часам: контекст отменялся, и
// llama_model_load_from_file возвращал BRIDGE_ERR_ABORTED («load cancelled»),
// хотя диск/VRAM были в порядке — оператор видел «модель не грузится».
//
// Доктрина: загрузка завершается терминальным состоянием (успех/ошибка/
// недоступность), а не таймером. Кап возвращается ТОЛЬКО явным opt-in
// CPPWORKER_LAZY_LOAD_TIMEOUT_SEC=N (секунды).
func lazyLoadDetachedTimeout() time.Duration {
	if v := os.Getenv("CPPWORKER_LAZY_LOAD_TIMEOUT_SEC"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 30 && n <= 86400 {
			logger.Get().Warnw(i18n.T("cppworker.lazyload.armed_timeout"),
				"timeout_sec", n, "source", "CPPWORKER_LAZY_LOAD_TIMEOUT_SEC")
			return time.Duration(n) * time.Second
		}
	}
	return 0
}

// loadContextForSharedLoad — R83-fix (2026-09-29): контекст для РАЗДЕЛЯЕМОЙ
// загрузки модели (одну модель грузит одна горутина, остальные запросы ждут её).
//
// Всегда отвязан от клиентского запроса и ограничен по времени: загрузка —
// общий ресурс, её нельзя убивать уходом одного клиента. Возвращённый cancel
// обязан вызвать вызывающий.
//
// Отдельной функцией — чтобы правило было покрыто тестом
// (lazyload_detached_ctx_r83_test.go): раньше здесь стояло условие
// `ctx.Done() != nil`, которое истинно как раз для клиентского r.Context(),
// из-за чего отвязка не срабатывала именно в своём сценарии.
func loadContextForSharedLoad() (context.Context, context.CancelFunc) {
	// contextWithTimeout трактует <= 0 как «без капа» (WithCancel), а не как
	// немедленно истёкший контекст — см. context_timeout.go.
	return contextWithTimeout(lazyLoadDetachedTimeout())
}

func init() {
	if v := os.Getenv("CPPWORKER_FALLBACK_ESTIMATED_LAYERS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 20 && n <= 200 {
			fallbackEstimatedLayers = n
			logger.Get().Infow("applied CPPWORKER_FALLBACK_ESTIMATED_LAYERS from env", "value", n)
		} else {
			logger.Get().Warnw("CPPWORKER_FALLBACK_ESTIMATED_LAYERS: invalid value, using default",
				"value", v, "default", fallbackEstimatedLayers, "valid_range", "20-200")
		}
	}
	if v := os.Getenv("CPPWORKER_FALLBACK_KV_RESERVE_MB"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n >= 256 && n <= 16384 {
			fallbackKVReserveMB = n
			logger.Get().Infow("applied CPPWORKER_FALLBACK_KV_RESERVE_MB from env", "value", n)
		} else {
			logger.Get().Warnw("CPPWORKER_FALLBACK_KV_RESERVE_MB: invalid value, using default",
				"value", v, "default", fallbackKVReserveMB, "valid_range", "256-16384")
		}
	}
	if v := os.Getenv("CPPWORKER_FALLBACK_OVERHEAD_MB"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n >= 256 && n <= 4096 {
			fallbackOverheadMB = n
			logger.Get().Infow("applied CPPWORKER_FALLBACK_OVERHEAD_MB from env", "value", n)
		} else {
			logger.Get().Warnw("CPPWORKER_FALLBACK_OVERHEAD_MB: invalid value, using default",
				"value", v, "default", fallbackOverheadMB, "valid_range", "256-4096")
		}
	}
	if v := os.Getenv("CPPWORKER_FALLBACK_SAFETY_FACTOR"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f >= 0.5 && f <= 1.0 {
			fallbackSafetyFactor = f
			logger.Get().Infow("applied CPPWORKER_FALLBACK_SAFETY_FACTOR from env", "value", f)
		} else {
			logger.Get().Warnw("CPPWORKER_FALLBACK_SAFETY_FACTOR: invalid value, using default",
				"value", v, "default", fallbackSafetyFactor, "valid_range", "0.5-1.0")
		}
	}
}

// ensureModelLoaded — ленивая загрузка модели из файловой системы если она ещё не в памяти.
//
// R60.57 (2026-09-13): принимает context.Context для проброса cancel в
// backend.LoadModelWithOpts. HTTP handlers передают r.Context() (через
// setupCancelTracking) — client disconnect / /api/cancel отменяет load.
//
// R62 (2026-09-14): возвращает (actualName, err) — actualName это имя под
// которым модель РЕАЛЬНО загружена в памяти. Если R60.60 dedup-pre-check
// обнаружил что файл уже загружен под alias (existingName != modelName),
// actualName = existingName. Handlers используют actualName для дальнейших
// операций (chat template lookup, Infer call), иначе получат "model not found".
//
// Возвращаемые ошибки:
//   - nil, modelName: модель уже загружена или успешно загружена сейчас
//   - "", errModelIsLoading: другая горутина уже грузит эту модель.
//     Хендлеры перехватывают эту ошибку и отвечают 503 с retryAfterMs=3000
//   - "", errors.Is(err, bridge.ErrAborted): load отменён через ctx
//     (C-bridge watcher в LoadModelWithOpts отменил load; см. PLAN.md §3)
//   - "", прочие: ошибка загрузки (validation, FS, C-bridge)
func ensureModelLoaded(ctx context.Context, modelName string) (string, error) {
	return ensureModelLoadedWithNCtx(ctx, modelName, 0)
}

// modelReadyPollInterval — период опроса состояния загрузки.
const modelReadyPollInterval = 500 * time.Millisecond

// modelReadyWaitTimeout — сколько ждать готовности модели, если запрос не задал
// собственный дедлайн. Значение переопределяется CPPWORKER_LOAD_WAIT_SEC.
//
// Почему по умолчанию так долго (15 минут): загрузка большой модели на этом
// стенде занимает минуты, а n_ctx-перезагрузка под клиента — это ТА ЖЕ загрузка.
// Ollama в такой ситуации тоже держит запрос, а не отвечает ошибкой; ранний
// отказ выглядит у клиента как «задача выполнена» (done:true) без ответа.
const modelReadyWaitTimeout = 15 * time.Minute

// modelReadyWaitLimit — предел ожидания (env CPPWORKER_LOAD_WAIT_SEC).
func modelReadyWaitLimit() time.Duration {
	if v := strings.TrimSpace(os.Getenv("CPPWORKER_LOAD_WAIT_SEC")); v != "" {
		if sec, err := strconv.Atoi(v); err == nil && sec > 0 {
			return time.Duration(sec) * time.Second
		}
	}
	return modelReadyWaitTimeout
}

// waitForModelReady — ждёт, пока модель перейдёт в StateLoaded.
//
// R113 (2026-10-13). До этой функции «модель загружена» означало «есть запись в
// b.models»: при активной загрузке/перезагрузке (n_ctx другого клиента, auto-tune,
// параллельный запрос) запись уже существует, состояние — loading, и генерация
// падала с «model X is not loaded (state: loading)». В стриминговом пути эта
// ошибка уходила клиенту как done:true, то есть как ЗАВЕРШЁННЫЙ ход без единого
// токена (живая жалоба: «COMPLETE прилетает до того, как вообще происходит
// потоковая генерация ответа»).
//
// Само ожидание живёт в cppbackend (там же состояние модели), здесь — предел из
// окружения, журнал и перевод в errModelIsLoading, который handleChat отдаёт как
// 503 + Retry-After.
func waitForModelReady(ctx context.Context, modelName string) error {
	limit := modelReadyWaitLimit()
	started := time.Now()

	done := make(chan error, 1)
	go func() {
		done <- backend.WaitForModelReady(ctx, modelName, modelReadyPollInterval, limit)
	}()

	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case err := <-done:
			switch {
			case err == nil:
				if waited := time.Since(started); waited > 2*modelReadyPollInterval {
					logger.Get().Infow(i18n.T("cppworker.lazyload.model_ready_after_wait"),
						"model", modelName, "waited_ms", waited.Milliseconds())
				}
				return nil
			case errors.Is(err, cppbackend.ErrModelIsLoading):
				logger.Get().Warnw(i18n.T("cppworker.lazyload.model_not_ready_503"),
					"model", modelName,
					"waited_ms", time.Since(started).Milliseconds(),
					"limit_sec", int(limit.Seconds()),
					"hint", i18n.T("cppworker.lazyload.model_not_ready_hint"))
				return errModelIsLoading
			default:
				return err
			}
		case <-ticker.C:
			state := "unknown"
			if st, ok := backend.ModelState(modelName); ok {
				state = string(st)
			}
			logger.Get().Infow(i18n.T("cppworker.lazyload.waiting_for_model"),
				"model", modelName, "state", state,
				"waited_ms", time.Since(started).Milliseconds(),
				"limit_sec", int(limit.Seconds()))
		}
	}
}

// loadTotalNCtxForRequest — окно НА КЛИЕНТА → СУММАРНОЕ окно загрузки модели.
//
// llama.cpp при kv_unified=false делит суммарное окно между слотами
// (n_ctx_seq = PAD(n_ctx / n_seq_max, 256)), поэтому клиенту, попросившему
// 32768 при parallel=2, нужно суммарно ~65536. Без этого пересчёта клиент
// получил бы ровно половину запрошенного.
func loadTotalNCtxForRequest(requestedPerClient, slots int) int {
	if requestedPerClient <= 0 {
		return 0
	}
	if slots < 1 {
		slots = 1
	}
	return cppbackend.PadContext(requestedPerClient) * slots
}

// chooseLazyLoadWindow — R229 (2026-10-22): окно ленивой загрузки по лестнице
// приоритета, которая раньше существовала только в комментариях:
//
//	явный num_ctx клиента  >  профиль модели  >  env CPPWORKER_CTX_SIZE
//
// Живой дефект. env-дефолт записывался в план ДО слияния с профилем, а
// applyProfileToLoadRequest заполняет только ПУСТЫЕ поля, поэтому профильное
// окно не применялось никогда: gemma-4-E4B-it-Q4_K_M (профиль 12288) поднималась
// ленивой загрузкой с 32768 (лог: load_n_ctx=32768, n_ctx_source=env).
//
// Единицы. requestedPerClient — окно НА КЛИЕНТА, поэтому переводится в суммарное
// n_ctx умножением на слоты (loadTotalNCtxForRequest). Профильное окно — итоговое
// для плана загрузки (как его задаёт панель настроек), слоты на него не
// умножаются: профиль уже описывает окно модели.
func chooseLazyLoadWindow(requestedPerClient, envDefault, slots int, prof *types.LlamaCppModelProfile) (int, string) {
	if requestedPerClient > 0 {
		if slots < 1 {
			slots = 1
		}
		return loadTotalNCtxForRequest(requestedPerClient, slots),
			fmt.Sprintf("num_ctx запроса клиента (%d на клиента × %d слота)", requestedPerClient, slots)
	}
	if prof != nil && prof.ContextLength > 0 {
		return prof.ContextLength, "профиль модели"
	}
	return envDefault, "env CPPWORKER_CTX_SIZE"
}

// ensureModelLoadedWithNCtx — R83-политика (2026-10-01): загрузка модели с
// ОКНОМ КЛИЕНТА, если клиент его явно указал.
//
// Зачем. Раньше lazy-load всегда брал окно из env (`CPPWORKER_CTX_SIZE`) и
// профиля модели, а `num_ctx` из запроса при загрузке не участвовал вовсе:
// клиент, которому нужно 32768, получал модель, загруженную на 8192 из профиля,
// и дальше начинались «перезагрузки под клиента». Пользователь формулирует это
// так: «если размер указан и модель не загружена — применять настройки клиента;
// дефолтные значения поломают всю работу».
//
// Приоритет (лестница уже реализована в sync_profile.go — профиль НЕ
// перебивает непустое значение из запроса):
//
//	явный num_ctx запроса  >  профиль модели  >  env CPPWORKER_CTX_SIZE
//
// requestedNCtx — окно НА ОДНОГО КЛИЕНТА (то, что клиент прислал в num_ctx);
// 0 = клиент окно не указывал, грузим как раньше.
//
// Если запрошенное окно физически не влезает, его уменьшит адаптивная
// стратегия/auto-tune, а rationale.AppliedNCtx покажет, что реально применилось.
func ensureModelLoadedWithNCtx(ctx context.Context, modelName string, requestedNCtx int) (string, error) {
	// 0) Если модель уже загружена И ГОТОВА — сразу выходим.
	//
	// R113 (2026-10-13): проверяем СОСТОЯНИЕ, а не факт наличия в b.models.
	// GetModel() отдаёт запись модели в любом состоянии, включая loading: раньше
	// этого хватало, чтобы считать модель загруженной, и дальше генерация падала
	// с «model X is not loaded (state: loading)». Стриминговый путь превращал эту
	// ошибку в чанк done:true → клиент (Cline) видел завершение хода, не получив
	// ни одного токена: «COMPLETE прилетает до того, как вообще происходит
	// потоковая генерация ответа». Ждём готовности — так же, как это делает
	// Ollama: запрос к загружаемой модели блокируется, а не падает.
	if info, err := backend.GetModel(modelName); err == nil {
		switch info.State {
		case cppbackend.StateLoaded:
			return modelName, nil
		case cppbackend.StateLoading:
			if err := waitForModelReady(ctx, modelName); err != nil {
				return "", err
			}
			return modelName, nil
		case cppbackend.StateError:
			// Запись осталась после неудачной загрузки: повторять её ожиданием
			// бессмысленно — отдаём понятную ошибку (балансер решает, перезагружать ли).
			return "", fmt.Errorf("model %s is in error state (previous load failed)", modelName)
		default:
			// unloaded/неизвестное: запись есть, но загрузку надо начинать —
			// уходим в обычный путь ниже (как было до R113).
		}
	}

	// R304 (2026-10-30): донор не поднимает своих копий — и по запросу чата тоже.
	//
	// ЗАЧЕМ ЗДЕСЬ, А НЕ В ОБРАБОТЧИКАХ ЧАТА. Ленивая загрузка — самая незаметная
	// дверь к локальной копии: /api/chat и /v1/* поднимают модель без всякой
	// «загрузки» в запросе, и на доноре это выглядело бы как обычная работа.
	// Проверка стоит ПОСЛЕ ветки «модель уже загружена и готова»: обслужить
	// собственную копию, оставшуюся с прошлой роли, — не преступление (генерация
	// не занимает новую память), а вот ПОДНЯТЬ ещё одну нельзя.
	if refused, mark := donorOwnCopyRefusedR304("lazy-load"); refused {
		return "", donorRefusalErrorR304(mark)
	}

	// 1) Используем TryLockLoad для постановки в очередь загрузки.
	// R303: сначала снимаем мёртвую заявку (висела бы до перезапуска процесса).
	reclaimStaleClaimR303(modelName, "lazyload")
	lockOk, lockErr := backend.TryLockLoadR303(modelName, "lazyload")
	if lockErr != nil {
		// R113: НЕ считаем это успехом «на глаз». TryLockLoad отвечает ошибкой и
		// тогда, когда модель есть в b.models, но перезагружается (состояние
		// loading) — например под n_ctx другого клиента. Ждём готовности только
		// для loading: для error/unloaded ждать нечего (ждать пришлось бы весь
		// предел ожидания впустую).
		if state, exists := backend.ModelState(modelName); exists {
			switch state {
			case cppbackend.StateLoading:
				if err := waitForModelReady(ctx, modelName); err != nil {
					return "", err
				}
				return modelName, nil
			case cppbackend.StateLoaded:
				return modelName, nil
			case cppbackend.StateError:
				return "", fmt.Errorf("model %s is in error state (previous load failed)", modelName)
			}
		}
		if err := waitForModelReady(ctx, modelName); err != nil {
			return "", err
		}
		return modelName, nil
	}
	if !lockOk {
		// Другая горутина уже грузит эту модель — ждём завершения.
		// R113: WaitForLoad смотрит на b.models (тоже без состояния), поэтому
		// решающим считаем waitForModelReady.
		// R303: ожидание ОГРАНИЧЕНО (loadWaitMaxR303) — мёртвая заявка на
		// загрузку не держит клиентский запрос бесконечно.
		if loaded, _ := backend.WaitForLoadTimeoutR303(modelName, loadWaitMaxR303()); loaded {
			if info, err := backend.GetModel(modelName); err == nil && info.State == cppbackend.StateLoaded {
				return modelName, nil
			}
		}
		if err := waitForModelReady(ctx, modelName); err != nil {
			return "", err
		}
		return modelName, nil
	}

	// lockOk == true — мы отвечаем за загрузку.
	defer backend.UnlockLoad(modelName)

	log := logger.Get()
	loadStart := time.Now()
	// R229 (2026-10-22): окно загрузки выбирается НИЖЕ — после того, как найден
	// файл модели. Причина: профиль ищется у ВЛАДЕЛЬЦА ФАЙЛА (R154), а лестница
	// приоритета (явный num_ctx клиента > профиль модели > env CPPWORKER_CTX_SIZE)
	// требует профиля ДО расчёта стратегии. До R229 env-дефолт записывался в план
	// здесь, а слияние с профилем (applyProfileToLoadRequest) заполняет только
	// ПУСТЫЕ поля — поэтому профильное окно не применялось никогда (живой случай:
	// gemma-4-E4B-it-Q4_K_M грузилась с 32768 вместо профильных 12288).
	log.Debugw(i18n.T("cppworker.lazyload.search_started"),
		"model", modelName, "requested_n_ctx", requestedNCtx, "env_default_n_ctx", *ctxSize)

	// Ищем модель в файловой системе
	mm := backend.ModelManager()
	if mm != nil {
		modelPath, err := mm.FindModelByPath(modelName)
		if err != nil {
			// R60.61 (2026-09-14): если FindModelByPath failed (алиас OpenWebUI
			// не совпадает с filename на диске) И в modelsDir ровно один .gguf —
			// используем его как fallback. Это решает случай с закешированным
			// алиасом (например "qwen3-instruct" при файле
			// "Qwen3-Instruct-2507-q4km.gguf") без явного рестарта OpenWebUI.
			if singlePath, ok := mm.SingleGGUFPath(); ok {
				log.Warnw("lazy-load: FindModelByPath failed, auto-picking single .gguf (R60.61)",
					"requested_name", modelName,
					"resolved_path", singlePath,
					"hint", "check OpenWebUI model DB — alias may be stale")
				modelPath = singlePath
			} else {
				// Fallback: конструируем путь из modelsDir
				modelPath = filepath.Join(*modelsDir, modelName)
				if !strings.HasSuffix(modelPath, ".gguf") {
					matches, globErr := filepath.Glob(modelPath + "*.gguf")
					if globErr == nil && len(matches) > 0 {
						modelPath = matches[0]
					} else {
						modelPath += ".gguf"
					}
				}
			}
		}

		// R60.61 (2026-09-14): early-fail если файла нет. Без этого
		// LoadModelWithOpts уходит в C-bridge → llama.cpp пытается mmap
		// несуществующий файл → висит (внутренний retry в llama.cpp)
		// → lock channel остаётся занят → WaitForLoad для параллельных
		// запросов блокируется навсегда. С явной ошибкой горутина
		// возвращается быстро → defer UnlockLoad → channel сигналится.
		if _, statErr := os.Stat(modelPath); statErr != nil {
			log.Errorw("lazy-load: model file not found",
				"model", modelName, "path", modelPath, "error", statErr)
			failAttempt := LoadAttempt{
				Timestamp:  loadStart,
				Model:      modelName,
				Path:       modelPath,
				Stage:      "file_not_found",
				Success:    false,
				Error:      statErr.Error(),
				DurationMs: time.Since(loadStart).Milliseconds(),
				Diagnostics: map[string]interface{}{
					"modelPath":   modelPath,
					"modelsDir":   derefString(modelsDir),
					"vramTotalMB": backendGPUVRAMTotal(),
					"vramFreeMB":  backendGPUVRAMFree(),
				},
			}
			RecordLoadAttempt(failAttempt)
			// R83 (2026-09-25): ту же причину кладём в реестр провалов — оттуда
			// её забирает agent через /api/models и показывает оператору
			// (связка cppworker → agent → webui). Раньше провал автозагрузки
			// существовал только в этом логе и в /api/diagnostics.
			loadFailures.recordDetailed(modelName, LoadFailureModelNotFound,
				fmt.Errorf("model file not found: %s", modelPath),
				failAttempt.Diagnostics)
			// R119 (2026-10-15): типизированная ошибка — иначе HTTP-слой не мог
			// отличить «модели нет» от «внутренней поломки» и отвечал 500 с
			// ПУСТЫМ телом. Живой случай (стенд): OpenWebUI просил llama-3-8b
			// (модели нет в каталоге), получал 500 Internal Server Error без
			// объяснения и повторял запрос — оператор видел «Internal Server
			// Error» и не понимал, что модель просто не найдена.
			return "", fmt.Errorf("%w: %s", ErrModelFileNotFound, modelPath)
		}

		// R229 (2026-10-22): окно загрузки — лестница приоритета
		//   явный num_ctx клиента  >  профиль модели  >  env CPPWORKER_CTX_SIZE.
		// Раньше env-дефолт попадал в план ДО слияния с профилем, а слияние
		// заполняет только пустые поля, поэтому профильное окно не применялось
		// никогда: gemma-4-E4B-it-Q4_K_M (профиль 12288) поднималась с 32768.
		// Профиль ищем у ВЛАДЕЛЬЦА ФАЙЛА (R154) — он же используется ниже при
		// слиянии остальных полей.
		winProfileName := profileLookupNameForPath(modelName, modelPath)
		var winProfile *types.LlamaCppModelProfile
		if requestedNCtx <= 0 {
			winProfile = lazyLoadProfileFor(winProfileName)
		}
		slots := 1
		if currentConfig != nil && currentConfig.DefaultNParallel > 1 {
			slots = currentConfig.DefaultNParallel
		}
		loadNCtx, loadNCtxSource := chooseLazyLoadWindow(requestedNCtx, *ctxSize, slots, winProfile)
		if winProfile != nil && winProfile.ContextLength > 0 && requestedNCtx <= 0 {
			log.Infow(i18n.T("cppworker.lazyload.window_from_profile"),
				"model", modelName, "profile", winProfileName,
				"contextLength", winProfile.ContextLength, "env_default", *ctxSize)
		}
		log.Infow("lazy-loading model from filesystem", "model", modelName,
			"load_n_ctx", loadNCtx, "n_ctx_source", loadNCtxSource)

		// === Auto-tune на LOAD (2026-06-26 BUGFIX) ===
		//
		// До этой правки lazy-load шёл с грубыми хардкодами (estimatedLayers=80,
		// kvReserve=2GB) и НЕ пересчитывал n_ctx. Для qwen3.6 (~22GB) на 20GB VRAM
		// это приводило к OOM: weights + KV-cache для n_ctx=32768 не влезают,
		// пользователь получает либо молчаливый fallback на n_ctx=4096,
		// либо ошибку. На 8GB VRAM работало, потому что модель целиком уходила
		// в mmap (gpu_layers=0) и n_ctx оставался 32768.
		//
		// Новая логика:
		//   1. Читает реальные NLayers/NEmbd/NHeads из GGUF header
		//      (cppbackend.ReadGGUFHeader через lazy ModelManager.GetModelMeta).
		//   2. Считает maxViableNCtx по реальной архитектуре.
		//   3. Stage 1: если requestedNCtx + gpuLayers влезают → exact_fit.
		//   4. Stage 2: уменьшаем gpuLayers (partial offload) — то же n_ctx.
		//   5. Stage 3: gpu_layers=0 + n_ctx = max_viable (всё через mmap).
		//
		// Отключается через CPPWORKER_AUTO_TUNE_NCTX_ON_LOAD=false.
		// Try adaptive SelectStrategy first (kvCacheType, gpu_layers, n_ctx auto-tuning)
		// Falls back to calculateLazyLoadOpts if environment profile not available.
		var opts cppbackend.LoadModelOpts
		var rationale LazyLoadRationale
		if globalEnv != nil && autoTuneNCtxOnLoadEnabled {
			env := globalEnv.Get()
			// Read GGUF meta for the model
			filename := modelName + ".gguf"
			meta, mErr := mm.GetModelMeta(filename)
			if mErr != nil {
				meta, mErr = mm.GetModelMeta(modelName)
			}
			if mErr == nil && meta != nil && meta.NLayers > 0 {
				strategy := SelectStrategy(&env, modelName, *meta, loadNCtx, *gpuLayers, currentConfig)
				opts = cppbackend.LoadModelOpts{
					GPULayers:   strategy.GPULayers,
					ContextSize: strategy.NCtx,
					BatchSize:   *batchSize,
					UseMmap:     strategy.UseMmap,
				}
				rationale = LazyLoadRationale{
					RequestedNCtx:      loadNCtx,
					RequestedGPULayers: *gpuLayers,
					AppliedNCtx:        strategy.NCtx,
					AppliedGPULayers:   strategy.GPULayers,
					AppliedUseMmap:     strategy.UseMmap,
					Source:             strategy.Stage,
					NLayers:            meta.NLayers,
					NEmbd:              meta.NEmbd,
					NHeads:             meta.NHeads,
					NKvHeads:           meta.NKvHeads,
					ModelSize:          meta.SizeBytes,
					// R83 §9.3 (2026-09-26): числа для отказа, если стратегия —
					// fallback_no_fit («не влезает даже в CPU-only»). Без них
					// InsufficientResourcesError напечатал бы нули: адаптивный путь
					// раньше отказывать не умел, поэтому поля не заполнялись.
					MaxViableNCtx:      strategy.NCtx,
					AvailableVRAMBytes: availableVRAMBytes(),
					AvailableRAMBytes:  availableRAMBytes(),
				}
				logger.Get().Infow("lazy-load: adaptive SelectStrategy applied",
					"model", modelName,
					"stage", strategy.Stage,
					"kv_cache_type", strategy.KVCacheType,
					"gpu_layers", strategy.GPULayers,
					"n_ctx", strategy.NCtx,
					"explanation", strategy.Explanation)
			} else {
				// Fallback to calculateLazyLoadOpts
				tunedOpts, calcRationale := calculateLazyLoadOpts(modelName, loadNCtx, *gpuLayers, currentConfig)
				opts = tunedOpts
				rationale = calcRationale
			}
		} else {
			tunedOpts, calcRationale := calculateLazyLoadOpts(modelName, loadNCtx, *gpuLayers, currentConfig)
			opts = tunedOpts
			rationale = calcRationale
		}
		opts.FlashAttnType = convertFlashAttn(*flashAttn)
		opts.NUMA = *numa

		// R83 §9.3 (2026-09-26): «не влезает даже в CPU-only» — это отказ, а не
		// «попробуем всё равно». До правки загрузка 16.4 GB модели в RAM
		// продолжалась, вытесняя всё остальное, и клиент видел OOM/таймаут
		// вместо причины. Ошибка типизирована (insufficient_resources):
		// handleInferenceError отдаёт 413 + structured JSON, балансер
		// проксирует его клиенту.
		if fitErr := insufficientResourcesFromFallbackNoFit(modelName, rationale); fitErr != nil {
			log.Errorw("lazy-load refused: model does not fit even in CPU-only mode",
				"model", modelName, "path", modelPath,
				"model_size_mb", rationale.ModelSize/(1024*1024),
				"available_ram_mb", rationale.AvailableRAMBytes/(1024*1024),
				"available_vram_mb", rationale.AvailableVRAMBytes/(1024*1024),
				"requested_n_ctx", rationale.RequestedNCtx,
				"max_viable_n_ctx", rationale.MaxViableNCtx)
			attempt := LoadAttempt{
				Timestamp:  loadStart,
				Model:      modelName,
				Path:       modelPath,
				Stage:      "insufficient_resources",
				Success:    false,
				Error:      fitErr.Error(),
				DurationMs: time.Since(loadStart).Milliseconds(),
			}
			RecordLoadAttempt(attempt)
			return "", fitErr
		}

		logger.Get().Infow("lazy-load: load opts calculated",
			"model", modelName,
			"rationale", rationale.FormatRationale())

		// Legacy auto-offload остался для обратной совместимости: если
		// CPPWORKER_AUTO_TUNE_NCTX_ON_LOAD=false, старая логика всё равно
		// отрабатывает. Round 10 (2026-07-28) BUGFIX: hardcoded константы
		// (estimatedLayers, kvReserve, overhead, safetyFactor) вынесены в
		// env-vars — см. CPPWORKER_FALLBACK_* в init() ниже.
		if *autoOffload && rationale.Source == "fallback_no_meta" {
			availableVRAM := availableVRAMBytes()
			if availableVRAM > 0 {
				filename := modelName + ".gguf"
				if meta, mErr := mm.GetModelMeta(filename); mErr == nil && meta.SizeBytes > 0 {
					safeVRAM := int64(float64(availableVRAM) * fallbackSafetyFactor)
					overhead := fallbackOverheadMB * 1024 * 1024
					// Резерв под KV-cache для n_ctx=32768 (configurable).
					kvReserve := fallbackKVReserveMB * 1024 * 1024
					availableForWeights := safeVRAM - overhead - kvReserve
					if availableForWeights > 0 && meta.SizeBytes > availableForWeights {
						// Модель не влезает целиком — partial offload.
						// Используем configurable оценку (default 80 для типичной LLM).
						estimatedLayers := fallbackEstimatedLayers
						weightsPerLayer := meta.SizeBytes / int64(estimatedLayers)
						if weightsPerLayer > 0 {
							gpuLayers := int(availableForWeights / weightsPerLayer)
							if gpuLayers > 0 && gpuLayers < estimatedLayers {
								logger.Get().Infow("lazy-load: auto-offload (legacy, file-based estimate)",
									"model", modelName,
									"old_gpu_layers", opts.GPULayers,
									"new_gpu_layers", gpuLayers,
									"model_size_mb", meta.SizeBytes/(1024*1024),
									"available_for_weights_mb", availableForWeights/(1024*1024),
									"estimated_layers", estimatedLayers,
									"vram_total_mb", availableVRAM/(1024*1024),
									"safety_factor", fallbackSafetyFactor,
									"kv_reserve_mb", fallbackKVReserveMB,
									"overhead_mb", fallbackOverheadMB)
								opts.GPULayers = gpuLayers
								opts.UseMmap = true
							}
						}
					}
				}
			}
		}

		// Записываем попытку в диагностический буфер (для /api/diagnostics).
		attempt := LoadAttempt{
			Timestamp: loadStart,
			Model:     modelName,
			Path:      modelPath,
			Stage:     "loading",
		}

		// R60.60 (2026-09-14): dedup-by-path pre-check ДО LoadModelWithOpts.
		// Если другая goroutine (или предыдущий trigger) уже загрузила этот
		// файл под другим именем — return success сразу, defer UnlockLoad
		// (line 128) release lock. Без этого LoadModelWithOpts сам сделает
		// dedup и вернёт *AlreadyLoadedAsError — но мы потеряем время на
		// запуск C-bridge llama_model_load_from_file + повторную проверку.
		// В RACE case (модель ещё загружается под другим именем) WaitForLoad
		// возвращает false → errModelIsLoading → 503 → OpenWebUI retry cascade.
		if existingName, _, found := backend.GetModelByPath(modelPath); found && existingName != modelName {
			log.Infow("ensureModelLoaded: dedup-by-path pre-check (R60.60)",
				"requested_name", modelName,
				"existing_name", existingName,
				"path", modelPath)
			return existingName, nil
		}

		// R83 (2026-09-29): загрузку НЕЛЬЗЯ привязывать к ctx конкретного клиента.
		//
		// ДЕФЕКТ (воспроизведён на живом стенде). Загрузка модели — разделяемый
		// ресурс: при параллельных запросах грузит ОДНА горутина (TryLockLoad выше),
		// остальные ждут её через WaitForLoad. Но ctx передавался от клиента, который
		// эту загрузку инициировал, поэтому обрыв/завершение ЕГО запроса отменяло
		// общую загрузку:
		//
		//	lazy-load failed: load cancelled: model load aborted by user
		//	(R60.57 checkpoint A: after llama_model_load_from_file)
		//	BRIDGE_ERR_ABORTED
		//
		// В итоге модель не загружалась НИ У КОГО: инициатор получал 500
		// («upstream returned HTTP 500»), а ожидавшие — 503 «model is not loaded and
		// auto-load failed». Именно это наблюдалось при трёх параллельных запросах:
		// первый обслуживался, второй и третий падали, хотя модель физически
		// загружалась.
		//
		// Теперь загрузка идёт на отдельном контексте с ограничением по времени:
		// завершение клиентского запроса её не убивает. Явный /api/cancel по-прежнему
		// не может прервать загрузку — это осознанный компромисс: ждать догрузки
		// лучше, чем остаться без модели из-за ушедшего клиента.
		//
		// R83-fix (2026-09-29, живой стенд). Предыдущая версия проверяла
		// `loadCtx.Done() != nil` — а это истина ровно для CANCELLABLE контекста,
		// то есть для клиентского r.Context(). Ветка «отвязать» срабатывала
		// только для context.Background() (у него Done() == nil) и НЕ срабатывала
		// для того самого случая, от которого защищала. Воспроизведение: три
		// параллельных /api/chat к НЕ загруженной модели → инициатор уходил по
		// таймауту → общая загрузка обрывалась («load cancelled: model load
		// aborted by user ... BRIDGE_ERR_ABORTED») → 503 у остальных.
		//
		// Отвязываем ВСЕГДА: загрузка — разделяемый ресурс, её контекст не может
		// принадлежать ни одному из клиентов.
		// R83/v52 (2026-10-01): ленивая загрузка обязана уважать профиль модели —
		// тот же, что применяет балансер при auto-load.
		//
		// ДЕФЕКТ (замер стенда). handleLoadModel/handleReloadModel вызывают
		// applyProfileToLoadRequest, а этот путь — нет. Прямой запрос к cppworker
		// (WebUI, диагностика, клиент мимо балансера) поднимал модель на
		// env-дефолтах контейнера: kvCacheType=f16 вместо профильного q4_0. KV
		// вырастал вчетверо, memfit видел «не влезает» и оставлял 19 слоёв из 42
		// на CPU — tokenLatencyMs 1035 (≈1 с/токен) против 125 мс у инстанса,
		// загруженного балансером. Гейт n_ctx при этом уже считал KV по профилю
		// (memfit_gate.effectiveKVCacheTypeForLoad) — то есть решение принималось
		// по одному типу KV, а загрузка шла с другим.
		//
		// Профиль только ДОПОЛНЯЕТ: applyProfileToLoadRequest заполняет поля,
		// которые остались пустыми/нулевыми, поэтому окно от клиента
		// (opts.ContextSize) не перезаписывается.
		if err := applySyncedProfileToLazyLoadOpts(winProfileName, &opts); err != nil {
			return "", err
		}

		loadCtx, cancelLoad := loadContextForSharedLoad()
		defer cancelLoad()

		if err := backend.LoadModelWithOpts(loadCtx, modelName, modelPath, opts); err != nil {
			log.Errorw("lazy-load failed", "model", modelName, "path", modelPath, "error", err)
			attempt.Stage = "load_failed"
			attempt.Success = false
			attempt.Error = err.Error()
			attempt.DurationMs = time.Since(loadStart).Milliseconds()
			attempt.Diagnostics = map[string]interface{}{
				"modelPath":   modelPath,
				"gpuLayers":   opts.GPULayers,
				"ctxSize":     opts.ContextSize,
				"batchSize":   opts.BatchSize,
				"useMmap":     opts.UseMmap,
				"modelsDir":   derefString(modelsDir),
				"vramTotalMB": backendGPUVRAMTotal(),
				"vramFreeMB":  backendGPUVRAMFree(),
			}
			RecordLoadAttempt(attempt)
			return "", fmt.Errorf("failed to load model %s: %w", modelName, err)
		}

		log.Infow("lazy-load successful", "model", modelName, "path", modelPath,
			"durationMs", time.Since(loadStart).Milliseconds())
		attempt.Stage = "complete"
		attempt.Success = true
		attempt.DurationMs = time.Since(loadStart).Milliseconds()
		attempt.Diagnostics = map[string]interface{}{
			"modelPath": modelPath,
		}
		RecordLoadAttempt(attempt)
		return modelName, nil
	}

	// mm == nil — ModelManager не инициализирован.
	attempt := LoadAttempt{
		Timestamp: loadStart,
		Model:     modelName,
		Stage:     "model_manager_nil",
		Success:   false,
		Error:     "model manager not initialized",
	}
	RecordLoadAttempt(attempt)
	return "", fmt.Errorf("model %s not found in filesystem", modelName)
}

// backendGPUVRAMTotal возвращает суммарную VRAM по всем GPU (для диагностики).
func backendGPUVRAMTotal() uint64 {
	if backend == nil {
		return 0
	}
	var total uint64
	for _, d := range backend.GetGPUDevices() {
		total += d.VRAMTotalMB
	}
	return total
}

// backendGPUVRAMFree возвращает суммарную свободную VRAM по всем GPU.
func backendGPUVRAMFree() uint64 {
	if backend == nil {
		return 0
	}
	var free uint64
	for _, d := range backend.GetGPUDevices() {
		free += d.VRAMFreeMB
	}
	return free
}

// applySyncedProfileToLazyLoadOpts — R83/v52 (2026-10-01): дополнить план ленивой
// загрузки профилем модели (pull-sync с балансера).
//
// ДЕФЕКТ (замер стенда). handleLoadModel/handleReloadModel вызывают
// applyProfileToLoadRequest, а путь ленивой загрузки — нет. Прямой запрос к
// cppworker (WebUI, диагностика, клиент мимо балансера) поднимал модель на
// env-дефолтах контейнера: kvCacheType=f16 вместо профильного q4_0. KV вырастал
// вчетверо, memfit видел «не влезает» и оставлял 19 слоёв из 42 на CPU —
// tokenLatencyMs 1035 (≈1 с/токен) против 125 мс у инстанса, который поднял
// балансер. Гейт n_ctx при этом уже считал KV по профилю
// (memfit_gate.effectiveKVCacheTypeForLoad): решение принималось по одному типу
// KV, а загрузка шла с другим.
//
// Профиль только ДОПОЛНЯЕТ план: applyProfileToLoadRequest заполняет пустые поля,
// поэтому окно, выбранное под клиента (opts.ContextSize), не перезаписывается.
func applySyncedProfileToLazyLoadOpts(modelName string, opts *cppbackend.LoadModelOpts) error {
	if opts == nil || profileSyncer == nil {
		logger.Get().Warnw(i18n.T("cppworker.lazyload.profiles_unavailable"), "model", modelName)
		return nil
	}
	// R228: пустой кэш профилей — не «нет профиля» (см. lazyLoadProfileFor).
	prof := lazyLoadProfileFor(modelName)
	if prof == nil {
		logger.Get().Warnw(i18n.T("cppworker.lazyload.profile_not_found"), "model", modelName,
			"profiles_known", profileSyncer.snapshotSize())
		return nil
	}
	logger.Get().Infow(i18n.T("cppworker.lazyload.applying_profile"), "model", modelName,
		"contextLength", prof.ContextLength, "numGpuLayers", prof.NumGPULayers,
		"kvCacheType", prof.KVCacheType, "parallel", prof.Parallel)
	ctxSize, batch, gpuLayers := opts.ContextSize, opts.BatchSize, opts.GPULayers
	flashAttn, kvCacheType := opts.FlashAttnType, opts.KVCacheType
	// R135 (2026-10-16): parallel из профиля — иначе ленивая загрузка поднимала
	// модель с одним слотом, хотя оператор выставил два (живой замер инструмента
	// scripts/e2e/02-model-params.js: профиль parallel=2 → maxSlots=1).
	parallel := opts.Parallel
	if !applyProfileToLoadRequest(prof, &ctxSize, &batch, &gpuLayers, &flashAttn, &kvCacheType, &parallel) {
		return fmt.Errorf("model %s is marked as disabled in profile", modelName)
	}
	if parallel != opts.Parallel {
		logger.Get().Infow(i18n.T("cppworker.lazyload.parallel_from_profile"),
			"model", modelName, "parallel", parallel, "was", opts.Parallel)
	}
	if kvCacheType != opts.KVCacheType {
		logger.Get().Infow(i18n.T("cppworker.lazyload.kv_cache_from_profile"),
			"model", modelName,
			"kv_cache_type", kvCacheType,
			"was", opts.KVCacheType)
	}
	opts.ContextSize, opts.BatchSize, opts.GPULayers = ctxSize, batch, gpuLayers
	opts.FlashAttnType, opts.KVCacheType = flashAttn, kvCacheType
	opts.Parallel = parallel
	// R83/v67 (2026-10-02): per-model reasoning из профиля при lazy-load.
	// Иначе LoadModelOpts.EnableReasoning оставался nil → cppbackend брал
	// cfg.DefaultEnableReasoning=false, и модель грузилась БЕЗ размышлений,
	// хотя галочка в WebUI стоит. Явный запрос (opts уже задан) приоритетнее.
	if opts.EnableReasoning == nil && prof.EnableReasoning != nil {
		opts.EnableReasoning = prof.EnableReasoning
		logger.Get().Infow(i18n.T("cppworker.lazyload.enable_reasoning_from_profile"),
			"model", modelName, "enable_reasoning", *prof.EnableReasoning)
	}
	return nil
}

// writeLoadingResponse — хелпер: отвечает 503 Service Unavailable с JSON
// {"error":"model is loading","loading":true,"retryAfterMs":3000,"elapsedMs":N}.
// Используется всеми хендлерами, которые вызвали ensureModelLoaded и получили
// errModelIsLoading.
func writeLoadingResponse(w http.ResponseWriter, modelName string, loadErr error) {
	elapsedMs, retryAfter := loadingInfoFor(loadErr)
	w.Header().Set("Retry-After", strconv.Itoa(retryAfter/1000))
	writeJSON(w, http.StatusServiceUnavailable, map[string]interface{}{
		"error":        "model is loading: " + modelName,
		"loading":      true,
		"model":        modelName,
		"elapsedMs":    elapsedMs,
		"retryAfterMs": retryAfter,
	})
}

// autoLoadModels сканирует modelsDir и загружает все найденные .gguf файлы при старте.
func autoLoadModels(cfg cppbackend.Config) {
	log := logger.Get()
	modelsDir := cfg.ModelsDir

	// R304 (2026-10-30): «загрузить всё при старте» — самый широкий способ
	// создать копии, и на доноре он запрещён целиком, а не по одной модели: у
	// донора нет своих моделей ВООБЩЕ.
	//
	// ПОЧЕМУ ПРОВЕРКА РАБОТАЕТ НА СТАРТЕ. Роль приезжает приказом позже, но
	// применённое состояние (backendRole и rpcServerEnabled) лежит в файле
	// операторских переопределений и читается ДО этой горутины (main.go:
	// applyRuntimeOverrides → NewBackend). То есть машина, перезапущенная как
	// донор, не начнёт с загрузки всех .gguf в каталоге — именно этого требовала
	// доктрина.
	if refused, mark := donorOwnCopyRefusedR304("auto-load"); refused {
		log.Warnw(i18n.T("cppworker.donor.autoload_skipped"),
			"dir", modelsDir, "role", mark.Role, "reason", mark.Reason)
		return
	}

	entries, err := os.ReadDir(modelsDir)
	if err != nil {
		log.Warnw("auto-load: failed to read models directory", "dir", modelsDir, "error", err)
		return
	}

	loaded := 0
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasSuffix(strings.ToLower(name), ".gguf") {
			continue
		}
		modelPath := filepath.Join(modelsDir, name)
		modelName := strings.TrimSuffix(name, ".gguf")

		opts := cppbackend.LoadModelOpts{
			GPULayers:     cfg.DefaultGPULayers,
			ContextSize:   cfg.DefaultCtxSize,
			BatchSize:     cfg.DefaultBatchSize,
			FlashAttnType: cfg.DefaultFlashAttnType,
			NUMA:          cfg.DefaultNUMA,
			UseMmap:       cfg.DefaultUseMmap,
			// Round 15 (2026-07-29): propagate defaultNParallel to LoadModelOpts
			// (bugfix: до этого поле Parallel оставалось 0 → SlotManager создавался
			// с maxSlots=1, multi-slot state isolation не работала). 0 = inherit
			// bridge default (=1).
			Parallel:    cfg.DefaultNParallel,
			KVCacheType: cfg.DefaultKVCacheType,
		}

		log.Infow("auto-loading model",
			"name", modelName,
			"path", modelPath,
			"gpuLayers", opts.GPULayers)

		reclaimStaleClaimR303(modelName, "auto-load")
		lockOk, lockErr := backend.TryLockLoadR303(modelName, "auto-load")
		if lockErr != nil {
			log.Infow("auto-load: model already loaded", "name", modelName)
			loaded++
			continue
		}

		if !lockOk {
			log.Infow("auto-load: another goroutine is loading this model, waiting",
				"name", modelName, "wait_limit_sec", loadWaitMaxR303().Seconds())
			// R303: ограниченное ожидание — мёртвая заявка не блокирует старт.
			okWait, timedOut := backend.WaitForLoadTimeoutR303(modelName, loadWaitMaxR303())
			if okWait {
				loaded++
				log.Infow("auto-load: model loaded by another goroutine", "name", modelName)
			} else if timedOut {
				log.Warnw("auto-load: still loading after limit, skipping",
					"name", modelName, "wait_limit_sec", loadWaitMaxR303().Seconds())
			} else {
				log.Warnw("auto-load: wait for model failed, skipping", "name", modelName)
			}
			continue
		}

		// R60.57: auto-load uses context.Background() — startup path,
		// no HTTP request to bind to. Load нельзя отменить извне (но watcher
		// в LoadModelWithOpts готов на случай будущего shutdown signal).
		if err := backend.LoadModelWithOpts(context.Background(), modelName, modelPath, opts); err != nil {
			backend.UnlockLoad(modelName)
			log.Errorw("auto-load: failed to load model",
				"name", modelName, "path", modelPath, "error", err)
			continue
		}
		backend.UnlockLoad(modelName)
		loaded++
		log.Infow("auto-loaded model successfully", "name", modelName)
	}

	if loaded > 0 {
		log.Infow("auto-load complete", "loaded", loaded, "dir", modelsDir)
	} else {
		log.Infow("auto-load: no .gguf files found", "dir", modelsDir)
	}
}
