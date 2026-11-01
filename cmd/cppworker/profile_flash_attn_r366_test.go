// profile_flash_attn_r366_test.go — R366 (2026-10-31).
//
// Контракт: flash attention при загрузке решается ОДНИМ правилом на всех путях
//
//	явное значение запроса  >  профиль модели  >  дефолт (конфиг / текущая загрузка)
//
// и профильное «выключить» — это 0 (disabled), а не -1 (auto): семантика поля на
// стороне моста именно такая (c/bridge/bridge.go:176). Прежняя реализация
// отдавала false → -1 и вдобавок не применяла профиль при дефолте контейнера -1
// (потому что «0 = не задано» для этого поля неверно).
package main

import "testing"

func flIntR366(v int) *int    { return &v }
func flBoolR366(v bool) *bool { return &v }

// TestR366_Precedence — три слоя приоритета.
func TestR366_Precedence(t *testing.T) {
	defs := -1 // CPPWORKER_FLASH_ATTN_TYPE:--1 в docker-compose.stack.yml

	t.Run("явный запрос главнее профиля и дефолта", func(t *testing.T) {
		for _, explicit := range []int{-1, 0, 1} {
			if got := profileFlashAttnToLoad(flIntR366(explicit), flBoolR366(false), defs); got != explicit {
				t.Errorf("explicit=%d: got %d, want %d (запрос обязан побеждать)", explicit, got, explicit)
			}
		}
	})

	t.Run("профиль false — это 0 (выключено), а не -1 (auto)", func(t *testing.T) {
		if got := profileFlashAttnToLoad(nil, flBoolR366(false), defs); got != 0 {
			t.Errorf("got %d, want 0 (disabled): при -1 llama.cpp включает flash attention по своему усмотрению", got)
		}
	})

	t.Run("профиль true — это 1 (включено)", func(t *testing.T) {
		if got := profileFlashAttnToLoad(nil, flBoolR366(true), defs); got != 1 {
			t.Errorf("got %d, want 1 (enabled)", got)
		}
	})

	t.Run("профиль молчит — действует дефолт пути", func(t *testing.T) {
		if got := profileFlashAttnToLoad(nil, nil, defs); got != defs {
			t.Errorf("got %d, want %d (дефолт конфига)", got, defs)
		}
		if got := profileFlashAttnToLoad(nil, nil, 1); got != 1 {
			t.Errorf("got %d, want 1 (дефолт текущей загрузки при reload)", got)
		}
	})

	t.Run("профиль перебивает дефолт -1 (главный живой дефект)", func(t *testing.T) {
		// До R366 здесь было -1: помощник заполнял только пустое, а -1 ≠ 0, поэтому
		// профильное «выключить» не применялось вовсе.
		if got := profileFlashAttnToLoad(nil, flBoolR366(false), -1); got != 0 {
			t.Errorf("got %d, want 0: профиль обязан перебивать env-дефолт -1", got)
		}
	})
}

// TestR366_LegacyLoadPathUsesRule — путь R364 (legacy /api/models/load) обязан
// считать flash attention тем же правилом, иначе дефект вернётся на одном пути.
func TestR366_LegacyLoadPathUsesRule(t *testing.T) {
	req := loadModelRequest{}
	defs := loadDefaultsR364{FlashAttnType: -1}

	// Профиль «выключить» → 0, несмотря на дефолт -1.
	got := resolveLoadParamsR364(req, loadProfileSeedR364{FlashAttn: flBoolR366(false)}, defs)
	if got.FlashAttnType != 0 {
		t.Errorf("legacy-путь: flashAttnType=%d, want 0 (профиль false → disabled)", got.FlashAttnType)
	}

	// Явный запрос главнее профиля.
	reqExplicit := loadModelRequest{FlashAttnType: flIntR366(1)}
	got = resolveLoadParamsR364(reqExplicit, loadProfileSeedR364{FlashAttn: flBoolR366(false)}, defs)
	if got.FlashAttnType != 1 {
		t.Errorf("legacy-путь: flashAttnType=%d, want 1 (явный запрос главнее профиля)", got.FlashAttnType)
	}

	// Профиль молчит → дефолт.
	got = resolveLoadParamsR364(loadModelRequest{}, loadProfileSeedR364{}, defs)
	if got.FlashAttnType != -1 {
		t.Errorf("legacy-путь: flashAttnType=%d, want -1 (дефолт конфига)", got.FlashAttnType)
	}
}

// TestR366STEWARD_EveryProfilePathAppliesFlashAttn — стюард: каждый путь
// загрузки, который применяет профиль модели, обязан звать profileFlashAttnToLoad.
//
// ЗАЧЕМ. Дефект R366 существовал именно потому, что правило жило строкой в
// общем помощнике: у него был один вызов на три пути, и все три ошибались
// одинаково. Теперь правило вынесено отдельно, и новый путь загрузки обязан
// позвать его сам — иначе flash attention из профиля снова «теряется».
func TestR366STEWARD_EveryProfilePathAppliesFlashAttn(t *testing.T) {
	files := parsePackageFilesR364(t)

	// Пути, которые применяют профиль к параметрам загрузки.
	wantPaths := []string{
		"handleLoadModel",                  // legacy /api/models/load (через resolveLoadParamsR364)
		"handleLoadWithParams",             // /api/models/load-with-params
		"handleReloadModel",                // /api/models/reload
		"applySyncedProfileToLazyLoadOpts", // ленивая загрузка по запросу чата
	}

	seen := map[string]bool{}
	for _, pf := range files {
		for _, fn := range funcsR364(pf.file) {
			calls := callNamesR364(fn)
			if calls["profileFlashAttnToLoad"] {
				seen[fn.Name.Name] = true
			}
			// applySyncedProfileToLazyLoadOpts зовёт правило напрямую; legacy-путь
			// зовёт resolveLoadParamsR364, который внутри уже использует правило.
			if fn.Name.Name == "handleLoadModel" && calls["resolveLoadParamsR364"] {
				seen[fn.Name.Name] = true
			}
		}
	}

	for _, want := range wantPaths {
		if !seen[want] {
			t.Errorf("%s: путь загрузки применяет профиль, но flash attention из профиля не считает "+
				"(нет profileFlashAttnToLoad).\n"+
				"    Профильное «выключить flash attention» при этом снова станет no-op — ровно дефект R366.", want)
		}
	}

	// Само правило обязано существовать: если его переименовали, список путей
	// устарел бы молча.
	ruleFound := false
	for _, pf := range files {
		for _, fn := range funcsR364(pf.file) {
			if fn.Name.Name == "profileFlashAttnToLoad" {
				ruleFound = true
			}
		}
	}
	if !ruleFound {
		t.Fatal("функция profileFlashAttnToLoad не найдена — обнови стюард")
	}
}
