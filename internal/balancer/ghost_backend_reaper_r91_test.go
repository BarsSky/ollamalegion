package balancer

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"ollama-loadbalancer/pkg/types"
)

// R91 (2026-10-09). Живая пара машин: после прогона нагрузочных тестов в
// /api/v1/backends?includeUnhealthy=true и в state.json навсегда осталась
// запись `share-probe` (image_cpp, метки sdworker/auto-registered/image,
// hasAgent=false, unhealthy, lastAgentContact=0001-01-01). Тестовый контейнер
// удалили, запись осталась — и вернулась после рестарта балансера.
//
// Тесты фиксируют правило: запись саморегистрации живёт, пока жив её агент;
// записи оператора (без метки auto-registered) и живые воркеры не трогаются.

const ghostTestTTL = 30 * time.Minute

func ghostBackend(id string, labels []string, status types.BackendStatus, hasAgent bool, lastContact time.Time) types.Backend {
	return types.Backend{
		ID:               id,
		Name:             id,
		Host:             id,
		Type:             types.BackendTypeImage,
		ImagePort:        18093,
		Labels:           labels,
		Status:           status,
		HasAgent:         hasAgent,
		LastAgentContact: lastContact,
	}
}

func autoRegisteredLabels() []string {
	return []string{"sdworker", "auto-registered", "image"}
}

func newReaperProxy(t *testing.T) *Proxy {
	t.Helper()
	return newProxyWithCleanup(t, &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{
			Host:      "localhost",
			Port:      8080,
			APIPort:   8081,
			StatePath: filepath.Join(t.TempDir(), "state.json"),
		},
	})
}

// TestReapGhostBackends_RemovesAbandonedAutoRegisteredRecord — главный кейс:
// запись ноды, чей агент молчит два часа, уходит из пула и из state.json.
func TestReapGhostBackends_RemovesAbandonedAutoRegisteredRecord(t *testing.T) {
	p := newReaperProxy(t)
	if err := p.AddBackend(ghostBackend("share-probe", autoRegisteredLabels(), types.StatusUnhealthy, false, time.Now().Add(-2*time.Hour))); err != nil {
		t.Fatalf("AddBackend: %v", err)
	}
	if err := p.SaveState(); err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	removed := p.reapGhostBackends(time.Now(), ghostTestTTL)
	if len(removed) != 1 || removed[0] != "share-probe" {
		t.Fatalf("removed = %v, ожидался [share-probe]", removed)
	}
	if p.BackendExists("share-probe") {
		t.Fatal("призрачная запись осталась в пуле бэкендов")
	}
	if persisted := readStateBackendIDs(t, p.statePath); len(persisted) != 0 {
		t.Fatalf("state.json всё ещё содержит %v — запись вернётся после рестарта", persisted)
	}
}

// TestReapGhostBackends_KeepsHealthyWorkerWithoutAgent — воркер отвечает на
// health-check, а агент мёртв: инференс по нему ещё работает, запись не трогаем.
func TestReapGhostBackends_KeepsHealthyWorkerWithoutAgent(t *testing.T) {
	p := newReaperProxy(t)
	if err := p.AddBackend(ghostBackend("sdworker-1", autoRegisteredLabels(), types.StatusHealthy, false, time.Now().Add(-2*time.Hour))); err != nil {
		t.Fatalf("AddBackend: %v", err)
	}
	if removed := p.reapGhostBackends(time.Now(), ghostTestTTL); len(removed) != 0 {
		t.Fatalf("removed = %v, healthy-бэкенд удалять нельзя", removed)
	}
	if !p.BackendExists("sdworker-1") {
		t.Fatal("healthy-бэкенд исчез из пула")
	}
}

// TestReapGhostBackends_KeepsOperatorCreatedRecord — запись, созданная руками
// (нет метки auto-registered), принадлежит оператору и не удаляется автоматически.
func TestReapGhostBackends_KeepsOperatorCreatedRecord(t *testing.T) {
	p := newReaperProxy(t)
	if err := p.AddBackend(ghostBackend("manual-image", []string{"image"}, types.StatusUnhealthy, false, time.Now().Add(-24*time.Hour))); err != nil {
		t.Fatalf("AddBackend: %v", err)
	}
	if removed := p.reapGhostBackends(time.Now(), ghostTestTTL); len(removed) != 0 {
		t.Fatalf("removed = %v, запись оператора удалять нельзя", removed)
	}
	if !p.BackendExists("manual-image") {
		t.Fatal("запись оператора исчезла из пула")
	}
}

// TestReapGhostBackends_KeepsRecordWithActiveRequests — по записи идёт запрос
// (или дренаж): удалять её нельзя, даже если она уже кандидат по молчанию.
func TestReapGhostBackends_KeepsRecordWithActiveRequests(t *testing.T) {
	p := newReaperProxy(t)
	if err := p.AddBackend(ghostBackend("busy-ghost", autoRegisteredLabels(), types.StatusUnhealthy, false, time.Now().Add(-2*time.Hour))); err != nil {
		t.Fatalf("AddBackend: %v", err)
	}
	p.backends["busy-ghost"].ActiveReqs = 1

	if removed := p.reapGhostBackends(time.Now(), ghostTestTTL); len(removed) != 0 {
		t.Fatalf("removed = %v, запись с активным запросом удалять нельзя", removed)
	}
	if !p.BackendExists("busy-ghost") {
		t.Fatal("запись с активным запросом исчезла из пула")
	}
}

// TestReapGhostBackends_RecentSilenceIsKept — агент замолчал минуту назад
// (перезапуск, сетевая пауза): запись живёт до истечения TTL.
func TestReapGhostBackends_RecentSilenceIsKept(t *testing.T) {
	p := newReaperProxy(t)
	if err := p.AddBackend(ghostBackend("restarting", autoRegisteredLabels(), types.StatusUnhealthy, false, time.Now().Add(-time.Minute))); err != nil {
		t.Fatalf("AddBackend: %v", err)
	}
	if removed := p.reapGhostBackends(time.Now(), ghostTestTTL); len(removed) != 0 {
		t.Fatalf("removed = %v, TTL ещё не истёк", removed)
	}
}

// TestReapGhostBackends_ZeroLastContactStartsClock — у записи нет ни одного
// контакта агента: срок отсчитывается от первого наблюдения и якорь сразу
// уезжает в саму запись, чтобы рестарт балансера не начинал отсчёт заново.
func TestReapGhostBackends_ZeroLastContactStartsClock(t *testing.T) {
	p := newReaperProxy(t)
	if err := p.AddBackend(ghostBackend("no-contact", autoRegisteredLabels(), types.StatusUnhealthy, false, time.Time{})); err != nil {
		t.Fatalf("AddBackend: %v", err)
	}

	now := time.Now()
	if removed := p.reapGhostBackends(now, ghostTestTTL); len(removed) != 0 {
		t.Fatalf("removed = %v: первый проход только начинает отсчёт", removed)
	}
	anchor := p.GetBackend("no-contact").AgentSilentSince
	if anchor == nil || anchor.IsZero() {
		t.Fatal("якорь молчания не записан в саму запись — после рестарта отсчёт начнётся заново")
	}
	if removed := p.reapGhostBackends(now.Add(ghostTestTTL-time.Minute), ghostTestTTL); len(removed) != 0 {
		t.Fatalf("removed = %v: TTL ещё не истёк", removed)
	}
	removed := p.reapGhostBackends(now.Add(ghostTestTTL+time.Minute), ghostTestTTL)
	if len(removed) != 1 || removed[0] != "no-contact" {
		t.Fatalf("removed = %v, ожидался [no-contact] после истечения TTL", removed)
	}
}

// TestLoadState_KeepsPersistedAnchorAcrossRestarts — якорь молчания живёт в
// state.json: рестарт балансера (а graceful shutdown переписывает файл текущим
// временем) не должен начинать отсчёт заново — именно так призрак `share-probe`
// переживал перезапуски.
func TestLoadState_KeepsPersistedAnchorAcrossRestarts(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	// Первый балансер: видит запись без контакта с агентом и ставит якорь.
	p1 := newProxyWithCleanup(t, &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{Host: "localhost", Port: 8080, APIPort: 8081, StatePath: statePath},
	})
	if err := p1.AddBackend(ghostBackend("share-probe", autoRegisteredLabels(), types.StatusUnhealthy, false, time.Time{})); err != nil {
		t.Fatalf("AddBackend: %v", err)
	}
	if removed := p1.reapGhostBackends(time.Now(), ghostTestTTL); len(removed) != 0 {
		t.Fatalf("первый проход не должен ничего удалять: %v", removed)
	}
	if err := p1.SaveState(); err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	// Второй балансер (рестарт) видит уже записанный якорь и убирает запись,
	// как только TTL истёк — без повторного ожидания.
	p2 := newProxyWithCleanup(t, &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{Host: "localhost", Port: 8080, APIPort: 8081, StatePath: statePath},
	})
	if !p2.BackendExists("share-probe") {
		t.Fatal("state.json не загрузился — тест бессмысленен")
	}
	removed := p2.reapGhostBackends(time.Now().Add(ghostTestTTL+time.Minute), ghostTestTTL)
	if len(removed) != 1 || removed[0] != "share-probe" {
		t.Fatalf("removed = %v, ожидался [share-probe]: якорь не пережил рестарт", removed)
	}
}

// TestGhostBackendTTL_Env — конфигурация уборки из окружения.
func TestGhostBackendTTL_Env(t *testing.T) {
	cases := []struct {
		env  string
		want time.Duration
	}{
		{"", defaultGhostBackendTTL},
		{"60", time.Minute},
		{"0", 0},
		{"-5", 0},
		{"abc", defaultGhostBackendTTL},
	}
	for _, c := range cases {
		t.Setenv("LB_GHOST_BACKEND_TTL_SEC", c.env)
		if got := GhostBackendTTL(); got != c.want {
			t.Errorf("LB_GHOST_BACKEND_TTL_SEC=%q → %v, ожидалось %v", c.env, got, c.want)
		}
	}
}

// TestReapGhostBackends_DisabledByZeroTTL — TTL=0 выключает уборку целиком.
func TestReapGhostBackends_DisabledByZeroTTL(t *testing.T) {
	p := newReaperProxy(t)
	if err := p.AddBackend(ghostBackend("share-probe", autoRegisteredLabels(), types.StatusUnhealthy, false, time.Now().Add(-24*time.Hour))); err != nil {
		t.Fatalf("AddBackend: %v", err)
	}
	if removed := p.reapGhostBackends(time.Now(), 0); len(removed) != 0 {
		t.Fatalf("removed = %v, при TTL=0 уборка выключена", removed)
	}
}

// readStateBackendIDs — ID записей из state.json на диске.
func readStateBackendIDs(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	var state types.StateFile
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatalf("parse state: %v", err)
	}
	ids := make([]string, 0, len(state.Backends))
	for _, b := range state.Backends {
		ids = append(ids, b.ID)
	}
	return ids
}
