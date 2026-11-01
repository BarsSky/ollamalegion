// upstream_request_id_r376_test.go — R376 (2026-11-01).
//
// Правило: id, который балансер выдал клиенту (X-Request-ID: req<hex>), обязан
// уехать в воркер вместе с генерацией — иначе `POST /api/cancel {"request_id":...}`
// не находит генерацию.
//
// Живой факт, из которого правило выросло: клиент получил `req8fab2156`, отменил
// по нему, а мастер и донор ответили `request_id not found: req8fab2156` — обе
// генерации доработали до конца (600 токенов, done_reason=stop). Причина: воркер
// заводил в трекере отмены собственный id (`gen-<наносекунды>`), потому что в
// запросе от балансера заголовка X-Request-Id не было.
package balancer

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"os"
	"testing"
)

// TestR376_StampSetsHeader — id из контекста попадает в запрос к воркеру.
func TestR376_StampSetsHeader(t *testing.T) {
	src, _ := http.NewRequest("POST", "http://worker/api/chat", nil)
	src = src.WithContext(context.WithValue(src.Context(), requestIDKey{}, "req8fab2156"))
	dst, _ := http.NewRequest("POST", "http://worker/api/chat", nil)

	stampUpstreamRequestIDFromRequestR376(dst, src)

	// Воркер читает r.Header.Get("X-Request-Id") — Go канонизирует имя.
	if got := dst.Header.Get("X-Request-Id"); got != "req8fab2156" {
		t.Errorf("X-Request-Id = %q, want req8fab2156", got)
	}
}

// TestR376_StampOverridesCopiedHeader — если заголовок клиента уже перенесён
// копированием, ставим ровно выданный клиенту id.
func TestR376_StampOverridesCopiedHeader(t *testing.T) {
	dst, _ := http.NewRequest("POST", "http://worker/api/chat", nil)
	dst.Header.Set("X-Request-ID", "client-own-id")

	stampUpstreamRequestIDR376(dst, "reqdeadbeef")

	if got := dst.Header.Get("X-Request-Id"); got != "reqdeadbeef" {
		t.Errorf("X-Request-Id = %q, want reqdeadbeef (итоговый id главнее скопированного)", got)
	}
}

// TestR376_EmptyRIDIsNoop — служебные вызовы без контекста не получают мусорный id.
func TestR376_EmptyRIDIsNoop(t *testing.T) {
	dst, _ := http.NewRequest("POST", "http://worker/api/chat", nil)
	stampUpstreamRequestIDR376(dst, "   ")
	if got := dst.Header.Get("X-Request-Id"); got != "" {
		t.Errorf("X-Request-Id = %q, want пусто", got)
	}
	if stampUpstreamRequestIDFromRequestR376(nil, nil); false {
		t.Fatal("unreachable")
	}
}

// TestR376STEWARD_EveryInferenceBuilderStampsID — стюард: каждый билдер запроса
// к воркеру на путях инференса обязан проставить id клиента. Новый билдер без
// этого снова сломает отмену по request_id — молча.
func TestR376STEWARD_EveryInferenceBuilderStampsID(t *testing.T) {
	files := []string{"proxy_request.go", "llamacpp_transport.go", "llamacpp_handlers_inference.go"}
	for _, name := range files {
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		var builders, stamped []string
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			// Вызовы штампов — обычные функции (Ident), билдеры — http.NewRequestWithContext
			// (SelectorExpr). Обрабатываем оба вида: иначе стюард «не видит» вставки.
			switch fun := call.Fun.(type) {
			case *ast.Ident:
				if fun.Name == "stampUpstreamRequestIDR376" || fun.Name == "stampUpstreamRequestIDFromRequestR376" {
					stamped = append(stamped, fset.Position(call.Pos()).String())
				}
			case *ast.SelectorExpr:
				if fun.Sel.Name != "NewRequestWithContext" {
					return true
				}
				if pkg, ok := fun.X.(*ast.Ident); !ok || pkg.Name != "http" {
					return true
				}
				builders = append(builders, fset.Position(call.Pos()).String())
			}
			return true
		})
		if len(builders) == 0 {
			t.Fatalf("%s: билдеров запросов не найдено — тест проверяет не то", name)
		}
		// Грубая, но честная проверка охвата: штампов должно быть не меньше, чем
		// билдеров (в файле на каждый билдер — свой вызов, включая ретраи).
		if len(stamped) < len(builders) {
			t.Errorf("%s: билдеров %d, проставлений id %d.\n"+
				"    Каждый запрос к воркеру на пути инференса обязан нести id клиента (R376),\n"+
				"    иначе отмена по request_id снова будет отвечать «not found».",
				name, len(builders), len(stamped))
		}
	}
}
