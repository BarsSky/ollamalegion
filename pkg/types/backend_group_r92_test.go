package types

// R92 (S2 плана 2026-10-07): валидация логических бэкендов
// (balancing.backendGroups[]).
//
// Это заготовка валидатора S4: конфиг раскладки обязан быть отвергнут ДО
// сохранения, если собрать группу физически невозможно (нет мастера, воркер без
// RPC-эндпоинта, один узел вместо двух).

import (
	"strings"
	"testing"
)

func validGroupR92() BackendGroupConfig {
	return BackendGroupConfig{
		ID:       "g1",
		Model:    "qwen3-14b*",
		Strategy: "layer",
		Members: []BackendGroupMemberConfig{
			{BackendID: "master", Role: "master"},
			{BackendID: "worker", Role: "worker", RPCEndpoint: "192.0.2.11:50052"},
		},
	}
}

func TestValidateBackendGroups_Valid_R92(t *testing.T) {
	if errs := ValidateBackendGroups([]BackendGroupConfig{validGroupR92()}); len(errs) != 0 {
		t.Errorf("корректная группа дала ошибки: %v", errs)
	}
	if errs := ValidateBackendGroups(nil); len(errs) != 0 {
		t.Errorf("пустой список групп ошибок не даёт: %v", errs)
	}
}

func TestValidateBackendGroups_Errors_R92(t *testing.T) {
	cases := []struct {
		name  string
		mut   func(*BackendGroupConfig)
		wants []string
	}{
		{
			name:  "пустой id",
			mut:   func(g *BackendGroupConfig) { g.ID = " " },
			wants: []string{"пустое поле id"},
		},
		{
			name:  "пустая модель",
			mut:   func(g *BackendGroupConfig) { g.Model = "" },
			wants: []string{"пустое поле model"},
		},
		{
			name:  "неизвестная стратегия",
			mut:   func(g *BackendGroupConfig) { g.Strategy = "tensor" },
			wants: []string{"неизвестная strategy"},
		},
		{
			name: "нет мастера",
			mut: func(g *BackendGroupConfig) {
				g.Members[0].Role = "worker"
				g.Members[0].RPCEndpoint = "h:1"
			},
			wants: []string{"ровно один master", "найдено 0"},
		},
		{
			name: "два мастера",
			mut: func(g *BackendGroupConfig) {
				g.Members[1].Role = "master"
			},
			wants: []string{"ровно один master", "найдено 2"},
		},
		{
			name: "воркер без rpcEndpoint",
			mut: func(g *BackendGroupConfig) {
				g.Members[1].RPCEndpoint = ""
			},
			wants: []string{"rpcEndpoint"},
		},
		{
			name: "дубликат backendId",
			mut: func(g *BackendGroupConfig) {
				g.Members[1].BackendID = g.Members[0].BackendID
			},
			wants: []string{"дважды"},
		},
		{
			name: "неизвестная роль",
			mut: func(g *BackendGroupConfig) {
				g.Members[1].Role = "helper"
			},
			wants: []string{"неизвестная роль"},
		},
		{
			name: "один узел вместо двух",
			mut: func(g *BackendGroupConfig) {
				g.Members = g.Members[:1]
			},
			wants: []string{"минимум 2 узла"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := validGroupR92()
			tc.mut(&g)
			errs := ValidateBackendGroups([]BackendGroupConfig{g})
			if len(errs) == 0 {
				t.Fatal("ожидалась ошибка валидации")
			}
			joined := strings.ToLower(strings.Join(errs, " | "))
			for _, want := range tc.wants {
				if !strings.Contains(joined, strings.ToLower(want)) {
					t.Errorf("в ошибках нет %q: %v", want, errs)
				}
			}
		})
	}
}

func TestValidateBackendGroups_DuplicateGroupID_R92(t *testing.T) {
	a, b := validGroupR92(), validGroupR92()
	b.Model = "другая"
	errs := ValidateBackendGroups([]BackendGroupConfig{a, b})
	if len(errs) == 0 || !strings.Contains(strings.Join(errs, " "), "повторяется") {
		t.Errorf("дубликат id группы должен ловиться: %v", errs)
	}
}

func TestValidateBackendGroups_RowStrategyAllowed_R92(t *testing.T) {
	g := validGroupR92()
	g.Strategy = "row"
	if errs := ValidateBackendGroups([]BackendGroupConfig{g}); len(errs) != 0 {
		t.Errorf("row — допустимая стратегия раскладки: %v", errs)
	}
}

func TestIsValidBackendGroupStrategy_R92(t *testing.T) {
	for _, ok := range []string{"layer", "row", " LAYER ", "Row"} {
		if !IsValidBackendGroupStrategy(ok) {
			t.Errorf("%q должна быть допустимой", ok)
		}
	}
	for _, bad := range []string{"", "tensor", "pipeline"} {
		if IsValidBackendGroupStrategy(bad) {
			t.Errorf("%q не должна быть допустимой", bad)
		}
	}
}

func TestBackendGroupConfig_Helpers_R92(t *testing.T) {
	g := validGroupR92()
	if m, ok := g.Master(); !ok || m.BackendID != "master" {
		t.Errorf("Master() = %+v ok=%v", m, ok)
	}
	if w := g.Workers(); len(w) != 1 || w[0].BackendID != "worker" {
		t.Errorf("Workers() = %+v", w)
	}
	if !g.IsGroupWorker("worker") || g.IsGroupWorker("master") {
		t.Error("IsGroupWorker должен различать роли")
	}

	broken := validGroupR92()
	broken.Members[0].Role = "worker"
	broken.Members[0].RPCEndpoint = "h:1"
	if _, ok := broken.Master(); ok {
		t.Error("без мастера Master() обязан вернуть ok=false")
	}
}

func TestMatchModelMask_MovedToTypes_R92(t *testing.T) {
	// R92: канонический матчер переехал из internal/balancer в pkg/types — им
	// пользуются и placement-политика, и логический бэкенд. Поведение прежнее.
	cases := []struct {
		pattern, model string
		want           bool
	}{
		{"qwen3*", "qwen3-14b", true},
		{"Qwen3*", "QWEN3-14b", true},
		{"qwen3*", "qwen3-14b.gguf", true},
		{"qwen3-14b", "qwen3-14b.gguf", true},
		{"qwen?", "qwen3", true},
		{"qwen?", "qwen33", false},
		{"hf.co/user/*", "hf.co/user/model", true},
		{"*", "anything", true},
		{"", "anything", false},
		{"qwen3*", "", false},
	}
	for _, tc := range cases {
		if got := MatchModelMask(tc.pattern, tc.model); got != tc.want {
			t.Errorf("MatchModelMask(%q, %q) = %v, ожидалось %v", tc.pattern, tc.model, got, tc.want)
		}
	}
}
