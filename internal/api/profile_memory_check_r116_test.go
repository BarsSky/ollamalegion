// profile_memory_check_r116_test.go — R116: правило «отказ против предупреждения».
//
// Проверяется именно правило разбора вердикта memoria: что считать отказом
// (профиль не сохраняем), что предупреждением (сохраняем, но говорим про CPU), и
// что НЕ отказом (cppworker не отдал метаданные — иначе отказ получали бы все
// профили на ещё не прочитанной модели).
package api

import (
	"strings"
	"testing"
)

func TestR116_FitsEntirelyIsSilent(t *testing.T) {
	v := interpretProfileMemory(map[string]interface{}{
		"feasible": true,
		"degraded": false,
		"stage":    "exact_fit",
	})
	if !v.Feasible || v.Degraded {
		t.Errorf("R116: влезает целиком → без отказа и без предупреждения, получено %+v", v)
	}
	if v.Message != "" {
		t.Errorf("R116: при полном попадании сообщать нечего, получено %q", v.Message)
	}
}

func TestR116_FitsWithOffloadWarns(t *testing.T) {
	v := interpretProfileMemory(map[string]interface{}{
		"feasible": true,
		"degraded": true,
		"stage":    "partial_offload",
	})
	if !v.Feasible {
		t.Errorf("R116: выгрузка в RAM — не отказ, профиль сохраняем")
	}
	if !v.Degraded || !strings.Contains(v.Message, "RAM") {
		t.Errorf("R116: ожидалось предупреждение про выгрузку в RAM, получено %q", v.Message)
	}
	if !strings.Contains(v.Message, "partial_offload") {
		t.Errorf("R116: в сообщении должен быть stage, получено %q", v.Message)
	}
}

func TestR116_DoesNotFitRefusesWithBreakdown(t *testing.T) {
	breakdown := map[string]interface{}{"vram_free_mb": 0, "ram_avail_mb": 1234}
	v := interpretProfileMemory(map[string]interface{}{
		"feasible":         false,
		"stage":            "does_not_fit",
		"suggestion":       "уменьшите n_ctx или используйте kv q4_0",
		"memory_breakdown": breakdown,
	})
	if v.Feasible {
		t.Fatalf("R116: не влезает — обязан быть отказ")
	}
	if !strings.Contains(v.Message, "уменьшите n_ctx") {
		t.Errorf("R116: подсказка обязана попасть в сообщение, получено %q", v.Message)
	}
	if v.Memory["memory_breakdown"] == nil {
		t.Errorf("R116: раскладка памяти обязана ехать в ответе (иначе оператор не поймёт причину)")
	}
}

func TestR116_NoMetadataIsFailOpen(t *testing.T) {
	if v := interpretProfileMemory(nil); !v.Feasible {
		t.Errorf("R116: нет метаданных → не отказываем (fail-open), получено %+v", v)
	}
	v := interpretProfileMemory(map[string]interface{}{"error": "model_metadata_unavailable"})
	if !v.Feasible {
		t.Errorf("R116: ошибка проверки → сохраняем профиль как раньше, получено %+v", v)
	}
	if !strings.Contains(v.Message, "model_metadata_unavailable") {
		t.Errorf("R116: причина пропуска проверки должна быть названа, получено %q", v.Message)
	}
}

func TestR116_EffectiveURLCarriesProfileValues(t *testing.T) {
	got := profileEffectiveURL("http://cppworker:18092", "gemma-4-E4B-it-Q4_K_M", 8192, "q4_0")
	for _, want := range []string{
		"/api/models/effective?",
		"model=gemma-4-E4B-it-Q4_K_M",
		"ctx=8192",
		"kvCacheType=q4_0",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("R116: в URL проверки нет %q: %s", want, got)
		}
	}
	// Профиль без явного ctx не должен превращаться в «ctx=0»: отсутствие
	// значения означает «пусть решает cppworker», а не «нулевой контекст».
	if bare := profileEffectiveURL("http://cw", "m", 0, ""); strings.Contains(bare, "ctx=") {
		t.Errorf("R116: без ctx параметр не подставляется: %s", bare)
	}
}
