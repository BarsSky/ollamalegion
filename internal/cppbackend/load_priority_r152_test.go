package cppbackend

import (
	"testing"
)

// R152 (2026-10-16): ПРИОРИТЕТ ОПЕРАТОРА — идущая перезагрузка не должна выглядеть
// как «модель уже загружена».
//
// ЖИВОЙ ДЕФЕКТ (мастер). Оператор нажал «применить и перезагрузить» (асинхронный
// reload, ctx=8192). Пока он шёл, прилетел load-with-params. TryLockLoad проверял
// b.models РАНЬШЕ b.loading, поэтому видел СТАРУЮ запись модели (reload ещё не убрал
// её) и отвечал «model already loaded». handleLoadWithParams трактовал это как
// «параметры изменились» → UnloadModel + новая загрузка → первая получила
// "load cancelled: model load aborted by user (R60.57 ... BRIDGE_ERR_ABORTED)", а
// модель поднялась с параметрами ВТОРОГО запроса (32768/q8_0/reasoning off).
//
// Инвариант, который здесь закреплён: если загрузка/перезагрузка УЖЕ идёт, второй
// запрос обязан получить «грузится» (false, nil) — тогда вызывающий уходит в
// respondWhileAnotherLoadInProgress (202 + progressUrl) и НЕ выгружает модель.
func TestR152ReloadInProgressReportedAsLoading(t *testing.T) {
	b := NewBackend(Config{DefaultCtxSize: 512, DefaultBatchSize: 64})
	if b == nil {
		t.Fatal("NewBackend вернул nil")
	}

	// Сцена: модель ЗАГРУЖЕНА (её запись в b.models ещё на месте) и оператор начал
	// её перезагрузку — именно так выглядит стенд в момент применения параметров.
	b.mu.Lock()
	b.models["m"] = &modelInstance{info: ModelInfo{Name: "m"}}
	b.mu.Unlock()

	ok, err := b.TryLockReload("m")
	if !ok || err != nil {
		t.Fatalf("оператор не смог начать перезагрузку: ok=%v err=%v", ok, err)
	}
	defer b.UnlockLoad("m")

	// Второй запрос (авто-подъём/клиент) во время перезагрузки.
	ok2, err2 := b.TryLockLoad("m")
	if ok2 || err2 != nil {
		t.Errorf("идёт перезагрузка -> ожидали (false, nil) «грузится», получили (%v, %v). "+
			"С err != nil вызывающий решает «параметры изменились» и ВЫГРУЖАЕТ модель, "+
			"отменяя работу оператора (R60.57 abort)", ok2, err2)
	}
}

// TestR152LoadedWithoutLoadKeepsOldBehaviour — прежняя семантика не сломана:
// модель загружена и никакой загрузки не идёт -> (true, "already loaded").
func TestR152LoadedWithoutLoadKeepsOldBehaviour(t *testing.T) {
	b := NewBackend(Config{DefaultCtxSize: 512, DefaultBatchSize: 64})
	if b == nil {
		t.Fatal("NewBackend вернул nil")
	}
	b.mu.Lock()
	b.models["m"] = &modelInstance{info: ModelInfo{Name: "m"}}
	b.mu.Unlock()

	ok, err := b.TryLockLoad("m")
	if !ok || err == nil {
		t.Errorf("загружено без активной загрузки -> ожидали (true, err!=nil), получили (%v, %v)", ok, err)
	}
}

// TestR152FreeLoadStillTakesLock — обычный случай (ни загрузки, ни модели) работает.
func TestR152FreeLoadStillTakesLock(t *testing.T) {
	b := NewBackend(Config{DefaultCtxSize: 512, DefaultBatchSize: 64})
	if b == nil {
		t.Fatal("NewBackend вернул nil")
	}
	ok, err := b.TryLockLoad("fresh")
	if !ok || err != nil {
		t.Fatalf("свободная загрузка: ok=%v err=%v", ok, err)
	}
	// Лок занят нами: повторный TryLockLoad обязан увидеть «грузится».
	ok2, err2 := b.TryLockLoad("fresh")
	if ok2 || err2 != nil {
		t.Errorf("повторная попытка во время своей загрузки -> ожидали (false, nil), получили (%v, %v)", ok2, err2)
	}
	b.UnlockLoad("fresh")
}
