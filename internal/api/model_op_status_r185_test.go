// model_op_status_r185_test.go — R185 (2026-10-20).
//
// ЖИВОЙ РАЗБОР. Воркер собран под CUDA_ARCH=86;120, карта sm_75. Балансер
// отказывал в загрузке, но HTTP-слой отдавал 500 — а 5xx для Cline, deepseek
// harness, hermes и openai-* значит «повтори, станет лучше». Клиент повторял,
// балансер снова пытался грузить, воркер падал по SIGABRT: оператор видел
// «бесконечно крутит» без причины.
//
// Контракт: отказ по архитектуре — 400 (терминально). Всё остальное остаётся
// как было: успех 200, «занято» 409 (клиент отличит force), прочие ошибки 500.
package api

import (
	"net/http"
	"testing"

	"ollama-loadbalancer/internal/balancer"
)

func TestR185_ModelOpStatus_ArchRefusalIsTerminal(t *testing.T) {
	r := &balancer.ModelOpResult{
		Success:   false,
		ModelName: "m",
		Error: "бэкенд be1 не может считать на этой карте: sm=75 " +
			"(образ собран под sm_120;86) [cuda_arch_unsupported]",
	}
	if got := modelOpStatusCode(r); got != http.StatusBadRequest {
		t.Fatalf("статус %d, ожидался 400: 5xx заставит клиента повторять вечно", got)
	}
}

func TestR185_ModelOpStatus_PreservesExistingRules(t *testing.T) {
	cases := []struct {
		name string
		in   *balancer.ModelOpResult
		want int
	}{
		{"успех", &balancer.ModelOpResult{Success: true}, http.StatusOK},
		{"nil-безопасность", nil, http.StatusOK},
		{"занято", &balancer.ModelOpResult{Busy: true, Error: "model busy"}, http.StatusConflict},
		{"обычная ошибка", &balancer.ModelOpResult{Error: "failed to open GGUF file"}, http.StatusInternalServerError},
		// Текст «архитектура карты» БЕЗ машинного кода не должен менять статус:
		// иначе любая чужая ошибка с этим словом станет 400.
		{"похожий текст без кода", &balancer.ModelOpResult{Error: "архитектура не поддерживается"}, http.StatusInternalServerError},
		// R262 (2026-10-28): «загрузка ещё идёт» — не ошибка.
		//
		// Живой факт: загрузка Qwen3.8-27B вернула 500 «poll timeout» через
		// ~380 с, а воркер догрузил модель через десятки секунд — панель
		// показывала «ошибка загрузки» при работающей модели. 202 говорит
		// правду: работа принята и продолжается, статус спросим позже.
		{"ещё грузится", &balancer.ModelOpResult{StillLoading: true, PollAfterSec: 30}, http.StatusAccepted},
		// А вот настоящий провал по-прежнему 5xx (см. «обычная ошибка» выше):
		// StillLoading и Error одновременно не должны «спасать» провал.
		{"провал, не still-loading", &balancer.ModelOpResult{Error: "auto-load failed: no such file"}, http.StatusInternalServerError},
		// R304 (2026-10-30): загрузку запретила РОЛЬ (донор раскладки).
		//
		// Живой факт на стенде: API отвечал 500 «internal» на попытку загрузить
		// модель на донора, хотя тело несло причину «донор раскладки...». 500 для
		// клиента значит «повтори, станет лучше», а роль сама не изменится —
		// оператор видел поломку балансера вместо объяснения.
		{"роль запрещает копию", &balancer.ModelOpResult{RoleRefused: true, Error: "донор раскладки"}, http.StatusConflict},
	}
	for _, c := range cases {
		if got := modelOpStatusCode(c.in); got != c.want {
			t.Errorf("%s: статус %d, ожидался %d", c.name, got, c.want)
		}
	}
}
