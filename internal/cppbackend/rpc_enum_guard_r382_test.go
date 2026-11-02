// rpc_enum_guard_r382_test.go — R382 (2026-11-01): предохранитель против цикла
// падений на RPC-перечислении устройств.
package cppbackend

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestR382_GuardArmsAndDisarms — маркер ставится перед вызовом и снимается после.
func TestR382_GuardArmsAndDisarms(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CPPWORKER_CONFIG_DIR", dir)

	if _, active := rpcEnumGuardActiveR382(); active {
		t.Fatal("до первого вызова предохранитель не активен")
	}

	armRPCEnumGuardR382([]string{"100.64.0.7:50052"})
	if _, active := rpcEnumGuardActiveR382(); !active {
		t.Fatal("после arm предохранитель обязан быть активен — иначе падение снова уйдёт в цикл")
	}
	if _, err := os.Stat(filepath.Join(dir, rpcEnumGuardFileName)); err != nil {
		t.Fatalf("маркер не создан в %s: %v", dir, err)
	}

	disarmRPCEnumGuardR382()
	if _, active := rpcEnumGuardActiveR382(); active {
		t.Fatal("после успешного возврата маркер обязан сниматься")
	}
}

// TestR382_GuardExpiresAfterCooldown — маркер старше кулдауна не блокирует
// навсегда: узел могли перезапустить, одна честная попытка раз в 10 минут нужна.
func TestR382_GuardExpiresAfterCooldown(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CPPWORKER_CONFIG_DIR", dir)

	old := time.Now().Add(-rpcEnumGuardCooldown - time.Minute).UTC().Format(time.RFC3339)
	payload := `{"armedAt":"` + old + `","servers":["100.64.0.7:50052"]}`
	if err := os.WriteFile(filepath.Join(dir, rpcEnumGuardFileName), []byte(payload), 0o644); err != nil {
		t.Fatalf("не записать маркер: %v", err)
	}

	if armedAt, active := rpcEnumGuardActiveR382(); active {
		t.Fatalf("маркер %s старше кулдауна, но предохранитель активен", armedAt.Format(time.RFC3339))
	}
}

// TestR382_GuardIgnoresBrokenMarker — битый маркер не должен навсегда
// отключать RPC-перечисление (иначе одна опечатка в файле = «раскладки нет»).
func TestR382_GuardIgnoresBrokenMarker(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CPPWORKER_CONFIG_DIR", dir)

	if err := os.WriteFile(filepath.Join(dir, rpcEnumGuardFileName), []byte("не json"), 0o644); err != nil {
		t.Fatalf("не записать маркер: %v", err)
	}
	if _, active := rpcEnumGuardActiveR382(); active {
		t.Fatal("битый маркер не должен считаться активным предохранителем")
	}
}
