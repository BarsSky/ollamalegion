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

	// AllowUnknownVRAMEstimate — разрешать генерацию, если оценка VRAM неизвестна
	// (у модели не заполнен vramEstimateMb и нет метрик воркера). По умолчанию
	// false: лучше явная ошибка конфигурации, чем OOM внутри движка.
	AllowUnknownVRAMEstimate bool `json:"allowUnknownVramEstimate,omitempty"`

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
}

// Дефолты (используются, когда поле в конфиге не задано).
const (
	DefaultImageQueueWaitTimeoutSec     = 30
	DefaultImageExclusiveLockTimeoutSec = 600
)

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
//	       || (estimate известна && estimate + headroom <= free)
//
// freeMB <= 0 означает «свободная VRAM неизвестна» — тогда, если estimate
// известна, гейт пропускает (не блокируем работу из-за отсутствия метрик),
// а если неизвестна и AllowUnknownVRAMEstimate=false — отказывает.
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
		if s.AllowUnknownVRAMEstimate {
			verdict.Allowed = true
			verdict.ReasonCode = ImageGateOK
			verdict.Message = "VRAM estimate is unknown; allowed by allowUnknownVramEstimate"
			return verdict
		}
		verdict.Allowed = false
		verdict.ReasonCode = ImageGateUnknownEstimate
		verdict.Message = "cannot estimate VRAM required by this image model"
		verdict.Hint = "set vramEstimateMb in the image model profile, or enable " +
			"balancing.image.allowUnknownVramEstimate to skip the gate"
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
