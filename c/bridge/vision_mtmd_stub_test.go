//go:build llama_stub

package bridge

import (
	"strings"
	"testing"
)

// R248/B2: в stub-сборке нет mtmd, поэтому загрузка проектора обязана
// завершаться явной ошибкой, а не тихим "ok". Это защищает cppworker от
// публикации capabilities.vision без реального проектора.
func TestR248_StubLoadMMProjFailsExplicitly(t *testing.T) {
	m := &ModelHandle{}
	err := m.LoadMMProj("/models/mmproj-F16.gguf")
	if err == nil {
		t.Fatal("LoadMMProj in llama_stub build must return an error, got nil")
	}
	if !strings.Contains(err.Error(), "llama_stub") {
		t.Fatalf("error should name the stub build, got %q", err.Error())
	}
}

// R248/B2: пустой путь — ошибка и без mtmd (контракт API, не сборки).
func TestR248_StubLoadMMProjEmptyPath(t *testing.T) {
	m := &ModelHandle{}
	if err := m.LoadMMProj(""); err == nil {
		t.Fatal("LoadMMProj(\"\") must fail")
	}
}

// R248/B2: без загруженного проектора capability.vision = false.
func TestR248_StubMMProjLoadedIsFalse(t *testing.T) {
	if (&ModelHandle{}).MMProjLoaded() {
		t.Fatal("MMProjLoaded must be false without a projector")
	}
	if (*ModelHandle)(nil).MMProjLoaded() {
		t.Fatal("MMProjLoaded on nil handle must be false")
	}
	if err := (*ModelHandle)(nil).LoadMMProj("/models/mmproj-F16.gguf"); err == nil {
		t.Fatal("LoadMMProj on nil handle must fail")
	}
}
