package types

import "fmt"

// R-Image (2026-10-02): политики сосуществования image-генерации с текстовым
// инференсом и бюджет VRAM.
//
// ПРОБЛЕМА, КОТОРУЮ ЭТО РЕШАЕТ: диффузионная модель (FLUX Q4 — пики 3.7–6.4 GB,
// Q8 — до 12 GB) и текстовая LLM на одной карте в VRAM обычно не сосуществуют.
// Если балансер об этом не знает, он выдаёт текстовый слот во время генерации
// (или наоборот), и одна из сторон падает с OOM уже внутри движка — то есть
// ошибкой, которую клиент видит как «сломался бэкенд», а не как нехватку памяти.
//
// Поэтому: (1) оценка VRAM для image-модели + гейт «не влезает» → 503
// insufficient_vram с подсказкой по OOM-лестнице, (2) политика сосуществования
// (по умолчанию exclusive — генерация владеет картой), (3) учёт headroom, чтобы
// не выесть память под текстовые слоты.
//
// Дополняет, а не заменяет: internal/memfit (KV/n_ctx для LLM) для диффузии
// НЕПРИМЕНИМ — у неё нет KV-cache и контекста, зато есть латенты и VAE.

// ImageCoexistencePolicy — как image-генерация делит GPU с текстовым инференсом.
type ImageCoexistencePolicy string

const (
	// ImageCoexistenceExclusive — во время генерации image-бэкенд владеет картой:
	// текстовые слоты на том же хосте не выдаются (ждём в очереди или 429).
	// Безопасно по умолчанию, но LLM простаивает во время генерации.
	ImageCoexistenceExclusive ImageCoexistencePolicy = "exclusive"

	// ImageCoexistenceOffload — image-бэкенд запущен с offload в RAM
	// (--offload-to-cpu / --backend te=cpu), поэтому допускается совместная
	// работа; медленнее, но LLM не блокируется.
	ImageCoexistenceOffload ImageCoexistencePolicy = "offload"

	// ImageCoexistenceDedicated — под image-бэкенд выделена отдельная GPU:
	// ограничения не применяются (сосуществование не требуется).
	ImageCoexistenceDedicated ImageCoexistencePolicy = "dedicated"
)

// IsValidImageCoexistencePolicy — известна ли политика (пустое = дефолт exclusive).
func IsValidImageCoexistencePolicy(p ImageCoexistencePolicy) bool {
	switch p {
	case "", ImageCoexistenceExclusive, ImageCoexistenceOffload, ImageCoexistenceDedicated:
		return true
	}
	return false
}

// EffectiveImageCoexistencePolicy — политика с дефолтом exclusive.
func (s ImageResourceSettings) EffectiveCoexistencePolicy() ImageCoexistencePolicy {
	if s.Coexistence == "" {
		return ImageCoexistenceExclusive
	}
	return s.Coexistence
}

// ImageResourceSettings — секция `balancing.image` в конфиге.
type ImageResourceSettings struct {
	// Coexistence — политика сосуществования (default exclusive).
	Coexistence ImageCoexistencePolicy `json:"coexistence,omitempty"`

	// VramHeadroomMB — сколько VRAM оставлять свободной сверх оценки image-модели
	// (например, чтобы текстовый бэкенд мог держать свои веса). 0 = не резервировать.
	VramHeadroomMB int `json:"vramHeadroomMb,omitempty"`

	// BlockOnUnknownVRAMEstimate — opt-in СТРОГОСТЬ: блокировать генерацию, если
	// оценку VRAM модели получить не удалось (нет vramEstimateMb в профиле, нет
	// метрик воркера, нет размеров файлов).
	//
	// Почему по умолчанию ВЫКЛЮЧЕНО (R-Image, 2026-10-02): на типовом стенде
	// оценка часто неизвестна (свежий bundle без профиля, воркер без метрик VRAM),
	// и строгий дефолт означал бы «генерация не работает вообще» при формально
	// исправной конфигурации. Поэтому неизвестная оценка ПРОПУСКАЕТ генерацию
	// (с WARN в логе), а гейт защищает там, где он реально что-то знает:
	// известная оценка + известная свободная VRAM.
	BlockOnUnknownVRAMEstimate bool `json:"blockOnUnknownVramEstimate,omitempty"`

	// QueueWaitTimeoutSec — сколько exclusive-режим ждёт освобождения GPU,
	// прежде чем ответить 429 с Retry-After. 0 = дефолт (30 с).
	QueueWaitTimeoutSec int `json:"queueWaitTimeoutSec,omitempty"`

	// ExclusiveLockTimeoutSec — предохранитель: сколько держать лок GPU без
	// прогресса, прежде чем освободить принудительно (защита от зависшей
	// генерации, которая иначе заблокирует текстовый трафик навсегда).
	// 0 = дефолт (600 с).
	ExclusiveLockTimeoutSec int `json:"exclusiveLockTimeoutSec,omitempty"`

	// GateDisabled — полностью отключить гейт VRAM и лок сосуществования
	// (оператор знает, что делает: например, image только на CPU).
	GateDisabled bool `json:"gateDisabled,omitempty"`

	// AllowToolLoad — разрешено ли ИНСТРУМЕНТУ (generate_image) поднимать
	// image-модель, которой нет в VRAM (R85, 2026-10-06).
	//
	// ПОЧЕМУ УКАЗАТЕЛЬ, А НЕ bool: у настройки ТРИ состояния, и они разные.
	// nil = «не задано» → действует флаг окружения LB_IMAGE_TOOL_ALLOW_LOAD
	// (дефолт on); true → разрешено; false → запрещено, даже если окружение
	// говорит обратное. Обычный bool не отличил бы «оператор выключил
	// автозагрузку из WebUI» от «поле не сохраняли», и выключение нельзя было бы
	// отличить от отсутствия настройки.
	//
	// Хранится в том же файле-переопределении, что и остальные поля
	// balancing.image (/app/data), поэтому переживает рестарт и правится из WebUI.
	AllowToolLoad *bool `json:"allowToolLoad,omitempty"`

	// ToolLoadTimeoutSec — сколько ждать загрузку image-модели, которую поднимает
	// ВЫЗОВ инструмента (R86-follow-up, 2026-10-06).
	//
	// ЗАЧЕМ В КОНФИГЕ, А НЕ ТОЛЬКО В ENV: время загрузки зависит от стенда
	// (модель 4.7 ГБ на медленном диске не укладывается в дефолтные 600 с), и
	// оператору нужен способ поднять лимит без правки compose и перезапуска
	// балансера — ровно как у галочки автозагрузки выше. Живой случай: на стенде
	// qwen-image-2.1 (4.7 ГБ) не поднялся за 600 с, и вызов вернул ошибку таймаута.
	//
	// ПОЧЕМУ УКАЗАТЕЛЬ: три состояния, как у AllowToolLoad. nil = «не задано» →
	// действует LB_IMAGE_TOOL_LOAD_TIMEOUT_SEC (дефолт 600); значение = оно и
	// действует. Обычный int не отличил бы «оператор поставил 0/ничего не сохранял».
	ToolLoadTimeoutSec *int `json:"toolLoadTimeoutSec,omitempty"`
}

// EffectiveToolLoadTimeout — действующий таймаут загрузки модели из вызова.
//
// fallback — значение из окружения (LB_IMAGE_TOOL_LOAD_TIMEOUT_SEC), уже
// приведённое к секундам; используется, когда настройка в конфиге не задана.
func EffectiveToolLoadTimeout(v *int, fallback int) int {
	if v == nil {
		return fallback
	}
	return *v
}

// Дефолты (используются, когда поле в конфиге не задано).
const (
	DefaultImageQueueWaitTimeoutSec     = 30
	DefaultImageExclusiveLockTimeoutSec = 600
)

// EffectiveAllowToolLoad — действует ли автозагрузка модели из инструмента,
// если настройка в конфиге не задана (nil).
//
// ПРАВИЛО (R85): незаданная настройка = РАЗРЕШЕНО. Так требование оператора
// «разрешить инструменту самому загружать модель» (см.
// plans/2026-10-06-image-tool-catalog-autoload.md) выполняется без правки
// конфига на уже развёрнутых стендах, а выключить можно либо галочкой в WebUI
// (false), либо флагом окружения. Единственный источник правды для вызова —
// imageToolSettings (окружение перекрывает этот дефолт).
func EffectiveAllowToolLoad(v *bool) bool {
	if v == nil {
		return true
	}
	return *v
}

// EffectiveQueueWaitTimeout — таймаут ожидания лока с дефолтом.
func (s ImageResourceSettings) EffectiveQueueWaitTimeout() int {
	if s.QueueWaitTimeoutSec > 0 {
		return s.QueueWaitTimeoutSec
	}
	return DefaultImageQueueWaitTimeoutSec
}

// EffectiveExclusiveLockTimeout — предохранитель лока с дефолтом.
func (s ImageResourceSettings) EffectiveExclusiveLockTimeout() int {
	if s.ExclusiveLockTimeoutSec > 0 {
		return s.ExclusiveLockTimeoutSec
	}
	return DefaultImageExclusiveLockTimeoutSec
}

// ImageVramEstimate — оценка потребности image-модели в VRAM.
//
// Источники (в порядке приоритета): явный профиль (ImageModelProfile.VramEstimateMB),
// затем измеренный воркером факт (ModelInfo.vram_estimate_mb из /api/image/models),
// затем грубая оценка по размерам файлов bundle'а.
type ImageVramEstimate struct {
	// RequiredMB — сколько VRAM нужно (0 = неизвестно).
	RequiredMB int `json:"requiredMb"`
	// Source — откуда взялась оценка: "profile" | "worker" | "files" | "unknown".
	Source string `json:"source"`
	// Detail — человекочитаемое пояснение (идёт в ошибку/лог).
	Detail string `json:"detail,omitempty"`
}

// IsKnown — есть ли осмысленная оценка.
func (e ImageVramEstimate) IsKnown() bool {
	return e.RequiredMB > 0
}

// ImageVRAMVerdict — решение гейта.
type ImageVRAMVerdict struct {
	// Allowed — можно ли запускать генерацию.
	Allowed bool `json:"allowed"`
	// ReasonCode — машинный код причины отказа:
	// "insufficient_vram" | "unknown_vram_estimate" | "gate_disabled" | "ok".
	ReasonCode string `json:"reasonCode"`
	// Message — текст для клиента (OpenAI-конверт).
	Message string `json:"message,omitempty"`
	// Hint — что сделать (OOM-лестница из исследования движка).
	Hint string `json:"hint,omitempty"`
	// FreeMB/RequiredMB/HeadroomMB — числа для логов и UI.
	FreeMB     int `json:"freeMb,omitempty"`
	RequiredMB int `json:"requiredMb,omitempty"`
	HeadroomMB int `json:"headroomMb,omitempty"`
}

// Коды причин гейта (машиночитаемые).
const (
	ImageGateOK               = "ok"
	ImageGateDisabled         = "gate_disabled"
	ImageGateInsufficientVRAM = "insufficient_vram"
	ImageGateUnknownEstimate  = "unknown_vram_estimate"
)

// ImageOOMHint — подсказка по лестнице снижения памяти (docs/research-sdcpp-
// lowvram-integration.md §6). Порядок важен: сначала дешёвые шаги.
const ImageOOMHint = "reduce model quantization (q8 -> q4_0/q3_k), enable diffusion-fa, " +
	"move text encoder to CPU (backend te=cpu), enable vae-tiling (vae-tile-size 512), " +
	"reduce resolution, or use --offload-to-cpu"

// EvaluateImageVRAM — гейт «помещается ли image-модель».
//
// Логика сознательно простая и предсказуемая (в отличие от memfit для LLM):
//
//	allowed = GateDisabled
//	       || estimate неизвестна (и не включён BlockOnUnknownVRAMEstimate)
//	       || freeMB неизвестна
//	       || (estimate + headroom <= freeMB)
//
// То есть гейт блокирует РОВНО один осмысленный случай: оценка известна,
// свободная VRAM известна, и модель в неё не влезает. Во всех остальных
// случаях генерация пропускается — иначе при неполных метриках фича просто
// не работает, а оператор не понимает, почему.
func EvaluateImageVRAM(est ImageVramEstimate, freeMB int, s ImageResourceSettings) ImageVRAMVerdict {
	if s.GateDisabled {
		return ImageVRAMVerdict{Allowed: true, ReasonCode: ImageGateDisabled}
	}

	headroom := s.VramHeadroomMB
	verdict := ImageVRAMVerdict{
		FreeMB:     freeMB,
		RequiredMB: est.RequiredMB,
		HeadroomMB: headroom,
	}

	if !est.IsKnown() {
		if s.BlockOnUnknownVRAMEstimate {
			verdict.Allowed = false
			verdict.ReasonCode = ImageGateUnknownEstimate
			verdict.Message = "cannot estimate VRAM required by this image model"
			verdict.Hint = "set vramEstimateMb in the image model profile, or disable " +
				"balancing.image.blockOnUnknownVramEstimate to skip the gate"
			return verdict
		}
		// Дефолт: неизвестная оценка НЕ блокирует (WARN пишет вызывающая сторона).
		verdict.Allowed = true
		verdict.ReasonCode = ImageGateOK
		verdict.Message = "VRAM estimate is unknown; gate skipped"
		return verdict
	}

	// Свободная VRAM неизвестна — не блокируем (метрик может не быть вовсе).
	if freeMB <= 0 {
		verdict.Allowed = true
		verdict.ReasonCode = ImageGateOK
		verdict.Message = "free VRAM is unknown; gate skipped"
		return verdict
	}

	if est.RequiredMB+headroom > freeMB {
		verdict.Allowed = false
		verdict.ReasonCode = ImageGateInsufficientVRAM
		verdict.Message = fmt.Sprintf(
			"image model needs %d MB VRAM (+%d MB headroom), only %d MB free",
			est.RequiredMB, headroom, freeMB)
		verdict.Hint = ImageOOMHint
		return verdict
	}

	verdict.Allowed = true
	verdict.ReasonCode = ImageGateOK
	return verdict
}
