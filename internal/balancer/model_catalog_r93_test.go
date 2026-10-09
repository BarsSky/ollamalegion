package balancer

import (
	"testing"
	"time"
)

// R93 (2026-10-09): каталог моделей обязан не только хранить список, но и
// ФИКСИРОВАТЬ ИЗМЕНЕНИЯ (что добавилось/исчезло и когда) и различать «моделей нет»
// от «каталог недоступен». Живой случай: каталог моделей (bind-mount) удалили,
// /api/models/files воркера отдавал dirError и пустой список, а балансерный каталог
// продолжал показывать 3 модели — WebUI рисовал призраков.
func TestModelCatalog_ChangeTracking_R93(t *testing.T) {
	mc := newModelCatalog()
	now := time.Now()

	// Первый снимок — это «узнали», а не «изменилось».
	if ch := mc.store("node-a", []string{"a.gguf", "b.gguf"}, "", now); ch.Changed {
		t.Error("первый снимок не должен считаться изменением состава")
	}

	// Удалили a, добавили c.
	ch := mc.store("node-a", []string{"b.gguf", "c.gguf"}, "", now.Add(time.Minute))
	if !ch.Changed {
		t.Fatal("изменение состава не зафиксировано")
	}
	if len(ch.Added) != 1 || ch.Added[0] != "c.gguf" {
		t.Errorf("added = %v, ожидался [c.gguf]", ch.Added)
	}
	if len(ch.Removed) != 1 || ch.Removed[0] != "a.gguf" {
		t.Errorf("removed = %v, ожидался [a.gguf]", ch.Removed)
	}

	snap := mc.snapshot()["node-a"]
	if snap.ChangedAt.IsZero() {
		t.Error("changedAt не проставлен — WebUI не покажет «когда изменилось»")
	}
	if len(snap.Added) != 1 || len(snap.Removed) != 1 {
		t.Errorf("снимок не несёт диф: added=%v removed=%v", snap.Added, snap.Removed)
	}
	if len(snap.Files) != 2 {
		t.Errorf("файлов в снимке %d, ожидалось 2 (b, c)", len(snap.Files))
	}

	// Тот же состав ещё раз — изменения нет, но история сохраняется.
	ch2 := mc.store("node-a", []string{"c.gguf", "b.gguf"}, "", now.Add(2*time.Minute))
	if ch2.Changed {
		t.Error("повторный снимок с тем же составом не должен считаться изменением")
	}
	snap2 := mc.snapshot()["node-a"]
	if !snap2.ChangedAt.Equal(snap.ChangedAt) {
		t.Errorf("история изменения потеряна: changedAt %v → %v", snap.ChangedAt, snap2.ChangedAt)
	}

	// Каталог воркера стал недоступен: список пуст + причина.
	dirErr := "create models dir: mkdir /app/models: file exists"
	ch3 := mc.store("node-a", nil, dirErr, now.Add(3*time.Minute))
	if !ch3.Changed {
		t.Error("исчезновение всех моделей обязано быть изменением")
	}
	snap3 := mc.snapshot()["node-a"]
	if len(snap3.Files) != 0 {
		t.Errorf("файлы остались в снимке (%v) — WebUI покажет призраков", snap3.Files)
	}
	if snap3.DirError != dirErr {
		t.Errorf("dirError = %q, ожидался %q", snap3.DirError, dirErr)
	}
	if has, known, _ := mc.lookup("node-a", "b.gguf"); has || !known {
		t.Errorf("после исчезновения каталога lookup(b.gguf) = (has=%v, known=%v), ожидалось (false, true)", has, known)
	}
}
