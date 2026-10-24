// rpc_mode_r115.go — R115: «RPC выключен флагом» должно быть однозначным.
//
// ЗАЧЕМ. Раскладка управляется двумя переменными: CPPWORKER_RPC_ENABLE
// (включена/выключена) и CPPWORKER_RPC_SERVERS (список узлов). До R115 список
// узлов СИЛОЙ включал раскладку (`cfg.RPCEnabled = true` при непустом списке),
// поэтому явное `CPPWORKER_RPC_ENABLE=false` не работало: оператор выключал
// режим, а мастер продолжал дозваниваться до узлов и планировать по их памяти.
// Единственным надёжным способом выключить раскладку было стереть список узлов,
// то есть потерять настройку.
//
// ПРАВИЛО. Раскладка работает тогда и только тогда, когда она ВКЛЮЧЕНА и есть
// хотя бы один узел. Узлы при выключенном режиме не используются НИГДЕ: ни в
// параметрах моста, ни в бюджете памяти, ни в сторожевых проверках. Поэтому в
// коде нет прямых обращений к cfg.RPCServers на путях загрузки — только через
// activeRPCServers, который возвращает nil при выключенном режиме.
package cppbackend

import "time"

// activeRPCServers — узлы раскладки, если режим включён, иначе nil.
//
// nil, а не пустой срез намеренно: все потребители (мост, memfit, сторож)
// трактуют nil как «раскладки нет» и планируют по локальным GPU, то есть
// выключение флагом даёт ровно то же поведение, что сборка без RPC.
func (b *Backend) activeRPCServers() []string {
	if b == nil || !b.cfg.RPCEnabled {
		return nil
	}
	if len(b.cfg.RPCServers) == 0 {
		return nil
	}
	return b.cfg.RPCServers
}

// RPCEnabled — включена ли раскладка (для /api/info и WebUI: режим должен быть
// виден снаружи, а не только в логе).
func (b *Backend) RPCEnabled() bool {
	if b == nil {
		return false
	}
	return b.cfg.RPCEnabled
}

// ActiveRPCServers — узлы, реально участвующие в раскладке (пустой срез при
// выключенном режиме). Нужен отчёту и WebUI, чтобы список в конфиге не выглядел
// работающим.
func (b *Backend) ActiveRPCServers() []string {
	out := b.activeRPCServers()
	if len(out) == 0 {
		return []string{}
	}
	return append([]string(nil), out...)
}

// SetRPCConfig — применить режим раскладки к ЖИВОМУ конфигу бэкенда.
//
// ЗАЧЕМ ОТДЕЛЬНЫЙ МЕТОД. `PUT /api/v1/cppworker/config/update` пишет в
// `currentConfig` (объект пакета cppworker), а бэкенд читает СВОЮ копию
// `b.cfg`. Живая проверка 2026-10-13 это показала: ответ был
// `{"applied":["rpcEnabled","rpcServers(cleared)"]}`, а `/api/models` продолжал
// отдавать `rpc_enabled=true`, один узел и 7009 MB удалённой памяти. То есть
// настройка «сохранялась» без эффекта — и WebUI показывал бы успех, ничего не
// меняя. Поэтому после применения конфига вызывающий обязан донести режим до
// бэкенда.
//
// Сброс снимка устройств здесь обязателен: он кэшируется на 30 с, и без сброса
// новый список узлов не начал бы действовать до истечения TTL.
func (b *Backend) SetRPCConfig(enabled bool, servers []string) {
	if b == nil {
		return
	}
	b.mu.Lock()
	b.cfg.RPCEnabled = enabled
	if enabled {
		b.cfg.RPCServers = append([]string(nil), servers...)
	} else {
		// Выключено — узлов нет вовсе (та же семантика, что у флага при старте).
		b.cfg.RPCServers = nil
	}
	b.mu.Unlock()

	// ВАЖНО: rpcDevMu берём ПОСЛЕ b.mu и не удерживая его — обратный порядок дал
	// бы взаимную блокировку с rpcDeviceSnapshot (он держит rpcDevMu и внутри
	// зовёт layoutBusy, которому нужен b.mu).
	b.rpcDevMu.Lock()
	b.rpcDevices = nil
	b.rpcDevAt = time.Time{}
	b.rpcDevMu.Unlock()
}

// layoutBusy — идёт ли загрузка модели или инференс (и что именно).
//
// ЗАЧЕМ ЭТО ОТДЕЛЬНАЯ ПРОВЕРКА. RPC-узел (`rpc-server`) обслуживает соединения по
// одному, и запрос памяти устройства — такая же RPC-команда, как передача
// тензора. Если наша фоновая проверка открывает соединение, пока идёт загрузка
// или генерация, ответ приходит неразобранным, а llama.cpp на это отвечает
// `RPC_STATUS_ASSERT` -> `GGML_ABORT` (ggml-rpc.cpp:30), то есть падением всего
// процесса.
//
// ЖИВОЕ ДОКАЗАТЕЛЬСТВО (2026-10-13, обе машины на ОДНОМ образе и коммите):
//
//	load_tensors:   CPU_Mapped model buffer size = 10993.98 MiB
//	load_tensors:        CUDA0 model buffer size =  2116.47 MiB
//	load_tensors: RPC0[192.0.2.10:50052] model buffer size =  2246.03 MiB
//	ggml-rpc.cpp:823: Remote RPC server crashed or returned malformed response
//
// Строка 823 — это `get_device_memory()`, то есть ОПРОС ПАМЯТИ, а не загрузка
// весов: падал фоновый сбор бюджета, а не сама раскладка. Поэтому до узлов нельзя
// дозваниваться, пока идёт работа, из-за которой соединение уже занято.
func (b *Backend) layoutBusy() (bool, string) {
	if b == nil {
		return false, ""
	}
	// НЕБЛОКИРУЮЩАЯ проверка замка загрузки.
	//
	// TryLock вместо Lock — не оптимизация, а защита от самоблокировки: функция
	// вызывается из пути загрузки (через сбор бюджета), а тот держит loadMu.
	// Обычный Lock в той же горутине заклинил бы процесс: живой прогон
	// 2026-10-13 — загрузка встала на `loading`, CPU ~5%, логи замолчали,
	// падения не было (restarts=0).
	//
	// Занятый замок трактуем как «идёт загрузка»: это консервативно и безопасно —
	// цена ошибки в том, что бюджет на время работы считается по локальной карте.
	if !b.loadMu.TryLock() {
		return true, "load_lock_held"
	}
	loading := len(b.loading) > 0
	b.loadMu.Unlock()
	if loading {
		return true, "model_load"
	}

	// Тот же приём для замка реестра моделей: он тоже может быть занят вызывающим
	// (путь загрузки считает бюджет, держа замок), и обычный RLock заклинил бы
	// процесс. Занятый замок = «идёт работа».
	if !b.mu.TryRLock() {
		return true, "models_lock_held"
	}
	names := make([]string, 0, len(b.models))
	for name := range b.models {
		names = append(names, name)
	}
	b.mu.RUnlock()
	for _, name := range names {
		if b.activeQueriesFor(name) > 0 {
			return true, "inference:" + name
		}
	}
	return false, ""
}

// LayoutBreakdown — ёмкость памяти по источникам: локальная карта и узлы.
//
// ЗАЧЕМ. `available_vram_mb` в /api/models — это ИТОГ раскладки (memfit считает
// бюджет как локальная карта + узлы), и снаружи по нему нельзя понять, откуда
// взялись гигабайты: «8 ГБ» и «8 ГБ + 7 ГБ донора» выглядят одинаково. Оператор
// видел одно число, балансер — тоже, а разница между ними решает, влезет модель
// целиком или уйдёт в partial_offload.
//
// Никаких новых зондов: итог берётся из MemfitBudget (единственный расчёт), а
// вклад узлов — из того же кэшированного снимка устройств, что и планирование.
// Локальное = итог − узлы, поэтому два числа не могут разойтись с решением.
// Источники вклада узлов в раскладку (R132, 2026-10-16).
//
// Смысл поля: снаружи «0 МБ узлов» и «узлы не измерялись» выглядят одинаково, а
// это разные утверждения. `none` — узлы на этом пути НЕ опрашивались (и брать
// вчерашний снимок за сегодняшний нельзя), `cache` — взято из уже собранного
// снимка, `probed` — узлы спрошены (живой опрос либо его кэш в пределах TTL).
const (
	RemoteSourceNone   = "none"
	RemoteSourceCache  = "cache"
	RemoteSourceProbed = "probed"
)

type LayoutBreakdown struct {
	RPCEnabled bool
	Servers    []string
	// LayoutFree/LayoutTotal — то, чем планирует memfit (локально + узлы).
	LayoutFree, LayoutTotal uint64
	// LocalFree/LocalTotal — только устройства этой машины.
	LocalFree, LocalTotal uint64
	// RemoteFree/RemoteTotal/Nodes — вклад узлов раскладки.
	RemoteFree, RemoteTotal uint64
	Nodes                   int
	Known                   bool
	// RemoteSource — откуда взялся вклад узлов: none / cache / probed.
	RemoteSource string
}

// LayoutBreakdown собирает разбивку. При выключенном режиме узлы не спрашиваются
// вовсе: Remote* = 0, Nodes = 0, Layout == Local.
//
// ВНИМАНИЕ: этот вариант МОЖЕТ дозвониться до узлов (через MemfitBudget и
// RPCCapacityBytes). Он для путей, которым ёмкость узлов нужна как ОСНОВАНИЕ
// решения (загрузка/раскладка). Справочные (read-only) ручки обязаны звать
// LayoutBreakdownCached: дозвон до молчащего мёртвого узла уходит в C-мост и
// роняет мастер (R130/R131/R132).
func (b *Backend) LayoutBreakdown() LayoutBreakdown {
	out := LayoutBreakdown{}
	if b == nil {
		return out
	}
	out.RPCEnabled = b.RPCEnabled()
	out.Servers = b.ActiveRPCServers()

	budget := b.MemfitBudget()
	out.Known = budget.VRAMKnown
	out.LayoutFree = uint64(budget.VRAMFree)
	out.LayoutTotal = uint64(budget.VRAMTotal)

	out.RemoteFree, out.RemoteTotal, out.Nodes = b.RPCCapacityBytes()
	if out.Nodes > 0 {
		out.RemoteSource = RemoteSourceProbed
	} else {
		out.RemoteSource = RemoteSourceNone
	}

	out.LocalFree, out.LocalTotal = out.LayoutFree, out.LayoutTotal
	if out.RemoteFree > 0 && out.LocalFree >= out.RemoteFree {
		out.LocalFree = out.LayoutFree - out.RemoteFree
	}
	if out.RemoteTotal > 0 && out.LocalTotal >= out.RemoteTotal {
		out.LocalTotal = out.LayoutTotal - out.RemoteTotal
	}
	return out
}

// LayoutBreakdownCached — та же разбивка, но узлы НЕ опрашиваются (R132,
// 2026-10-16): вклад узлов берётся из уже собранного снимка (см.
// MemfitBudgetCached/cachedRemoteVRAM), а если снимка нет или он протух — его
// просто нет, и это видно по RemoteSource.
//
// ЖИВОЙ ДЕФЕКТ, ЗАКРЫТЫЙ ЭТИМ. После R131 в справочной ручке `GET /api/models`
// остался третий вызов к узлам — `LayoutBreakdown()`. На узле, который принимает
// TCP и молчит (дублёр: слушатель в сети стека; гейт R130 такую тишину считает
// признаком живого rpc-server), чтение списка моделей снова уходило в C-мост.
func (b *Backend) LayoutBreakdownCached() LayoutBreakdown {
	out := LayoutBreakdown{}
	if b == nil {
		return out
	}
	out.RPCEnabled = b.RPCEnabled()
	out.Servers = b.ActiveRPCServers()

	// Локальная часть берётся чистым локальным расчётом, а не вычитанием узлов из
	// итога: так Local и Remote не могут разъехаться на гонке обновления снимка.
	local := b.MemfitBudgetLocal()
	out.Known = local.VRAMKnown
	out.LocalFree = uint64(local.VRAMFree)
	out.LocalTotal = uint64(local.VRAMTotal)

	remoteFree, remoteTotal, remoteNodes, fresh := b.cachedRemoteVRAM()
	out.RemoteFree, out.RemoteTotal, out.Nodes = remoteFree, remoteTotal, remoteNodes
	if fresh {
		out.RemoteSource = RemoteSourceCache
		out.Known = out.Known || out.Nodes > 0
	} else {
		out.RemoteSource = RemoteSourceNone
	}

	out.LayoutFree = out.LocalFree + out.RemoteFree
	out.LayoutTotal = out.LocalTotal + out.RemoteTotal
	return out
}
