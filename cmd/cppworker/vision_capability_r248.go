// vision_capability_r248.go — R248, часть B1 плана
// (docs/plans/2026-10-27-mmproj-vision-and-tools-cancel-r248.md).
//
// ЗАЧЕМ. До B1 изображения отбивались 415 ВСЕГДА: даже модель, которую можно
// загрузить с mmproj, не имела способа сказать «я вижу картинки». Теперь
// возможность модели — это явный факт (`capabilities.vision` в `/v1/models`),
// по которому балансер решает: passthrough изображений или честный 415.
//
// ГРАНИЦА B1: загрузка mmproj и image-эмбеддинги — это B2/B3. Здесь только
// достоверный признак и маршрутизация: пока mmproj не грузится, ВСЕ модели
// остаются vision=false, и поведение 415 не меняется (регрессии нет).
package main

import (
	"strings"
	"sync"
)

var (
	visionCapsMu sync.RWMutex
	// visionCaps — переопределения оператора/загрузчика: модель → умеет ли
	// картинки. Заполняется при загрузке с mmproj (B2) или из профиля модели.
	visionCaps = map[string]bool{}
)

// setModelVisionCapable — отметить модель как vision-capable (или снять признак).
// Вызывается загрузкой модели с mmproj; до B2 никем не вызывается.
func setModelVisionCapable(model string, capable bool) {
	name := strings.ToLower(strings.TrimSpace(model))
	if name == "" {
		return
	}
	visionCapsMu.Lock()
	visionCaps[name] = capable
	visionCapsMu.Unlock()
}

// modelVisionCapable — умеет ли модель изображения.
//
// Пока mmproj не подключён (B2), у всех моделей false — честный отказ 415.
// Признак намеренно НЕ выводится из имени модели: «gemma-4» бывает и текстовой,
// и vision-сборкой, а угадывание дало бы 200 с моделью, которая картинку не
// видела (ровно тот дефект, который R247 закрыл).
func modelVisionCapable(model string) bool {
	name := strings.ToLower(strings.TrimSpace(model))
	if name == "" {
		return false
	}
	visionCapsMu.RLock()
	defer visionCapsMu.RUnlock()
	return visionCaps[name]
}

// resetVisionCaps — сброс (тесты/выгрузка модели).
func resetVisionCaps() {
	visionCapsMu.Lock()
	visionCaps = map[string]bool{}
	visionCapsMu.Unlock()
}
