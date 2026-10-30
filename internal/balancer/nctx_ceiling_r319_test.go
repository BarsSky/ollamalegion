// nctx_ceiling_r319_test.go — R319 (2026-10-30): кластерный потолок окна виден
// отдельно от профилей.
//
// ЖИВАЯ ЖАЛОБА: «модель обучена на 131072, VRAM даёт 131072 — почему окно 32768?».
// Второе число (LB_NCTX_RELOAD_MAX_N_CTX) не отдавалось ни в одном ответе API,
// если у модели не было профиля, — то есть причина ограничения отсутствовала
// именно там, где её ищет оператор.
//
// Что фиксируем:
//  1. заданный потолок отдаётся с limitedBy="env" и числом в тексте причины;
//  2. незаданный потолок — 0 и пустой limitedBy, при этом reason объясняет, что
//     предел проверит загрузка (VRAM/GGUF), а не молчит;
//  3. nil-прокси не паникует (поле диагностическое — карточка не должна ломаться).
package balancer

import (
	"strings"
	"testing"
)

// TestR319_ClusterCeilingReportsEnv — потолок задан: число и источник.
func TestR319_ClusterCeilingReportsEnv(t *testing.T) {
	p := newCeilingTestProxy(t, "gemma-4-E4B-it-Q4_K_M", nil, 32768)

	ceiling, limitedBy, reason := p.ClusterNCtxCeiling()
	if ceiling != 32768 {
		t.Fatalf("envCeiling = %d, want 32768", ceiling)
	}
	if limitedBy != "env" {
		t.Errorf("limitedBy = %q, want env", limitedBy)
	}
	if !strings.Contains(reason, "LB_NCTX_RELOAD_MAX_N_CTX=32768") {
		t.Errorf("reason без имени переменной и значения: %q", reason)
	}
	if !strings.Contains(reason, "профил") {
		t.Errorf("reason не говорит, что профиль может быть выше: %q", reason)
	}
}

// TestR319_ClusterCeilingUnsetIsHonest — потолка нет: 0, пустой источник, но не
// пустой текст: «предел проверит загрузка».
func TestR319_ClusterCeilingUnsetIsHonest(t *testing.T) {
	p := newCeilingTestProxy(t, "gemma-4-E4B-it-Q4_K_M", nil, 0)

	ceiling, limitedBy, reason := p.ClusterNCtxCeiling()
	if ceiling != 0 {
		t.Fatalf("envCeiling = %d, want 0 (потолок не задан)", ceiling)
	}
	if limitedBy != "" {
		t.Errorf("limitedBy = %q, want пусто", limitedBy)
	}
	if reason == "" {
		t.Fatal("reason пуст — оператор снова не узнает, чем ограничено окно")
	}
	for _, want := range []string{"VRAM", "GGUF"} {
		if !strings.Contains(reason, want) {
			t.Errorf("reason не называет физический предел %q: %q", want, reason)
		}
	}
}

// TestR319_ClusterCeilingNilProxy — диагностическое поле не паникует.
func TestR319_ClusterCeilingNilProxy(t *testing.T) {
	var p *Proxy
	ceiling, limitedBy, reason := p.ClusterNCtxCeiling()
	if ceiling != 0 || limitedBy != "" {
		t.Fatalf("nil-прокси: ceiling=%d limitedBy=%q, want 0/пусто", ceiling, limitedBy)
	}
	if reason == "" {
		t.Fatal("nil-прокси: reason пуст — поле должно быть честным, а не пустым")
	}
}
