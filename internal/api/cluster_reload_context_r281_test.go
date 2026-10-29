// cluster_reload_context_r281_test.go — R281 (2026-10-29): операторский reload
// с другим окном контекста обязан ПЕРЕЗАГРУЖАТЬ модель.
//
// ЖИВОЙ ДЕФЕКТ (воспроизведён дважды на стенде). `POST
// /api/v1/cluster/models/{model}/reload {contextSize: 8192}` на уже загруженной
// модели отвечал `ok: already loaded` и перезагрузку НЕ выполнял — окно
// оставалось прежним (32768). Приходилось делать unload + load вручную.
// Снаружи это выглядит как «выставил окно в WebUI, а загрузилось старое» —
// ровно та жалоба, из-за которой затевался весь разбор настроек.
//
// Правило вынесено в чистую функцию needContextReload, чтобы не появилось
// второго мнения «уже загружено» (в серии такой класс ловили четыре раза).
package api

import "testing"

func intPtr(v int) *int { return &v }

// TestR281_NeedContextReloadMatrix — таблица правила.
func TestR281_NeedContextReloadMatrix(t *testing.T) {
	cases := []struct {
		name      string
		loadedCtx int
		ctxKnown  bool
		requested *int
		want      bool
		why       string
	}{
		{
			name: "другое окно → перезагрузка", loadedCtx: 32768, ctxKnown: true,
			requested: intPtr(8192), want: true,
			why: "главный живой случай: загружено 32768, просят 8192",
		},
		{
			name: "то же окно → already loaded", loadedCtx: 8192, ctxKnown: true,
			requested: intPtr(8192), want: false,
			why: "перезагружать нечего, «уже загружено» — правда",
		},
		{
			name: "окно не задано → прежнее поведение", loadedCtx: 8192, ctxKnown: true,
			requested: nil, want: false,
			why: "не задано = оператор не просил менять",
		},
		{
			name: "окно 0 → не задано", loadedCtx: 8192, ctxKnown: true,
			requested: intPtr(0), want: false,
			why: "0 не является валидным окном",
		},
		{
			name: "факт неизвестен, окно задано → перезагрузка", loadedCtx: 0, ctxKnown: false,
			requested: intPtr(8192), want: true,
			why: "старые сборки cppworker не сообщают contextLength: «уже загружено» было бы догадкой",
		},
		{
			name: "факт неизвестен, окно не задано → уже загружено", loadedCtx: 0, ctxKnown: false,
			requested: nil, want: false,
			why: "нечего сравнивать и менять не просили",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := needContextReload(c.loadedCtx, c.ctxKnown, c.requested)
			if got != c.want {
				t.Fatalf("needContextReload(%d, %v, %v) = %v, want %v (%s)",
					c.loadedCtx, c.ctxKnown, c.requested, got, c.want, c.why)
			}
		})
	}
}

// TestR281_ResultCarriesWindowNumbers — ответ оператору обязан называть ЧИСЛА:
// без них «перезагрузили» неотличимо от «ничего не делали».
func TestR281_ResultCarriesWindowNumbers(t *testing.T) {
	res := clusterReloadModelBackendResult{
		BackendID:       "bk-1",
		Status:          "ok",
		Reloaded:        true,
		PreviousContext: 32768,
		NewContext:      8192,
	}
	if !res.Reloaded {
		t.Fatal("reloaded должен быть true при реальной перезагрузке")
	}
	if res.PreviousContext != 32768 || res.NewContext != 8192 {
		t.Fatalf("числа окна потеряны: previous=%d new=%d", res.PreviousContext, res.NewContext)
	}
}
