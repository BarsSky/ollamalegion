// queue_seam_steward_p2_14_test.go — P2-14 (2026-11-11): шов очереди не имеет
// права вернуться в продакшен.
//
// ЗАЧЕМ. В proxy.go годами стоял no-op `SetQueueManagerProxy()` «для
// совместимости»: с R78 у QueueManager нет ссылки на proxy (канал и пул воркеров
// удалены, осталась статистика), и вызов ничего не делал. Его звали ~150 тестов
// как шов «подними прокси с очередью», поэтому реестр долгов считал удаление
// невыгодным. Настоящая проблема была не в тестах, а в том, что ПРОДАКШЕН
// объявлял фальшивый API: новый код мог вызвать его и решить, что что-то настроил.
//
// РЕШЕНИЕ: шов объявлен в _test.go (queue_seam_test.go), внешние пакеты свои
// no-op вызовы потеряли. Этот тест — замок на решении: если метод вернётся в
// продакшен-файл, тест упадёт.
package balancer

import (
	"os"
	"strings"
	"testing"
)

// TestP2_14_QueueSeamIsTestOnly — объявление шва живёт только в _test.go, и
// продакшен-код его не вызывает.
func TestP2_14_QueueSeamIsTestOnly(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("не прочитать каталог пакета: %v", err)
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("не прочитать %s: %v", name, err)
		}
		text := string(src)
		if strings.Contains(text, "func (p *Proxy) SetQueueManagerProxy()") {
			t.Errorf("%s: шов SetQueueManagerProxy объявлен в продакшен-файле — "+
				"это фальшивый API (no-op с R78); держите его в _test.go", name)
		}
		if strings.Contains(text, ".SetQueueManagerProxy()") {
			t.Errorf("%s: продакшен-код вызывает no-op SetQueueManagerProxy — "+
				"вызов ничего не настраивает, но выглядит как настройка", name)
		}
	}

	// И обратная сторона: шов обязан существовать для внутренних тестов, иначе
	// 150 тестов снова начнут его требовать в продакшене.
	src, err := os.ReadFile("queue_seam_test.go")
	if err != nil {
		t.Fatalf("не прочитать queue_seam_test.go: %v", err)
	}
	if !strings.Contains(string(src), "func (p *Proxy) SetQueueManagerProxy()") {
		t.Error("шов SetQueueManagerProxy пропал из queue_seam_test.go — " +
			"внутренние тесты используют его как «подними прокси с очередью»")
	}
}
