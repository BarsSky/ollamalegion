package types

import "time"

// Prediction - прогноз критического состояния бэкенда
type Prediction struct {
	SecondsToCritical float64 `json:"secondsToCritical"` // Секунд до критического состояния (-1 = нет данных, +Inf = не определено)
	CriticalReason    string  `json:"criticalReason"`    // Причина: "gpu_usage", "vram", "ram", "disk", "concurrent_requests", "models_capacity", "none"
	GPUUsageTrend     float64 `json:"gpuUsageTrend"`     // Тренд загрузки GPU (% в минуту, >0 — рост)
	VRAMUsageTrend    float64 `json:"vramUsageTrend"`    // Тренд использования VRAM (% в минуту)
	RAMUsageTrend     float64 `json:"ramUsageTrend"`     // Тренд использования RAM (% в минуту)
	FreeSlotsTrend    float64 `json:"freeSlotsTrend"`    // Тренд свободных слотов (слотов в минуту, <0 — уменьшение)
	RequestCapacity   float64 `json:"requestCapacity"`   // Текущая ёмкость запросов (0-100%, 100% = полная загрузка)
}

// MetricsSnapshot - точка истории метрик для прогнозирования
type MetricsSnapshot struct {
	Timestamp         time.Time `json:"timestamp"`
	GPUUsagePercent   float64   `json:"gpuUsagePercent"`
	VRAMUsagePercent  float64   `json:"vramUsagePercent"`
	RAMUsagePercent   float64   `json:"ramUsagePercent"`
	ActiveRequests    int       `json:"activeRequests"`
	RunningModels     int       `json:"runningModels"`
	FreeSlots         int       `json:"freeSlots"`
	RequestsPerSecond float64   `json:"requestsPerSecond"`
}