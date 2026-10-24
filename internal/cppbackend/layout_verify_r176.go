// layout_verify_r176.go — R176 (2026-10-19): СВЕРКА «ПЕРЕДАЛИ / ЗАГРУЗИЛОСЬ».
//
// ЗАЧЕМ. R174 локализовал причину исчезновения раскладки при «Применить профиль»:
// бюджет и допуск узла в порядке (`MemfitBudget: ёмкость RPC-узлов добавлена`,
// `RPC-раскладка передана в мост`), но при самой загрузке llama.cpp печатал только
//
//	llama_prepare_model_devices: using device CUDA0 (RTX 3070) - 7097 MiB free
//
// без единой строки `using device RPC0`, и все 42 слоя уезжали на локальную карту.
// Отдельная проба устройства за 0.6 с до этого узел ВИДЕЛА, поэтому «тихое
// выпадение» сервера на этапе загрузки проходило незамеченным: загрузчик не
// сверял, что переданные серверы действительно стали устройствами модели.
//
// Здесь две вещи:
//  1. layoutRegistration — чистое правило сверки (покрыто тестами): какие серверы
//     передали и какие RPC-устройства реально участвовали в загрузке.
//  2. splitFitsDevices — правило «сплит длины N применяется только к N устройствам»:
//     при reload мы копировали сплит прошлого инстанса, и при уменьшении числа
//     устройств лишние пропорции молча игнорировались.
package cppbackend

import (
	"time"
	"strings"

	"ollama-loadbalancer/c/bridge"
)

// layoutRegistration — сверка переданных серверов раскладки с фактической
// раскладкой слоёв по устройствам.
//
// Возвращает (degraded, reason): degraded=true означает «серверы передали, но
// ни один из них не стал устройством модели» — раскладки НЕТ, и об этом надо
// сказать, а не молчать.
//
// Правила:
//   - серверов не передавали → сверять нечего (degraded=false);
//   - передали и есть устройство RPC* с ненулевым числом слоёв → раскладка есть;
//   - передали, но RPC-устройства не появилось или на нём 0 слоёв → degraded.
func layoutRegistration(servers []string, layers []bridge.DeviceLayerRange) (bool, string) {
	if len(servers) == 0 {
		return false, ""
	}
	var rpcDevices []string
	for _, l := range layers {
		if !strings.HasPrefix(strings.ToUpper(strings.TrimSpace(l.Device)), "RPC") {
			continue
		}
		if l.Count > 0 {
			name := l.Device
			if l.Node != "" {
				name += "[" + l.Node + "]"
			}
			rpcDevices = append(rpcDevices, name)
		}
	}
	if len(rpcDevices) > 0 {
		return false, ""
	}
	return true, "узлы " + strings.Join(servers, ",") + " переданы в мост, но ни один не стал " +
		"устройством модели: раскладка не применилась, слои остались на локальных устройствах " +
		"(узел, скорее всего, ещё держит соединение прошлой сессии — он обслуживает одно за раз)"
}

// splitFitsDevices — применим ли готовый tensor_split к фактическому числу устройств.
//
// R176: при reload сплит копировался из прошлого инстанса
// (`manual tensor split applied tensorSplit=[0.538, 0.462]`), хотя устройств могло
// стать меньше (RPC-узел не зарегистрировался). Лишние пропорции llama.cpp молча
// игнорирует, и ошибка не всплывает. Правило: длина сплита обязана совпадать с
// числом устройств, иначе сплит пересчитывается.
func splitFitsDevices(split []float32, deviceCount int) bool {
	if len(split) == 0 {
		// Сплита нет — пересчитывать нечего, авто-путь решает сам.
		return true
	}
	return deviceCount > 0 && len(split) == deviceCount
}

// waitRPCNodeReadyForLoad — дождаться, пока узел РЕАЛЬНО ОБСЛУЖИТ нас, а не просто
// примет TCP-соединение.
//
// R177 (2026-10-19). Причина, найденная в R174/R176: узел обслуживает ОДНО
// соединение за раз. При reload старая сессия закрывается поздно, и загрузка
// модели «не видит» узел: llama.cpp печатает только `using device CUDA0`, RPC0
// отсутствует, все слои уезжают на локальную карту (и теперь это честно
// помечается layout_degraded).
//
// ПРОВЕРКА ДОЛЖНА БЫТЬ ЗАПРОСОМ, А НЕ CONNECT. `bridge.CollectDevices` с
// RPCServers спрашивает у узла память устройства — это команда протокола, которую
// узел может выполнить только когда ОН СВОБОДЕН. Пока он доигрывает прошлую
// сессию, наш запрос ждёт в очереди accept и упирается в таймаут. Поэтому
// успешный снимок = узел действительно готов принять загрузку.
//
// Возвращает (готов, номер успешной попытки). Попытки короткие: узел освобождает
// слот за десятки-сотни миллисекунд после закрытия прошлой сессии.
func (b *Backend) waitRPCNodeReadyForLoad(servers []string, attempts int, delay time.Duration) (bool, int) {
	if b == nil || len(servers) == 0 {
		return true, 0
	}
	if attempts <= 0 {
		attempts = 1
	}
	nodeCfg := bridge.ModelConfig{SplitMode: b.cfg.DefaultSplitMode, RPCServers: servers}
	for i := 1; i <= attempts; i++ {
		snapshot := bridge.CollectDevices(nodeCfg)
		if len(remoteDevices(snapshot)) > 0 {
			return true, i
		}
		if i < attempts && delay > 0 {
			time.Sleep(delay)
		}
	}
	return false, attempts
}