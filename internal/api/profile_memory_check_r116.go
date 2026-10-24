// profile_memory_check_r116.go — R116: профиль нельзя сохранить «в никуда».
//
// ЗАЧЕМ. Профиль модели — источник истины: именно его `contextLength` и
// `kvCacheType` уйдут в загрузку. Если эти значения не влезают в суммарную
// память (VRAM карты + память узлов раскладки + RAM), оператор узнаёт об этом
// только при загрузке — и видит отказ или падение уже потом, а причина остаётся
// в профиле.
//
// ПРАВИЛО. Перед сохранением/применением профиля спрашиваем у cppworker вердикт
// по ИМЕННО ЭТИМ значениям — тем же расчётом (memfit), что и загрузка, через
// эндпоинт `GET /api/models/effective?model=…&ctx=…&kvCacheType=…` (R116).
// Второй формулы памяти не заводим: отчёт и решение обязаны совпадать.
//
// ИСХОДЫ:
//   - не влезает совсем (feasible=false) → профиль НЕ сохраняется, клиент
//     получает 4xx с раскладкой памяти (memory_breakdown) и подсказкой;
//   - влезает с выгрузкой в RAM (degraded=true) → сохраняется, но с
//     предупреждением: часть слоёв пойдёт на CPU, будет медленнее;
//   - влезает целиком → как раньше.
//
// Разбор вердикта вынесен в чистую функцию: правило («что считается отказом, а
// что предупреждением») обязано проверяться тестом, а не выводиться из чтения
// HTTP-обвязки.
package api

import (
	"fmt"
	"net/url"
)

// cppworkerEffectivePath — эндпоинт эффективных параметров и вердикта памяти.
const cppworkerEffectivePath = "/api/models/effective"

// profileMemoryVerdict — что делать с профилем по итогам проверки памяти.
type profileMemoryVerdict struct {
	// Feasible=false — сохранять нельзя: не влезает ни в VRAM, ни в RAM.
	Feasible bool
	// Degraded=true — влезает с выгрузкой части слоёв в RAM (медленнее).
	Degraded bool
	// Message — человеческое объяснение для UI (уходит в ответ).
	Message string
	// Memory — сырой блок `memory` из ответа (stage, потолки, breakdown).
	Memory map[string]interface{}
}

// interpretProfileMemory — чистое правило разбора вердикта.
//
// memory — блок `memory` из ответа cppworker (может быть nil, если метаданных
// модели нет: тогда мы НЕ отказываем — иначе отказ получали бы все профили на
// модели, которую cppworker ещё не успел прочитать).
func interpretProfileMemory(memory map[string]interface{}) profileMemoryVerdict {
	v := profileMemoryVerdict{Feasible: true, Memory: memory}
	if memory == nil {
		v.Message = "проверка памяти недоступна: cppworker не отдал метаданные модели"
		return v
	}
	if errStr, ok := memory["error"].(string); ok && errStr != "" {
		// ctx не определён / нет метаданных — это не «не влезает».
		v.Message = "проверка памяти пропущена: " + errStr
		return v
	}
	if feasible, ok := memory["feasible"].(bool); ok && !feasible {
		v.Feasible = false
		v.Message = "профиль не помещается в доступную память"
		if s, ok := memory["suggestion"].(string); ok && s != "" {
			v.Message += ": " + s
		}
		return v
	}
	if degraded, ok := memory["degraded"].(bool); ok && degraded {
		v.Degraded = true
		v.Message = "профиль помещается только с выгрузкой части слоёв в RAM — " +
			"модель будет работать заметно медленнее"
		if stage, ok := memory["stage"].(string); ok && stage != "" {
			v.Message += " (stage=" + stage + ")"
		}
	}
	return v
}

// profileEffectiveURL — адрес проверки для конкретного профиля.
//
// ctx и kvCacheType подставляются из профиля: проверяем ИМЕННО то, что будет
// применено, а не «типичную» конфигурацию.
func profileEffectiveURL(baseURL, modelName string, ctx int, kvCacheType string) string {
	q := url.Values{}
	q.Set("model", modelName)
	if ctx > 0 {
		q.Set("ctx", fmt.Sprintf("%d", ctx))
	}
	if kvCacheType != "" {
		q.Set("kvCacheType", kvCacheType)
	}
	return baseURL + cppworkerEffectivePath + "?" + q.Encode()
}

// checkProfileMemory — вердикт памяти для профиля (nil-безопасно).
//
// Возвращает (verdict, ok): ok=false означает «спросить не удалось» — в этом
// случае вызывающий обязан СОХРАНИТЬ профиль как раньше (fail-open), потому что
// недоступность cppworker — не повод запрещать настройку. Отказ выдаётся только
// на осознанный вердикт «не влезает».
func (s *Server) checkProfileMemory(modelName string, ctx int, kvCacheType string) (profileMemoryVerdict, bool) {
	if s == nil || s.proxy == nil {
		return profileMemoryVerdict{Feasible: true}, false
	}
	_, baseURL := s.firstCppWorkerBackend()
	if baseURL == "" {
		return profileMemoryVerdict{Feasible: true}, false
	}
	var payload struct {
		Memory map[string]interface{} `json:"memory"`
	}
	if err := s.cppworkerGetJSON(profileEffectiveURL(baseURL, modelName, ctx, kvCacheType), &payload); err != nil {
		return profileMemoryVerdict{Feasible: true}, false
	}
	return interpretProfileMemory(payload.Memory), true
}
