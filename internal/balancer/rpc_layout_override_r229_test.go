//go:build llama_stub

// rpc_layout_override_r229_test.go — R229 (2026-10-22): тумблер раскладки
// принадлежит оператору.
//
// ЖИВОЙ ДЕФЕКТ. Оператор выключил раскладку у главного (rpcEnabled=false), воркер
// это применил, а через ~15 секунд гард EnsureRPCLayout вернул rpcEnabled=true со
// списком доноров — потому что роль главного + живой донор значат «раскладку
// включить». Тумблер «сам включался обратно», и следующая загрузка снова уходила
// на донора.
//
// Тесты фиксируют:
//   - «выкл» переживает тик гарда и повторные вызовы (конфиг воркера не трогается);
//   - «авто» возвращает прежнее поведение по ролям;
//   - «вкл» побеждает роль «самодостаточный»;
//   - «вкл», но узлы мертвы — раскладка всё равно не отдаётся загрузке (мёртвый
//     адрес в llama.cpp = SIGABRT), зато выбор оператора сохранён.
package balancer

import (
	"strings"
	"testing"

	"ollama-loadbalancer/pkg/types"
)

// boolPtr берём из profiles_persistence_test.go (одна функция на пакет).

// TestR229_RPCOffOverrideSurvivesRoleTick — главное: «выкл» не перезаписывается
// гардом ни на следующем тике, ни после повторных EnsureRPCLayout.
func TestR229_RPCOffOverrideSurvivesRoleTick(t *testing.T) {
	p, rec := newLayoutGuardTestProxy(t, types.BackendRoleMain, types.BackendRoleDonor)
	setReachability(p, testDonorAddr) // донор ЖИВ — роль потребовала бы rpcEnabled=true

	if !p.SetRPCEnabledOverride("main-1", boolPtr(false)) {
		t.Fatal("SetRPCEnabledOverride(false) не сохранил выбор")
	}
	if mode := p.RPCEnabledOverrideMode("main-1"); mode != "off" {
		t.Fatalf("режим = %q, want off", mode)
	}

	degraded, reason := p.EnsureRPCLayout("main-1")
	if degraded {
		t.Fatalf("выключение оператором помечено деградацией: %s", reason)
	}
	if rec.enabled() {
		t.Fatal("раскладка включена после явного «выкл» — это и есть живой дефект")
	}
	if got := rec.servers(); len(got) != 0 {
		t.Errorf("список узлов = %v, want пусто", got)
	}

	// Тик ролей: гард вызывается снова и снова.
	for i := 0; i < 3; i++ {
		p.EnsureRPCLayout("main-1")
	}
	if rec.enabled() {
		t.Fatal("повторный тик гарда вернул раскладку — операторский выбор перезаписан")
	}
	if n := rec.count(); n != 1 {
		t.Errorf("конфиг воркера записан %d раз, want 1 (значение не меняется)", n)
	}
}

// TestR229_RPCAutoRestoresRoleLayout — «авто» снимает выбор и возвращает раскладку
// по ролям (без рестарта балансера и без ручной правки конфига воркера).
func TestR229_RPCAutoRestoresRoleLayout(t *testing.T) {
	p, rec := newLayoutGuardTestProxy(t, types.BackendRoleMain, types.BackendRoleDonor)
	setReachability(p, testDonorAddr)

	if !p.SetRPCEnabledOverride("main-1", boolPtr(false)) {
		t.Fatal("SetRPCEnabledOverride(false) не сохранил выбор")
	}
	p.EnsureRPCLayout("main-1")
	if rec.enabled() {
		t.Fatal("раскладка не выключилась")
	}

	if !p.SetRPCEnabledOverride("main-1", nil) {
		t.Fatal("сброс в «авто» не сохранился")
	}
	if mode := p.RPCEnabledOverrideMode("main-1"); mode != "auto" {
		t.Fatalf("режим = %q, want auto", mode)
	}
	degraded, reason := p.EnsureRPCLayout("main-1")
	if degraded {
		t.Fatalf("авто-режим с живым узлом помечен деградацией: %s", reason)
	}
	if !rec.enabled() {
		t.Fatal("«авто» не вернуло раскладку по ролям")
	}
	if got := rec.servers(); len(got) != 1 || got[0] != testDonorAddr {
		t.Fatalf("список узлов = %v, want [%s]", got, testDonorAddr)
	}
}

// TestR229_RPCOverrideOnBeatsStandaloneRole — «вкл» — это явный приказ оператора,
// он побеждает роль «самодостаточный» (иначе тумблер снова был бы мёртвым).
func TestR229_RPCOverrideOnBeatsStandaloneRole(t *testing.T) {
	p, rec := newLayoutGuardTestProxy(t, types.BackendRoleStandalone, types.BackendRoleDonor)
	setReachability(p, testDonorAddr)

	if !p.SetRPCEnabledOverride("main-1", boolPtr(true)) {
		t.Fatal("SetRPCEnabledOverride(true) не сохранил выбор")
	}
	degraded, reason := p.EnsureRPCLayout("main-1")
	if degraded {
		t.Fatalf("деградация при живом узле: %s", reason)
	}
	if !rec.enabled() {
		t.Fatal("роль standalone победила явное «вкл» оператора")
	}
	if got := rec.servers(); len(got) != 1 || got[0] != testDonorAddr {
		t.Fatalf("список узлов = %v, want [%s]", got, testDonorAddr)
	}
}

// TestR229_RPCOverrideOn_DeadNodesStillLoadsLocally — оператор включил, но узлы
// мертвы: выбор сохранён, однако в загрузку мёртвый адрес НЕ отдаётся (иначе
// llama.cpp падает SIGABRT), поэтому фактически раскладка выключается с причиной.
func TestR229_RPCOverrideOn_DeadNodesStillLoadsLocally(t *testing.T) {
	p, rec := newLayoutGuardTestProxy(t, types.BackendRoleMain, types.BackendRoleDonor)
	setReachability(p) // живых нет

	if !p.SetRPCEnabledOverride("main-1", boolPtr(true)) {
		t.Fatal("SetRPCEnabledOverride(true) не сохранил выбор")
	}
	degraded, reason := p.EnsureRPCLayout("main-1")
	if !degraded {
		t.Fatal("нет пометки деградации, хотя узлы недоступны")
	}
	if !strings.Contains(reason, testDonorAddr) {
		t.Errorf("причина не называет недоступный узел: %q", reason)
	}
	if rec.enabled() {
		t.Fatal("мёртвый адрес отдан загрузке — это роняет llama.cpp")
	}
	if mode := p.RPCEnabledOverrideMode("main-1"); mode != "on" {
		t.Errorf("выбор оператора потерян (mode=%q), хотя узлы временно мертвы", mode)
	}
}

// TestR229_RPCOverrideUnknownBackend — неизвестный бэкенд не создаёт состояние.
func TestR229_RPCOverrideUnknownBackend(t *testing.T) {
	p, _ := newLayoutGuardTestProxy(t, types.BackendRoleMain, types.BackendRoleDonor)
	if p.SetRPCEnabledOverride("nope", boolPtr(false)) {
		t.Fatal("выбор сохранён для несуществующего бэкенда")
	}
	if mode := p.RPCEnabledOverrideMode("nope"); mode != "auto" {
		t.Errorf("режим = %q, want auto", mode)
	}
}
