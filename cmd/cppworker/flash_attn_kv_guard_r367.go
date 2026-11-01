// flash_attn_kv_guard_r367.go — R367 (2026-10-31): внятный отказ на
// противоречие «flash attention выключен + квантованный KV-cache».
//
// ЖИВОЙ СЛУЧАЙ (этот же раунд, живая проверка R366). Оператор выставил в
// профиле модели `flashAttn: false` и забыл про `kvCacheType: q4_0`. После
// R366 это значение наконец ПРИМЕНЯЕТСЯ (раньше молча игнорировалось), и
// загрузка упала:
//
//	POST /api/models/load → 500
//	"load failed: ... failed to create context"
//	llama_init_from_model: quantized V cache requires flash_attn to be enabled
//
// llama.cpp прав: квантованный V-cache работает только с включённым flash
// attention. Но оператор видел «failed to create context» без причины — и это
// ровно тот тип «молчаливого» отказа, который проект старается не допускать:
// причина известна ДО загрузки, значит её надо назвать.
//
// Поэтому здесь предзагрузочная проверка: если flash attention выключен ЯВНО
// (0; -1 = auto не считается — llama.cpp сам включает его для квантованного KV)
// и тип KV квантованный, отвечаем 400 с указанием обоих полей и способа
// починить. Пары «auto/-1 + q4_0» и «off + f16» остаются допустимыми.
package main

import (
	"fmt"
	"net/http"

	"ollama-loadbalancer/pkg/logger"
)

// kvCacheTypesRequiringFlashAttnR367 — типы KV, которые llama.cpp не умеет
// квантовать без flash attention. Пустая строка = «наследовать» (f16) — не
// требует; f16 — не требует.
var kvCacheTypesRequiringFlashAttnR367 = map[string]bool{
	"q8_0": true,
	"q4_0": true,
}

// flashAttnKVCacheConflictR367 — есть ли противоречие в паре значений.
// Возвращает описание конфликта или nil.
func flashAttnKVCacheConflictR367(flashAttnType int, kvCacheType string) error {
	// -1 = auto: llama.cpp включит flash attention сам, конфликта нет.
	// 1 = enabled: конфликта нет по определению.
	if flashAttnType != 0 {
		return nil
	}
	if !kvCacheTypesRequiringFlashAttnR367[kvCacheType] {
		return nil
	}
	return fmt.Errorf("flash attention выключен (flashAttn=false), но kvCacheType=%s: "+
		"llama.cpp требует включённый flash attention для квантованного KV-cache "+
		"(в его логе: \"quantized V cache requires flash_attn to be enabled\"). "+
		"Включите flashAttn в профиле модели или выберите kvCacheType=f16", kvCacheType)
}

// checkFlashAttnKVCacheR367 — HTTP-обёртка: при конфликте отвечает 400 с
// причиной и возвращает false (caller не начинает загрузку).
func checkFlashAttnKVCacheR367(w http.ResponseWriter, modelName string, flashAttnType int, kvCacheType string) bool {
	conflict := flashAttnKVCacheConflictR367(flashAttnType, kvCacheType)
	if conflict == nil {
		return true
	}
	if logger.Get() != nil {
		logger.Get().Warnw("load refused: flash attention off with quantized KV cache (R367)",
			"model", modelName,
			"flash_attn_type", flashAttnType,
			"kv_cache_type", kvCacheType)
	}
	writeError(w, http.StatusBadRequest, conflict.Error())
	return false
}
