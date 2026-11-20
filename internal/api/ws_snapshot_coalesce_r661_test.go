//go:build llama_stub

// ws_snapshot_coalesce_r661_test.go — R661 (2026-11-20): снапшоты clusterState
// склеиваются, а не копятся; застрявшая запись не держит соединение 30 с.
//
// ЖИВОЙ ДЕФЕКТ (лог балансера): `failed to send websocket snapshot` + «нет связи»
// в WebUI. Канал подписчика — буфер 100, публикация неблокирующая; медленный
// клиент накапливал до 99 устаревших снапшотов, обработчик вычитывал их по одному,
// на каждый ставил 30 с write deadline, и одна застрявшая запись рвала соединение
// ради данных, которые уже не были правдой.
//
// Проверяется: (1) дренаж отдаёт САМЫЙ СВЕЖИЙ снапшот и считает отброшенные;
// (2) закрытая подписка распознаётся (иначе цикл крутился бы по closed-каналу
// вхолостую); (3) обработчик /ws/metrics действительно склеивает и использует
// короткий deadline — иначе модуль есть, а подключён он не там (этот класс дефекта
// в проекте уже ловили стражем подключения).
package api

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// TestR661_DrainKeepsLatestSnapshot — из накопленных снапшотов остаётся последний.
func TestR661_DrainKeepsLatestSnapshot(t *testing.T) {
	ch := make(chan []byte, 100)
	ch <- []byte("second")
	ch <- []byte("third")
	ch <- []byte("fourth")

	latest, skipped, closed := drainLatestClusterSnapshot(ch, []byte("first"))
	if closed {
		t.Fatal("канал открыт — closed обязан быть false")
	}
	if string(latest) != "fourth" {
		t.Fatalf("latest = %q, ожидался самый свежий снапшот %q", string(latest), "fourth")
	}
	if skipped != 3 {
		t.Fatalf("skipped = %d, ожидалось 3 (два из канала + перезапись first)", skipped)
	}
	if len(ch) != 0 {
		t.Fatalf("в канале осталось %d снапшотов — дренаж обязан опустошать очередь", len(ch))
	}
}

// TestR661_DrainEmptyKeepsFirst — нечего склеивать: отдаём прочитанный снапшот.
func TestR661_DrainEmptyKeepsFirst(t *testing.T) {
	ch := make(chan []byte, 4)
	latest, skipped, closed := drainLatestClusterSnapshot(ch, []byte("only"))
	if closed || skipped != 0 || string(latest) != "only" {
		t.Fatalf("latest=%q skipped=%d closed=%v, ожидалось %q/0/false", string(latest), skipped, closed, "only")
	}
}

// TestR661_DrainClosedChannel — снятая подписка: закрытый канал виден вызывающему.
func TestR661_DrainClosedChannel(t *testing.T) {
	ch := make(chan []byte, 4)
	ch <- []byte("last")
	close(ch)

	latest, skipped, closed := drainLatestClusterSnapshot(ch, []byte("first"))
	if !closed {
		t.Fatal("закрытый канал обязан вернуть closed=true (иначе обработчик не выйдет)")
	}
	if string(latest) != "last" || skipped != 1 {
		t.Fatalf("latest=%q skipped=%d, ожидалось %q/1", string(latest), skipped, "last")
	}
}

// TestR661_WsMetricsHandlerCoalescesSnapshots — страж подключения.
func TestR661_WsMetricsHandlerCoalescesSnapshots(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "handlers_core.go", nil, 0)
	if err != nil {
		t.Fatalf("разбор handlers_core.go: %v", err)
	}
	src, err := os.ReadFile("handlers_core.go")
	if err != nil {
		t.Fatalf("чтение handlers_core.go: %v", err)
	}

	var found bool
	var body string
	var calls []string
	ast.Inspect(file, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Name == nil || fn.Name.Name != "wsMetricsHandler" || fn.Recv == nil {
			return true
		}
		found = true
		start := fset.Position(fn.Pos()).Offset
		end := fset.Position(fn.End()).Offset
		if start >= 0 && end <= len(src) && end > start {
			body = string(src[start:end])
		}
		ast.Inspect(fn.Body, func(inner ast.Node) bool {
			call, ok := inner.(*ast.CallExpr)
			if !ok {
				return true
			}
			if ident, ok := call.Fun.(*ast.Ident); ok {
				calls = append(calls, ident.Name)
			}
			return true
		})
		return false
	})
	if !found {
		t.Fatal("wsMetricsHandler не найдена — страж не может проверить путь, обновите тест")
	}
	joined := strings.Join(calls, ",")
	if !strings.Contains(joined, "drainLatestClusterSnapshot") {
		t.Fatal("обработчик /ws/metrics не склеивает снапшоты: медленный клиент снова получит " +
			"пачку устаревших состояний и 30-секундную блокировку (живой дефект R661)")
	}
	if !strings.Contains(body, "wsSnapshotWriteDeadline") {
		t.Fatal("write deadline снапшота не через wsSnapshotWriteDeadline: значение снова " +
			"разъедется по обработчику")
	}
	if strings.Contains(body, `Errorw("failed to send websocket snapshot"`) {
		t.Fatal("вернулось неинформативное сообщение «failed to send websocket snapshot»: " +
			"причина должна называть, что клиент не читает поток")
	}
}
