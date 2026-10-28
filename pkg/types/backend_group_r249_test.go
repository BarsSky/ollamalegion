package types

import (
	"strings"
	"testing"
)

// R249 (2026-10-28): один бэкенд не может быть участником ДВУХ групп.
//
// Живая жалоба оператора: в блоке раскладки уже назначенные доноры/мастера
// оставались доступными в таблице, он добавлял того же донора второму мастеру и
// получал непонятную ошибку при применении. Проверка внутри одной группы этого
// не ловила — нужен cluster-wide учёт.
func TestR249_BackendCannotBelongToTwoGroups(t *testing.T) {
	first := BackendGroupConfig{
		ID:       "g1",
		Model:    "qwen3-14b*",
		Strategy: "layer",
		Members: []BackendGroupMemberConfig{
			{BackendID: "master-a", Role: "master"},
			{BackendID: "donor-shared", Role: "worker", RPCEndpoint: "192.0.2.11:50052"},
		},
	}
	second := BackendGroupConfig{
		ID:       "g2",
		Model:    "qwen3-32b*",
		Strategy: "layer",
		Members: []BackendGroupMemberConfig{
			{BackendID: "master-b", Role: "master"},
			{BackendID: "donor-shared", Role: "worker", RPCEndpoint: "192.0.2.11:50052"},
		},
	}

	errs := ValidateBackendGroups([]BackendGroupConfig{first, second})
	if len(errs) == 0 {
		t.Fatal("повторный донор в двух группах должен быть отклонён")
	}
	joined := strings.Join(errs, "; ")
	if !strings.Contains(joined, "donor-shared") {
		t.Fatalf("в ошибке нет id участника: %s", joined)
	}
	if !strings.Contains(joined, `"g1"`) {
		t.Fatalf("в ошибке нет группы, где участник уже занят: %s", joined)
	}
	if !strings.Contains(joined, "разъедините") {
		t.Fatalf("в ошибке нет подсказки, что делать: %s", joined)
	}
}

// R249: разные участники в двух группах — корректный конфиг.
func TestR249_TwoGroupsWithDistinctMembersStayValid(t *testing.T) {
	a := BackendGroupConfig{
		ID: "g1", Model: "m1", Strategy: "layer",
		Members: []BackendGroupMemberConfig{
			{BackendID: "master-a", Role: "master"},
			{BackendID: "donor-a", Role: "worker", RPCEndpoint: "192.0.2.11:50052"},
		},
	}
	b := BackendGroupConfig{
		ID: "g2", Model: "m2", Strategy: "layer",
		Members: []BackendGroupMemberConfig{
			{BackendID: "master-b", Role: "master"},
			{BackendID: "donor-b", Role: "worker", RPCEndpoint: "192.0.2.12:50052"},
		},
	}
	if errs := ValidateBackendGroups([]BackendGroupConfig{a, b}); len(errs) != 0 {
		t.Fatalf("разные участники — конфиг корректен, получено: %v", errs)
	}
}
