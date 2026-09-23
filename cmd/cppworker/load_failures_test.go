// load_failures_test.go — R66d (2026-09-22): реестр последних провалов загрузки.
//
// Регресс, который он закрывает: async-загрузка отсутствующего GGUF падала за
// миллисекунды, но нигде не запоминалась, поэтому GET /api/models и
// /api/models/load/progress показывали «ничего нет» — и балансер поллил до
// maxWait (3-15 минут), а клиент (реальный Cline) получал 503 «подожди 90с».

package main

import (
	"errors"
	"testing"
	"time"
)

func TestLoadFailureRegistry_RecordGetClear(t *testing.T) {
	r := newLoadFailureRegistry(time.Minute)

	if _, ok := r.get("m1"); ok {
		t.Fatal("пустой реестр не должен ничего возвращать")
	}

	r.record("m1", errors.New("failed to open GGUF file 'models/x.gguf' (No such file or directory)"))
	e, ok := r.get("m1")
	if !ok {
		t.Fatal("после record запись должна быть видна")
	}
	if e.Err == "" || e.At.IsZero() {
		t.Errorf("запись пустая: %+v", e)
	}

	// Успешная загрузка снимает запись.
	r.clear("m1")
	if _, ok := r.get("m1"); ok {
		t.Error("после clear запись не должна находиться")
	}
}

func TestLoadFailureRegistry_TTLExpiry(t *testing.T) {
	r := newLoadFailureRegistry(30 * time.Second)
	base := time.Now()
	r.now = func() time.Time { return base }

	r.record("m2", errors.New("boom"))
	if _, ok := r.get("m2"); !ok {
		t.Fatal("свежая запись должна быть видна")
	}

	// Уходим за TTL: get сам вычищает запись.
	r.now = func() time.Time { return base.Add(31 * time.Second) }
	if _, ok := r.get("m2"); ok {
		t.Error("запись старше TTL не должна возвращаться")
	}
	if _, ok := r.m["m2"]; ok {
		t.Error("просроченная запись должна удаляться из карты")
	}

	// list тоже не должен показывать просроченные.
	r.now = func() time.Time { return base }
	r.record("m3", errors.New("boom2"))
	r.now = func() time.Time { return base.Add(2 * time.Minute) }
	if got := r.list(); len(got) != 0 {
		t.Errorf("list после TTL = %v, want пусто", got)
	}
}

func TestLoadFailureRegistry_EdgeCases(t *testing.T) {
	var nilReg *loadFailureRegistry
	// nil-получатель не должен паниковать (реестр может быть не инициализирован).
	nilReg.record("m", errors.New("x"))
	if _, ok := nilReg.get("m"); ok {
		t.Error("nil-реестр не должен ничего возвращать")
	}
	if got := nilReg.list(); len(got) != 0 {
		t.Errorf("nil-реестр list = %v, want пусто", got)
	}
	nilReg.clear("m")

	r := newLoadFailureRegistry(time.Minute)
	r.record("", errors.New("пустое имя — no-op"))
	r.record("named", nil) // nil error — no-op
	if got := r.list(); len(got) != 0 {
		t.Errorf("no-op вызовы не должны создавать записи, got %v", got)
	}
}
