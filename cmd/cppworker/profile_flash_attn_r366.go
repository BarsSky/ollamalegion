// profile_flash_attn_r366.go — R366 (2026-10-31): flash attention из профиля
// модели, одно правило для всех путей загрузки.
//
// ДЕФЕКТ (разобран в docs/plans/2026-10-31-load-profile-legacy-path.md §5).
// Поле `flashAttn` профиля обрабатывалось строкой внутри
// applyProfileToLoadRequest, и эта строка ошибалась дважды:
//
//  1. `false` превращалось в -1. Семантика поля на стороне моста другая
//     (c/bridge/bridge.go:176):
//     -1 = auto, 0 = disabled, 1 = enabled.
//     То есть «выключить flash attention» в профиле включало auto — llama.cpp
//     решал сам и на поддерживаемых моделях включал обратно. Оператор видел
//     галочку «выключено» и работающий flash attention.
//  2. Помощник заполняет только ПУСТЫЕ поля («0 = не задано»), а у этого поля
//     0 — содержательное значение. При дефолте контейнера
//     CPPWORKER_FLASH_ATTN_TYPE=-1 (см. deployments/docker-compose.stack.yml)
//     «клиент молчит» и «клиент выключил» выглядели одинаково ненулевыми, и
//     профильное «выключить» не применялось ВООБЩЕ.
//
// Поэтому здесь приоритет выражен явно и одинаково для всех путей:
//
//	явное значение запроса  >  профиль модели  >  дефолт (конфиг/текущая загрузка)
//
// Пути, которые обязаны её звать (иначе профиль снова «теряется» — проверяется
// стюард-тестом profile_flash_attn_steward_r366_test.go):
//   - handleLoadModel          (legacy /api/models/load, через resolveLoadParamsR364);
//   - handleLoadWithParams     (/api/models/load-with-params);
//   - handleReloadModel        (/api/models/reload);
//   - applySyncedProfileToLazyLoadOpts (ленивая загрузка по запросу чата).
package main

// profileFlashAttnToLoad — итоговое значение flashAttnType для загрузки.
//
// request — то, что клиент прислал явно (nil = не задавал); profile — значение
// профиля (nil = профиль молчит); def — дефолт пути (конфиг/env, а при reload —
// значение текущей загрузки).
func profileFlashAttnToLoad(request *int, profile *bool, def int) int {
	if request != nil {
		return *request
	}
	if profile != nil {
		if *profile {
			return 1
		}
		return 0
	}
	return def
}
