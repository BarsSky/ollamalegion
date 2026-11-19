package balancer

// force_rebalance_log_r493_test.go — сторож долга 12m: шумный warn не должен
// вернуться.
//
// ЖИВОЙ ФАКТ, ради которого страж. На втором стенде (2026-11-16, соук R493)
// лог за 46 с дал 10 warn «force rebalance failed, queueing request» при
// load_ratio=1 — это штатный уход в единую admission-очередь, а не ошибка.
// Всего 20 warn из 507 строк: настоящие предупреждения в шуме не видны.
//
// ПОЧЕМУ СТРАЖ, А НЕ ТЕСТ ПОВЕДЕНИЯ. Уровень лога — не наблюдаемое поведение
// клиента: ответ 200 в обоих случаях. Проверить «warn пишется только когда
// очередь не примет» поведением значило бы поднимать весь стек логирования ради
// строки; в репозитории для такого класса есть сторожа по исходнику (серия
// R493), страж ниже — из той же серии.

import (
	"os"
	"strings"
	"testing"
)

// TestR493_ForceRebalanceWarnOnlyWhenQueueCannotTake — оба места, где решается
// «альтернативы нет», зовут общий helper, а не пишут Warnw напрямую.
func TestR493_ForceRebalanceWarnOnlyWhenQueueCannotTake(t *testing.T) {
	for _, file := range []string{"proxy.go", "session_handler.go"} {
		src, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("не прочитать %s: %v", file, err)
		}
		text := string(src)
		if strings.Contains(text, `Warnw("force rebalance failed, queueing request"`) {
			t.Errorf("%s: вернулся прямой Warnw «force rebalance failed» — при единой "+
				"очереди это штатный путь (долг 12m), уровень обязан решать "+
				"logForceRebalanceNoAltR493", file)
		}
		if !strings.Contains(text, "logForceRebalanceNoAltR493(") {
			t.Errorf("%s: нет вызова logForceRebalanceNoAltR493 — решение об уровне "+
				"потеряно", file)
		}
	}

	// Helper обязан различать два случая: единая очередь (info) и legacy (warn).
	helper, err := os.ReadFile("force_rebalance_log.go")
	if err != nil {
		t.Fatalf("не прочитать helper: %v", err)
	}
	h := string(helper)
	if !strings.Contains(h, "admissionUnifiedR493()") {
		t.Error("helper не проверяет admissionUnifiedR493 — уровень не зависит от того, " +
			"примет ли запрос единая очередь")
	}
	if !strings.Contains(h, "Warnw(") || !strings.Contains(h, "Infow(") {
		t.Error("helper обязан иметь оба уровня: Warnw для legacy-пути и Infow для единой очереди")
	}
}
