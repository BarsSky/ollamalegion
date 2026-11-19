package balancer

import (
	"testing"
	"time"

	"ollama-loadbalancer/pkg/types"
)

// R553 (F8, вторая половина, docs/plans/2026-11-19-webui-and-balancer-fixes.md §2.8).
//
// ИЗМЕРЕНО НА ПРЕДЫДУЩЕМ КОДЕ (R552): end-to-end тест холодной загрузки при
// включённом `SyncModelLoad.Enabled` занимал **120 секунд**. Причина — порядок
// этапов: P3 инициировал прогрев и ждал его синхронно `ModelLoadTimeout` (120 с по
// умолчанию), и только потом управление доходило до этапа 3b, который всё равно
// выбирал свободный узел. То есть клиент платил две минуты за путь, на котором
// модель заведомо не успевает подняться (живые cold-load — 140–485 с).
//
// После правки этап 3b стоит ВЫШЕ P3: узел выбирается сразу, а загрузку выполняет
// гейт R325 (с бюджетом авто-загрузки, а не с 120-секундным синхронным ожиданием).

// TestR553_ColdLoadSkipsSyncWait — при включённом SyncModelLoad выбор узла не
// ждёт синхронного прогрева: ответ за единицы миллисекунд, а не за таймаут P3.
func TestR553_ColdLoadSkipsSyncWait(t *testing.T) {
	p := availProxyR493(t, map[string]int{"only": 2})
	setMetricsR493(p, "only", nil, 20490)
	catalogStoreR552(p, "only", []string{"m27b.gguf"}, "")
	p.lbAutoLoadAsync = true

	// P3 включён и его таймаут намеренно БОЛЬШОЙ: если 3b снова окажется ниже P3,
	// тест это увидит по времени (ждать 3 с здесь нельзя — тест обязан быть
	// быстрым, поэтому таймаут P3 задан 3 с, а граница проверки — 1 с).
	p.config.Balancing.SyncModelLoad.Enabled = true
	p.config.Balancing.ModelLoadTimeout = 3 // секунды

	start := time.Now()
	got := p.selectBackend("m27b", types.BackendTypeLlamaCpp)
	elapsed := time.Since(start)

	if got != "only" {
		t.Fatalf("холодная загрузка: селектор вернул %q, ожидался узел с файлом на диске", got)
	}
	if elapsed > time.Second {
		t.Fatalf("выбор узла занял %v: этап 3b снова стоит ниже P3 и ждёт синхронный прогрев "+
			"(на проде это 120 с — измерено в R552)", elapsed)
	}
}

// TestR553_SyncLoadStillReachable — перестановка не «выключила» P3: когда этап 3b
// не находит узла (модели нет ни в каталоге, ни в VRAM), синхронная загрузка
// остаётся доступной. Это защита от того, чтобы правка тихо отменила настройку
// оператора «грузить синхронно».
func TestR553_SyncLoadStillReachable(t *testing.T) {
	p := availProxyR493(t, map[string]int{"only": 2})
	setMetricsR493(p, "only", nil, 20490)
	// Каталог пуст: 3b не может выбрать узел.
	p.lbAutoLoadAsync = false
	p.config.Balancing.SyncModelLoad.Enabled = true
	p.config.Balancing.ModelLoadTimeout = 1 // чтобы тест не висел долго

	if p.modelHasOnDiskSomewhereR493("m27b", p.getAllowedTypesList(types.BackendTypeLlamaCpp)) {
		t.Fatal("условие теста не выполнено: каталог не должен знать о файле")
	}
	if got := p.pickLoadableFreeBackendR493("m27b", p.getAllowedTypesList(types.BackendTypeLlamaCpp)); got != "" {
		t.Fatalf("условие теста не выполнено: 3b не должен выбирать узел, got %q", got)
	}

	start := time.Now()
	got := p.selectBackend("m27b", types.BackendTypeLlamaCpp)
	elapsed := time.Since(start)

	// P3 обязан был СРАБОТАТЬ (он единственный, кто может выбрать узел) — значит
	// время выбора сопоставимо с его таймаутом, а не нулевое.
	if got != "only" {
		t.Fatalf("без 3b узел должен выбираться этапом P3 (синхронная загрузка), got %q", got)
	}
	if elapsed < 500*time.Millisecond {
		t.Fatalf("выбор занял %v: похоже, P3 больше не выполняется вовсе — "+
			"настройка «грузить синхронно» перестала работать", elapsed)
	}
}
