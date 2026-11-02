// rpc_enum_guard_r382.go — R382 (2026-11-01): предохранитель против ЦИКЛА
// падений на RPC-перечислении устройств.
//
// ЧТО ПРОИСХОДИТ. Перед загрузкой планировщик (memfit) должен знать свободную
// память узлов раскладки, поэтому он зовёт bridge.CollectDevices с адресами
// узлов. Внутри — llama.cpp'шный `ggml_backend_rpc_add_server`: он делает
// hello-handshake к узлу. Если узел принял TCP, но handshake не состоялся
// (слот узла занят чужим соединением — rpc-server обслуживает одно за раз),
// llama.cpp падает SIGSEGV'ом прямо в этом вызове (живой стек 2026-11-01:
// bridge_collect_devices → ggml_backend_rpc_add_server → SIGSEGV, PC в libc).
// Go-слой это поймать не может: падает процесс целиком, и контейнер
// перезапускается. Повторная попытка — снова падение, пока узел не освободится:
// КРУГ падений.
//
// ЧТО ДЕЛАЕМ. Сигнал о риске не «TCP недоступен» (это уже проверяется), а сам
// факт предыдущего падения: перед рискованным вызовом ставим файл-маркер, после
// успешного возврата — снимаем. Если маркер на месте при старте процесса, значит
// прошлая попытка не вернулась: RPC-перечисление пропускается на время
// остывания (rpcEnumGuardCooldown), планировщик считает только локальную карту,
// а причина и действие названы в логе. Это не лечит llama.cpp — это выводит
// систему из цикла: одно падение вместо бесконечного.
//
// МАРКЕР ЛЕЖИТ ТАМ ЖЕ, ГДЕ ОПЕРАТОРСКИЕ ПЕРЕОПРЕДЕЛЕНИЯ (CPPWORKER_CONFIG_DIR,
// по умолчанию ./config): каталог уже смонтирован в контейнере и переживает
// перезапуск.
package cppbackend

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"ollama-loadbalancer/c/bridge"
	"ollama-loadbalancer/pkg/logger"
)

const (
	// rpcEnumGuardFileName — файл-маркер «RPC-перечисление не вернулось».
	rpcEnumGuardFileName = "rpc-enum-crash-guard.json"
	// rpcEnumGuardCooldown — сколько не трогаем RPC-перечисление после падения.
	rpcEnumGuardCooldown = 10 * time.Minute
)

// rpcEnumGuardDir — каталог маркера: тот же, что у операторских переопределений.
func rpcEnumGuardDir() string {
	dir := strings.TrimSpace(os.Getenv("CPPWORKER_CONFIG_DIR"))
	if dir == "" {
		dir = "config"
	}
	return dir
}

// rpcEnumGuardPath — полный путь маркера.
func rpcEnumGuardPath() string {
	return filepath.Join(rpcEnumGuardDir(), rpcEnumGuardFileName)
}

// armRPCEnumGuardR382 — поставить маркер ПЕРЕД рискованным вызовом.
func armRPCEnumGuardR382(servers []string) {
	record := map[string]interface{}{
		"armedAt": time.Now().UTC().Format(time.RFC3339),
		"servers": servers,
	}
	raw, err := json.Marshal(record)
	if err != nil {
		return
	}
	path := rpcEnumGuardPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	_ = os.WriteFile(path, raw, 0o644)
}

// disarmRPCEnumGuardR382 — снять маркер ПОСЛЕ успешного возврата.
func disarmRPCEnumGuardR382() {
	_ = os.Remove(rpcEnumGuardPath())
}

// rpcEnumGuardActiveR382 — активен ли предохранитель: (когда поставлен, активен).
//
// Маркер старше кулдауна считается истёкшим: один честный повторный заход раз в
// 10 минут лучше, чем «никогда» — узел за это время могли перезапустить.
func rpcEnumGuardActiveR382() (time.Time, bool) {
	raw, err := os.ReadFile(rpcEnumGuardPath())
	if err != nil {
		return time.Time{}, false
	}
	var record struct {
		ArmedAt string   `json:"armedAt"`
		Servers []string `json:"servers"`
	}
	if err := json.Unmarshal(raw, &record); err != nil {
		return time.Time{}, false
	}
	armedAt, err := time.Parse(time.RFC3339, record.ArmedAt)
	if err != nil {
		return time.Time{}, false
	}
	if time.Since(armedAt) >= rpcEnumGuardCooldown {
		return armedAt, false
	}
	return armedAt, true
}

// rpcEnumGuardSkipReasonR382 — человеческая причина пропуска для лога.
func rpcEnumGuardSkipReasonR382(armedAt time.Time) string {
	return "предыдущее RPC-перечисление устройств не вернулось (llama.cpp падает SIGSEGV на " +
		"неудачном hello, когда слот узла занят) — поставлен предохранитель в " +
		armedAt.Format(time.RFC3339) + "; грузим с локальной картой. " +
		"Действие: перезапустить rpc-server узла (или подождать, пока он освободится)"
}

// logRPCEnumGuardSkipR382 — сообщить о пропуске один раз за вызов.
func logRPCEnumGuardSkipR382(armedAt time.Time) {
	logger.Get().Warnw("R382: RPC-перечисление устройств пропущено — предохранитель после падения",
		"armed_at", armedAt.Format(time.RFC3339),
		"cooldown", rpcEnumGuardCooldown.String(),
		"reason", rpcEnumGuardSkipReasonR382(armedAt))
}

// collectDevicesWithGuardR382 — рискованный вызов моста под предохранителем.
//
// Возвращает (снимок, пропущено). Пропущено=true означает: RPC-перечисление не
// выполнялось (предохранитель активен), снимок содержит ТОЛЬКО локальные
// устройства — планировщик будет консервативнее, зато процесс выживет.
func collectDevicesWithGuardR382(cfg bridge.ModelConfig, servers []string) ([]bridge.DeviceInfo, bool) {
	if armedAt, active := rpcEnumGuardActiveR382(); active {
		logRPCEnumGuardSkipR382(armedAt)
		local := cfg
		local.RPCServers = nil
		return bridge.CollectDevices(local), true
	}
	armRPCEnumGuardR382(servers)
	out := bridge.CollectDevices(cfg)
	disarmRPCEnumGuardR382()
	return out, false
}
