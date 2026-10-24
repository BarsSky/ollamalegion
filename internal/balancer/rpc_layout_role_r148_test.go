package balancer

import (
	"strings"
	"testing"
	"time"

	"ollama-loadbalancer/pkg/types"
)

// R148 (2026-10-16): роль бэкенда ОПРЕДЕЛЯЕТ, хочет ли балансер узел раскладки.
//
// ЖИВОЙ СЛУЧАЙ, ради которого тест: донор (CYBORG) числился с ролью `donor`, и
// балансер каждые 15 секунд слал ему приказ поднять узел раскладки
// (`EnsureRPCNodeServing`) и строил раскладку слоёв, хотя донор должен был
// работать ОБЫЧНЫМ вторым бэкендом (не RPC). Переключение роли в `standalone`
// через API сняло и `wanted`, и саму раскладку — в логе «узел раскладки погашен
// (роль больше не донор)» и «раскладка выключена (самодостаточный режим)».
//
// Тест закрепляет инвариант: раскладку требует ТОЛЬКО роль donor; main и
// standalone (в т.ч. пустая роль) узла не требуют. Если кто-то изменит правило,
// стенд снова начнёт строить раскладку там, где оператор её не хотел.

func r148ProxyWithRole(t *testing.T, role string) *Proxy {
	t.Helper()
	config := createTestConfig()
	proxy := newProxyWithCleanup(t, config)
	if role != "" {
		if !proxy.SetBackendRole("backend-1", role) {
			t.Fatalf("SetBackendRole(%q) вернул false — роль не применена", role)
		}
	}
	return proxy
}

func TestR148RPCNodeWantedOnlyForDonorRole(t *testing.T) {
	// БЕЗ t.Parallel(): тест меняет РОЛИ бэкендов — это общее состояние прокси,
	// к тому же SetBackendRole планирует асинхронное сохранение состояния.
	// Параллельный прогон с чужими тестами один раз дал редкую флакость в
	// другом тесте пакета (TestGenerateImageForTool_LoadsNotLoadedModel), и
	// воспроизвести её намеренно не удалось — поэтому просто не мешаем другим.

	cases := []struct {
		role string
		want bool
		why  string
	}{
		{types.BackendRoleDonor, true, "донор — единственная роль, требующая узел раскладки"},
		{types.BackendRoleMain, false, "главный узел раскладки не требует"},
		{types.BackendRoleStandalone, false, "обычный режим (наш живой случай: донор переведён в standalone)"},
		{"", false, "роль не задана — раскладку не строим"},
	}

	for _, tc := range cases {
		tc := tc
		t.Run("role="+tc.role, func(t *testing.T) {
			// Роли пишутся в общий реестр бэкендов, поэтому тесты не параллелим
			// внутри (t.Parallel только на уровне всего теста).
			proxy := r148ProxyWithRole(t, tc.role)
			rep := proxy.RPCNodeReport("backend-1")
			if rep.Wanted != tc.want {
				t.Errorf("RPCNodeReport.Wanted = %v, want %v (%s); role=%q",
					rep.Wanted, tc.want, tc.why, rep.Role)
			}
			if rep.Role != tc.role {
				t.Errorf("RPCNodeReport.Role = %q, want %q", rep.Role, tc.role)
			}
		})
	}
}

// TestR148StandaloneDoesNotProbeDonors — в обычном режиме балансер не должен даже
// ПРОБОВАТЬ донора как узел раскладки, а главный — должен.
//
// Так тест ловит именно тот живой дефект: пока донор числился `donor`, балансер
// каждые 15 с слал ему приказ поднять узел раскладки и строил раскладку слоёв;
// после перевода в `standalone` в логе «раскладка выключена (самодостаточный
// режим)». Проба подменена, поэтому решение зависит ТОЛЬКО от роли, а не от сети,
// и «живой» узел не может замаскировать отсутствие раскладки.
func TestR148StandaloneDoesNotProbeDonors(t *testing.T) {
	// БЕЗ t.Parallel() — по той же причине: меняем роли бэкендов.

	proxy := r148ProxyWithRole(t, types.BackendRoleStandalone)
	if !proxy.SetBackendRole("backend-2", types.BackendRoleDonor) {
		t.Fatal("не удалось назначить роль donor второму бэкенду")
	}

	var probed []string
	proxy.rpcProbe = func(server string, timeout time.Duration) bool {
		probed = append(probed, server)
		return true // узел «живой»: если раскладку запросят, она не деградирует
	}

	degraded, reason := proxy.EnsureRPCLayout("backend-1")
	if len(probed) != 0 {
		t.Errorf("standalone: донора пробовать не должен, а спросил %v", probed)
	}
	// Причину «деградации» здесь сравнивать целиком нельзя: у тестового прокси
	// может не быть порта cppworker, и это отдельная причина. Важно, что причина
	// НЕ про узлы раскладки — то есть раскладка в самодостаточном режиме не
	// планировалась вовсе.
	_ = degraded
	if strings.Contains(reason, "узлы раскладки") || strings.Contains(reason, "узел раскладки") {
		t.Errorf("standalone: раскладку планировать не должен, а причина про узлы: %q", reason)
	}

	// Та же конфигурация, но backend-1 — главный: донор обязан быть опрошен.
	if !proxy.SetBackendRole("backend-1", types.BackendRoleMain) {
		t.Fatal("не удалось назначить роль main")
	}
	probed = nil
	proxy.EnsureRPCLayout("backend-1")
	if len(probed) == 0 {
		t.Error("main с донором обязан проверить узел раскладки, а не проигнорировал его")
	}
}
