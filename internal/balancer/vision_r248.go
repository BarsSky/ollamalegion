// vision_r248.go — R248, часть B1: признак vision у модели на стороне балансера.
//
// ЗАЧЕМ. R247 отбивал изображения 415 всегда. B1 добавляет достоверный признак:
// cppworker отдаёт `capabilities.vision` в `/v1/models`, балансер агрегирует его
// по бэкендам (кэш 30 с) и решает — пропустить изображения (passthrough) или
// честно ответить 415. Пока mmproj не загружается (B2/B3), у всех моделей
// vision=false, и поведение 415 не меняется.
package balancer

import (
	"strings"
	"sync"
	"time"
)

// visionCapabilityCacheTTL — как часто перечитывать /v1/models бэкендов.
const visionCapabilityCacheTTL = 30 * time.Second

type visionCapabilityCache struct {
	mu      sync.RWMutex
	byModel map[string]bool
	at      time.Time
}

var visionCaps visionCapabilityCache

// modelVisionCapable — умеет ли модель изображения (по данным бэкендов).
func (lr *LlamaCppRouter) modelVisionCapable(model string) bool {
	name := strings.ToLower(strings.TrimSpace(model))
	if name == "" {
		return false
	}
	visionCaps.mu.RLock()
	fresh := time.Since(visionCaps.at) < visionCapabilityCacheTTL
	cached, ok := visionCaps.byModel[name]
	visionCaps.mu.RUnlock()
	if fresh && ok {
		return cached
	}
	// Перечитываем у бэкендов; при ошибке остаёмся на кэше (лучше «не знаем =
	// false», чем пропустить картинку модели, которая её не увидит).
	refreshVisionCaps(lr)
	visionCaps.mu.RLock()
	defer visionCaps.mu.RUnlock()
	return visionCaps.byModel[name]
}

// refreshVisionCaps — опрос /v1/models всех llama.cpp-бэкендов.
func refreshVisionCaps(lr *LlamaCppRouter) {
	if lr == nil || lr.proxy == nil {
		return
	}
	found := map[string]bool{}
	for _, b := range lr.getLlamaCppBackends() {
		tags, err := lr.fetchLlamaCppModels(b.host, b.port)
		if err != nil {
			continue
		}
		for _, tag := range tags {
			if tag.Vision {
				found[strings.ToLower(strings.TrimSpace(tag.Name))] = true
			}
		}
	}
	visionCaps.mu.Lock()
	visionCaps.byModel = found
	visionCaps.at = time.Now()
	visionCaps.mu.Unlock()
}

// resetVisionCapabilityCache — сброс кэша (тесты, смена набора бэкендов).
func resetVisionCapabilityCache() {
	visionCaps.mu.Lock()
	visionCaps.byModel = map[string]bool{}
	visionCaps.at = time.Time{}
	visionCaps.mu.Unlock()
}

// visionShouldReject — решение по изображениям: отдать 415 или пропустить.
// Чистая функция — вся логика решения в одном месте и покрыта тестом.
func visionShouldReject(capable bool, images int) bool {
	return images > 0 && !capable
}
