package balancer

// R101 (S1 плана 2026-10-07): проверка совместимости сторон RPC стала ЖИВОЙ.
//
// До R101 валидатор композиции всегда видел «версия неизвестна» (warning), и
// поймать реальное расхождение сборок было нечем. Теперь версия приходит от узла
// (env `CPPWORKER_LLAMA_BUILD` → регистрация → `types.Backend.BuildInfo`) и
// сравнивается. Проверяем все три исхода: совпали, разошлись, не сообщены.

import (
	"strings"
	"testing"

	"ollama-loadbalancer/internal/composition"
	"ollama-loadbalancer/pkg/types"
)

func setBuildInfoR101(p *Proxy, backendID, build string) {
	p.mu.Lock()
	if state := p.backends[backendID]; state != nil && state.Backend != nil {
		state.Backend.BuildInfo = build
	}
	p.mu.Unlock()
}

func issueOfR101(report composition.Report, check composition.CheckID) (composition.Issue, bool) {
	for _, issue := range report.Issues {
		if issue.Check == check {
			return issue, true
		}
	}
	return composition.Issue{}, false
}

func TestComposition_BuildInfoMatchingIsConfirmed_R101(t *testing.T) {
	p := compositionProxyR95(t, 6144, 16384)
	const build = "gguf-v0.19.0-1369-g1d2869c6e"
	setBuildInfoR101(p, r95MasterID, build)
	setBuildInfoR101(p, r95WorkerID, build)

	report := p.ValidateCompositionR95(compositionRequestR95(listenR95(t), 8<<30))

	if _, bad := issueOfR101(report, composition.CheckVersionMismatch); bad {
		t.Fatalf("одинаковые сборки не должны давать расхождение: %s", report.String())
	}
	if _, unknown := issueOfR101(report, composition.CheckVersionUnknown); unknown {
		t.Errorf("обе версии сообщены — предупреждения «не подтверждено» быть не должно: %s", report.String())
	}
}

func TestComposition_BuildInfoMismatchIsCaught_R101(t *testing.T) {
	p := compositionProxyR95(t, 6144, 16384)
	setBuildInfoR101(p, r95MasterID, "gguf-v0.19.0-1369-g1d2869c6e")
	setBuildInfoR101(p, r95WorkerID, "gguf-v0.19.0-1500-gdeadbeef")

	report := p.ValidateCompositionR95(compositionRequestR95(listenR95(t), 8<<30))

	issue, found := issueOfR101(report, composition.CheckVersionMismatch)
	if !found {
		t.Fatalf("разные сборки узлов раскладки обязаны ловиться: %s", report.String())
	}
	if issue.Level != composition.LevelError {
		t.Errorf("уровень = %q, ожидалось error (§0.6: протокол версионно-чувствителен)", issue.Level)
	}
	if !strings.Contains(issue.Text, "1d2869c6e") || !strings.Contains(issue.Text, "deadbeef") {
		t.Errorf("в тексте должны быть ОБЕ версии: %s", issue.Text)
	}
}

func TestComposition_BuildInfoUnknownStaysHonest_R101(t *testing.T) {
	// Узлы версию не сообщают (образ без CPPWORKER_LLAMA_BUILD) — это не ошибка,
	// но и не подтверждение: валидатор обязан сказать об этом прямо.
	p := compositionProxyR95(t, 6144, 16384)

	report := p.ValidateCompositionR95(compositionRequestR95(listenR95(t), 8<<30))

	if _, bad := issueOfR101(report, composition.CheckVersionMismatch); bad {
		t.Error("без версий расхождения быть не может")
	}
	if _, found := issueOfR101(report, composition.CheckVersionUnknown); !found {
		t.Fatalf("неизвестная версия должна помечаться предупреждением: %s", report.String())
	}
	if report.HasErrors() {
		t.Errorf("отсутствие версии не блокирует сборку: %s", report.String())
	}
}

func TestComposition_BuildInfoFromBackendState_R101(t *testing.T) {
	// Проверяем именно путь данных: состояние бэкенда → узел валидатора.
	p := compositionProxyR95(t, 6144, 16384)
	setBuildInfoR101(p, r95MasterID, "  build-with-spaces  ")
	setBuildInfoR101(p, r95WorkerID, "build-with-spaces")

	report := p.ValidateCompositionR95(compositionRequestR95(listenR95(t), 8<<30))
	if _, bad := issueOfR101(report, composition.CheckVersionMismatch); bad {
		t.Fatalf("пробелы не должны превращать одну версию в две: %s", report.String())
	}
}

func TestBackendBuildInfo_ExistsWithoutRegistrationPayload_R101(t *testing.T) {
	// Поле доступно любому потребителю состояния узла (в т.ч. /api/v1/backends).
	backend := types.Backend{ID: "x", BuildInfo: "build-x"}
	if backend.BuildInfo != "build-x" {
		t.Fatal("поле BuildInfo должно быть частью модели бэкенда")
	}
}
