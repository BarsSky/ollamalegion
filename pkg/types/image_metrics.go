package types

import "time"

// R-Image (2026-10-02): снимок состояния image-бэкенда (тип image_cpp) для
// WebUI/Monitor и управляющего API.
//
// ЗАЧЕМ ОТДЕЛЬНЫЙ БЛОК: у текстовых бэкендов модели и состояние лежат в
// Ollama/LlamaCpp-метриках, а image-воркер отдаёт свой контракт
// (/api/image/models) — без отдельного поля Monitor и страница Models видели
// image-бэкенд «пустым»: ни порта, ни моделей, ни состояния (жалоба оператора).
//
// Данные заполняет поллер image_resources (internal/balancer), который и так
// опрашивает воркер для гейта VRAM, поэтому дополнительной нагрузки нет.

// ImageModelBrief — краткая запись модели image-воркера для UI.
type ImageModelBrief struct {
	Name           string `json:"name"`
	State          string `json:"state"` // not_loaded | loading | loaded | error
	Family         string `json:"family,omitempty"`
	SizeBytes      int64  `json:"sizeBytes,omitempty"`
	VramEstimateMB int    `json:"vramEstimateMb,omitempty"`
	ActiveQueries  int64  `json:"activeQueries,omitempty"`
	Error          string `json:"error,omitempty"`
}

// ImageBackendMetrics — состояние image-бэкенда, как его видит балансер.
type ImageBackendMetrics struct {
	// State — состояние воркера целиком (loaded/loading/not_loaded/error).
	State string `json:"state,omitempty"`
	// CurrentModel — имя текущей модели воркера (может быть пустым).
	CurrentModel string `json:"currentModel,omitempty"`
	// Models — известные воркеру bundle'ы (включая незагруженные).
	Models []ImageModelBrief `json:"models,omitempty"`
	// VramFreeMB/VramTotalMB — VRAM хоста, если воркер её сообщил
	// (0 = неизвестно; это НЕ «ноль свободно»).
	VramFreeMB  int `json:"vramFreeMb,omitempty"`
	VramTotalMB int `json:"vramTotalMb,omitempty"`
	// PinnedRevision — версия движка, под которую написан контракт.
	PinnedRevision string `json:"pinnedRevision,omitempty"`
	// UpdatedAt — когда снимок получен (UI показывает свежесть данных).
	UpdatedAt time.Time `json:"updatedAt,omitempty"`
	// LastError — ошибка последнего опроса воркера (снимок недостоверен,
	// но UI должен видеть, что именно не так, а не пустоту).
	LastError string `json:"lastError,omitempty"`
}
