// default_profile_reasoning_test.go — R106b (2026-10-12).
//
// ЗАЧЕМ. Оператор жалуется: «модель gemma4 при загрузке применяет не те
// параметры, что указаны в настройках» и «не дало поменять размышления с
// выключенного на включенный». Одна из половин дефекта — отображение (его
// закрывает internal/api/load_defaults_reasoning_test.go), вторая — ЦЕПОЧКА
// применения: значение reasoning из «настроек по умолчанию для новых моделей»
// (config.defaultModelProfile) обязано доезжать до тела загрузки cppworker.
//
// Цепочка: executeLlamaCppLoad → applyProfileLoadParams(профиль модели) →
// applyProfileLoadParams(defaultModelProfile) → buildLlamaCppLoadBody →
// ключ `enableReasoning` (cppworker принимает его на /api/models/load и
// /api/models/load-with-params, см. cmd/cppworker/balancer_load_body_r83_test.go).
//
// Приоритет: явный запрос > профиль модели > настройки по умолчанию > env
// контейнера cppworker.
package balancer

import (
	"testing"

	"ollama-loadbalancer/pkg/types"
)

// TestR106b_DefaultProfile_ReasoningFillsGapAndReachesBody — модель без своего
// reasoning получает значение из «настроек по умолчанию», и оно попадает в тело
// загрузки (иначе cppworker возьмёт свой defaultEnableReasoning = false).
func TestR106b_DefaultProfile_ReasoningFillsGapAndReachesBody(t *testing.T) {
	var req ModelOpRequest

	// 1) профиль модели: reasoning НЕ задан (nil) — типичный случай, когда
	//    оператор включил размышления только в «настройках по умолчанию».
	applyProfileLoadParams(&req, types.LlamaCppModelProfile{
		ContextLength: 32768,
		Parallel:      2,
	})
	if req.EnableReasoning != nil {
		t.Fatalf("reasoning не должен появляться из профиля, где его нет: %v", *req.EnableReasoning)
	}

	// 2) настройки по умолчанию с включёнными размышлениями.
	on := true
	applyProfileLoadParams(&req, types.LlamaCppModelProfile{
		ContextLength:   65536,
		EnableReasoning: &on,
	})
	if req.EnableReasoning == nil || !*req.EnableReasoning {
		t.Fatalf("reasoning из defaultModelProfile не доехал до запроса загрузки: %v", req.EnableReasoning)
	}

	body := buildLlamaCppLoadBody(req, nil, nil, false)
	v, ok := body["enableReasoning"]
	if !ok {
		t.Fatalf("в теле загрузки нет ключа enableReasoning — cppworker поднимет модель "+
			"со своим defaultEnableReasoning (обычно false). body=%v", body)
	}
	if b, _ := v.(bool); !b {
		t.Fatalf("enableReasoning в теле = %v, want true", v)
	}
}

// TestR106b_ModelProfile_ReasoningWinsOverDefault — если у модели свой профиль с
// ЯВНЫМ reasoning, настройки по умолчанию его не перебивают. Без этого «выключить
// размышления у одной модели» было бы невозможно.
func TestR106b_ModelProfile_ReasoningWinsOverDefault(t *testing.T) {
	var req ModelOpRequest

	off := false
	applyProfileLoadParams(&req, types.LlamaCppModelProfile{
		ContextLength:   8192,
		EnableReasoning: &off,
	})
	on := true
	applyProfileLoadParams(&req, types.LlamaCppModelProfile{
		ContextLength:   65536,
		EnableReasoning: &on,
	})

	if req.EnableReasoning == nil || *req.EnableReasoning {
		t.Fatalf("профиль модели с reasoning=false обязан выигрывать у настроек по "+
			"умолчанию: %v", req.EnableReasoning)
	}
	// contextLength — тоже из профиля модели (он выигрывает).
	if req.ContextSize == nil || *req.ContextSize != 8192 {
		t.Fatalf("contextSize = %v, want 8192 (профиль модели)", req.ContextSize)
	}

	body := buildLlamaCppLoadBody(req, nil, nil, false)
	if b, _ := body["enableReasoning"].(bool); b {
		t.Fatalf("в тело ушло enableReasoning=true, хотя профиль модели говорит false: %v", body)
	}
}
