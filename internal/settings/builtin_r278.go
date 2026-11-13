package settings

// builtin_r278.go — встроенные «горячие» настройки этапа 2 аудита.
//
// ПОЧЕМУ ТОЛЬКО ЭТИ. HotReload имеет смысл там, где значение читается на каждый
// запрос/стрим, а не участвует в конструировании сервера или транспорта. Здесь
// ровно те переменные, которые уже читаются геттером в момент обслуживания:
// лимит строки стрима, keepalive и пределы буферизации non-stream, буфер
// tool-вызова, ожидание перезагрузки, admission-ожидание и TTL проверки версии
// llama.cpp. Остальные (порты, таймауты http.Server, пути) требуют рестарта и
// в реестр сознательно НЕ попадают — иначе WebUI предлагал бы кнопку, которая
// ничего не меняет.
//
// ДЕФОЛТЫ И ЛОЯЛЬНОСТЬ К МУСОРУ. Default и NormalizeEnv повторяют то, что
// делали прежние геттеры: мусор и подозрительно маленькие значения из env
// трактуются как «взять дефолт» (иначе одна опечатка в .env ломала бы стенд).
// Значения из WebUI/API, наоборот, валидируются и при неверном значении
// возвращают ошибку — оператор должен видеть отказ, а не молчаливую подмену.

import (
	"fmt"
	"sync"
)

// Встроенные описания храним и в карте для случая «реестр не инициализирован»
// (HotInt без Init): одна и та же формула приоритета должна работать и там.
//
// Карта наполняется ЛЕНИВО (sync.Once), а не только из Init: HotInt обязан
// работать и без глобального реестра — иначе юнит-тесты и лёгкие сборки
// получили бы второе мнение о приоритете источников.
var (
	builtinsR278Map  = map[string]Setting{}
	builtinsR278Once sync.Once
)

func ensureBuiltinsMapR278() {
	builtinsR278Once.Do(func() {
		for _, s := range builtinListR278() {
			builtinsR278Map[s.Name] = s
		}
	})
}

func builtinsR278(name string) (Setting, bool) {
	ensureBuiltinsMapR278()
	s, ok := builtinsR278Map[name]
	return s, ok
}

func registerBuiltinsR278(r *Registry) {
	ensureBuiltinsMapR278()
	for _, s := range builtinListR278() {
		r.Register(s)
	}
}

func builtinListR278() []Setting {
	const (
		streamMaxLineDefault = 8 << 20   // 8 МиБ: кадр с аргументами tool-call
		toolBufferDefault    = 64 * 1024 // кросс-чанковый буфер tool-вызова
		minStreamLine        = 64 * 1024
		minToolBuffer        = 4096
	)
	return []Setting{
		{
			Name:        "LB_STREAM_MAX_LINE_BYTES",
			Type:        TypeBytes,
			Default:     streamMaxLineDefault,
			Group:       "streaming",
			HotReload:   true,
			Description: "Предел длины одной строки SSE/NDJSON. Слишком маленькое значение рвёт большие tool-call'ы; меньше 64 КиБ игнорируется.",
			Validate: func(v int64) error {
				if v < minStreamLine {
					return fmt.Errorf("too small: %d < %d", v, minStreamLine)
				}
				return nil
			},
			NormalizeEnv: func(v int64) int64 {
				if v < minStreamLine {
					return streamMaxLineDefault
				}
				return v
			},
		},
		{
			Name:        "LB_OPENAI_AUTO_STREAM_MAX_SEC",
			Type:        TypeDurationSec,
			Default:     300,
			Group:       "streaming",
			HotReload:   true,
			Description: "Сколько секунд максимум копить non-stream ответ (агентские клиенты рвут раньше). 0 отключает предел.",
			Validate: func(v int64) error {
				if v < 0 {
					return fmt.Errorf("must be >= 0 (0 disables the limit), got %d", v)
				}
				return nil
			},
		},
		{
			Name:        "LB_OPENAI_AUTO_STREAM_MAX_BYTES",
			Type:        TypeBytes,
			Default:     16 << 20,
			Group:       "streaming",
			HotReload:   true,
			Description: "Сколько байт максимум копить non-stream ответ. 0 отключает предел.",
			Validate: func(v int64) error {
				if v < 0 {
					return fmt.Errorf("must be >= 0 (0 disables the limit), got %d", v)
				}
				return nil
			},
		},
		{
			Name:        "LB_OPENAI_AUTO_STREAM_KEEPALIVE_SEC",
			Type:        TypeDurationSec,
			Default:     10,
			Group:       "streaming",
			HotReload:   true,
			Description: "Период пробельного keepalive, пока копится non-stream ответ (иначе клиент рвёт соединение, а тело пустое). 0 отключает.",
			Validate: func(v int64) error {
				if v < 0 {
					return fmt.Errorf("must be >= 0 (0 disables keepalive), got %d", v)
				}
				return nil
			},
			NormalizeEnv: func(v int64) int64 {
				if v <= 0 {
					return 0
				}
				if v < 2 {
					return 2 // меньше 2 с — бессмысленно: сам keepalive дороже пользы
				}
				return v
			},
		},
		{
			Name:        "LB_TOOL_CALL_BUFFER_BYTES",
			Type:        TypeBytes,
			Default:     toolBufferDefault,
			Group:       "streaming",
			HotReload:   true,
			Description: "Предел кросс-чанкового буфера текстового tool-вызова. Меньше 4 КиБ игнорируется: иначе вызов, пишущий файл, снова начнёт теряться.",
			Validate: func(v int64) error {
				if v < minToolBuffer {
					return fmt.Errorf("too small: %d < %d", v, minToolBuffer)
				}
				return nil
			},
			NormalizeEnv: func(v int64) int64 {
				if v < minToolBuffer {
					return toolBufferDefault
				}
				return v
			},
		},
		{
			Name:        "LB_RELOAD_DEFER_MAX_SEC",
			Type:        TypeDurationSec,
			Default:     120,
			Group:       "nctx",
			HotReload:   true,
			Description: "Сколько ждать завершения чужих стримов перед перезагрузкой модели. 0 = не ждать (операцию пропустить сразу).",
			Validate: func(v int64) error {
				if v < 0 {
					return fmt.Errorf("must be >= 0, got %d", v)
				}
				return nil
			},
			NormalizeEnv: func(v int64) int64 {
				if v < 0 {
					return 120
				}
				return v
			},
		},
		{
			Name:        "LB_ADMISSION_WAIT_SEC",
			Type:        TypeDurationSec,
			Default:     300,
			Group:       "queue",
			HotReload:   true,
			Description: "Сколько ждать свободный слот, прежде чем ответить 503. 0 = не ждать (прежнее поведение).",
			// Отрицательное значение исторически означало «ждать до отмены
			// клиентом, но не дольше 30 минут» — эта трактовка осталась в
			// геттере, чтобы не менять смысл существующих .env.
			Validate: func(int64) error { return nil },
		},
		{
			// R291 (2026-10-29): «кнопка думает две минуты».
			//
			// Живой факт: ручной reload при активном стриме отвечал ВЕРНО, но
			// только через LB_RELOAD_DEFER_MAX_SEC (120 с) — клиент воспринимал
			// это как зависание, хотя решение было известно сразу.
			//
			// Правило: если ждать пришлось бы дольше этого окна — отвечаем
			// «отложено» НЕМЕДЛЕННО; короткое ожидание оставляем, потому что
			// стрим может закончиться, и операция выполнится в том же запросе.
			// 0 = отвечать сразу всегда.
			Name:        "LB_RELOAD_DEFER_FAST_FAIL_SEC",
			Type:        TypeDurationSec,
			Default:     5,
			Group:       "nctx",
			HotReload:   true,
			Description: "Сколько максимум ждать чужие стримы в HTTP-ответе перед отказом «отложено». Больше — отвечаем сразу. 0 = отвечать сразу всегда.",
			Validate: func(v int64) error {
				if v < 0 {
					return fmt.Errorf("must be >= 0, got %d", v)
				}
				return nil
			},
			NormalizeEnv: func(v int64) int64 {
				if v < 0 {
					return 5
				}
				return v
			},
		},
		{
			Name:        "LB_LLAMA_UPSTREAM_TTL_SEC",
			Type:        TypeDurationSec,
			Default:     21600, // 6 часов
			Group:       "upstream",
			HotReload:   true,
			Description: "Как часто перепроверять последнюю стабильную версию llama.cpp. Меньше нуля/ноль игнорируются (остаётся дефолт).",
			Validate: func(v int64) error {
				if v <= 0 {
					return fmt.Errorf("must be > 0, got %d", v)
				}
				return nil
			},
			NormalizeEnv: func(v int64) int64 {
				if v <= 0 {
					return 21600
				}
				return v
			},
		},
		{
			// R297 (2026-10-29): «поток повис — кадров нет, соединение открыто».
			//
			// Живой факт: последний кадр в 15:01:20, дальше тишина навсегда —
			// ни `done:true`, ни закрытия. Общие idle-таймауты на стенде выключены
			// (LB_STREAMING_NEVER_TIMEOUT=1), поэтому зависание не ловил никто.
			//
			// Почему настройка, а не константа: (1) оператору нужно проверить это
			// живьём за секунды, а не ждать пять минут; (2) порог честно зависит
			// от железа — на CPU-модели пауза между кадрами бывает долгой.
			// Считается ТОЛЬКО после первого кадра: молчание до первого кадра —
			// это prefill, и обрывать его нельзя. 0 отключает сторож.
			Name:        "LB_STREAM_STALL_ABORT_SEC",
			Type:        TypeDurationSec,
			Default:     300,
			Group:       "streaming",
			HotReload:   true,
			Description: "Сколько секунд тишины от воркера посреди УЖЕ начатого стрима считать зависанием и закрывать поток кадром-причиной. 0 отключает сторож.",
			Validate: func(v int64) error {
				if v < 0 {
					return fmt.Errorf("must be >= 0 (0 disables the watchdog), got %d", v)
				}
				return nil
			},
			NormalizeEnv: func(v int64) int64 {
				if v < 0 {
					return 300
				}
				return v
			},
		},
		{
			// R298 (2026-10-29): сколько секунд тишины ДО первого кадра считать
			// зависанием.
			//
			// ЗАЧЕМ ОТДЕЛЬНО ОТ LB_STREAM_STALL_ABORT_SEC. Тот сторож меряет
			// тишину ПОСРЕДИ уже начатого стрима. Молчание до первого кадра —
			// это prefill: на CPU-модели он законно идёт минутами, и обрывать
			// его нельзя (именно поэтому оператор включил
			// LB_STREAMING_NEVER_TIMEOUT). По умолчанию здесь 0 = выключено.
			//
			// Нужна для двух случаев: (1) проверка живьём — поставить 10 и
			// увидеть, что поток закрывается кадром-причиной, а не висит;
			// (2) стенды, где первый байт обязан приходить быстро (GPU).
			Name:        "LB_STREAM_FIRST_FRAME_ABORT_SEC",
			Type:        TypeDurationSec,
			Default:     0,
			Group:       "streaming",
			HotReload:   true,
			Description: "Сколько секунд тишины ДО первого кадра считать зависанием (0 = не обрывать prefill). Для проверки живьём поставьте 10 вместе с LB_STREAM_STALL_ABORT_SEC=10.",
			Validate: func(v int64) error {
				if v < 0 {
					return fmt.Errorf("must be >= 0 (0 disables the watchdog), got %d", v)
				}
				return nil
			},
			NormalizeEnv: func(v int64) int64 {
				if v < 0 {
					return 0
				}
				return v
			},
		},
		{
			// R293 (2026-10-29): размещение модели на СВОБОДНОМ узле вместо
			// перезагрузки того узла, где она уже загружена.
			//
			// Живой факт со второго стенда: второй узел пуст, файл модели у него
			// есть, но при несовпадении окна балансер перезагружал модель на
			// первом узле — и рвал чужой стрим («stream ended by upstream before
			// completion»). Флаг горячий: 0 возвращает прежнее поведение без
			// рестарта (полезно, когда модель на диске второго узла медленная
			// или узел не предназначен для инференса).
			Name:        "LB_PLACE_OVER_RELOAD",
			Type:        TypeBool,
			Default:     1,
			Group:       "nctx",
			HotReload:   true,
			Description: "1 = при несовпадении окна сначала пробовать другой свободный узел с файлом модели; 0 = сразу перезагружать текущий узел (прежнее поведение).",
			Validate: func(v int64) error {
				if v != 0 && v != 1 {
					return fmt.Errorf("must be 0 or 1, got %d", v)
				}
				return nil
			},
			NormalizeEnv: func(v int64) int64 {
				if v != 0 {
					return 1
				}
				return 0
			},
		},
		{
			// R303 (2026-10-30): D4 двухузлового отчёта — клиент ждал 300 c и
			// получал 503 «being reloaded … (loaded=0)», хотя модель была
			// загружена на ДОНОРЕ: кэш метрик опрашивает узлы раз в ~30 c и
			// свежую копию не видит, поэтому выбор падал на пустой узел, а
			// preflight планировал загрузку с нуля. Флаг разрешает спросить
			// ЖИВОЙ кластер («где модель уже загружена») перед таким решением.
			//
			// Горячий: 0 возвращает прежнее поведение без рестарта (нужно, когда
			// опрос узлов дорогой — медленный канал). Стоимость при 1: один
			// GET /api/models на узел не чаще раза в 3 c и только на пути, где
			// иначе была бы ложная загрузка с нуля.
			Name:        "LB_LIVE_COPY_PROBE",
			Type:        TypeBool,
			Default:     1,
			Group:       "nctx",
			HotReload:   true,
			Description: "1 = перед загрузкой/ростом окна спрашивать живой кластер, где модель уже загружена (кэш метрик отстаёт на ~30 c); 0 = доверять только кэшу (прежнее поведение).",
			Validate: func(v int64) error {
				if v != 0 && v != 1 {
					return fmt.Errorf("must be 0 or 1, got %d", v)
				}
				return nil
			},
			NormalizeEnv: func(v int64) int64 {
				if v != 0 {
					return 1
				}
				return 0
			},
		},
		{
			// R304 (2026-10-30): донор держит ТОЛЬКО память мастера — доктрина
			// оператора «бэкенд стал донором, значит свои копии моделей он больше
			// не поднимает».
			//
			// ПОЧЕМУ ПО УМОЛЧАНИЮ ЗАПРЕТ (0). Смысл роли donor — отдать VRAM через
			// rpc-server. Своя копия на том же узле отнимает у мастера ровно те
			// гигабайты, ради которых донора назначили; живьём это выглядело как
			// «второй агент вырвал модель у первого» и как загрузка в занятую
			// память. Флаг оставлен как ИСКЛЮЧЕНИЕ для одного случая: оператор
			// назначил донором узел, который ещё обслуживает идущие стримы, и хочет
			// дожить до их конца, не обрывая работу. Горячий: действует без
			// рестарта, и «выключено» не оставляет дыр — тот же предикат
			// cannotHostModelR304 спрашивают все пути выбора.
			Name:        "LB_DONOR_ALLOW_OWN_COPIES",
			Type:        TypeBool,
			Default:     0,
			Group:       "nctx",
			HotReload:   true,
			Description: "0 = донор держит только память мастера и не поднимает своих копий моделей; 1 = разрешить донору свои копии (исключение на время идущих стримов).",
			Validate: func(v int64) error {
				if v != 0 && v != 1 {
					return fmt.Errorf("must be 0 or 1, got %d", v)
				}
				return nil
			},
			NormalizeEnv: func(v int64) int64 {
				if v != 0 {
					return 1
				}
				return 0
			},
		},
		{
			// R318 (2026-10-30): фильтр «узел скоро упрётся в критический ресурс»
			// в выборе бэкенда.
			//
			// ЖИВОЙ ДЕФЕКТ. В панели настроек был тумблер «Prediction filtering»:
			// сервер его ПРИНИМАЛ и молча не применял, а GET всегда отдавал true.
			// Оператор менял поле и не видел эффекта — ровно та жалоба, ради
			// которой затевался аудит настроек.
			//
			// ПОЧЕМУ ЭТО НЕ КОСМЕТИКА. Сам фильтр — жёсткий: узел, у которого
			// Prediction.SecondsToCritical < 300, ИСКЛЮЧАЕТСЯ из кандидатов на
			// пять минут (candidate.go, backend_selector.go). Если прогноз шумит
			// (или метрики пришли битыми), живой узел выпадает из выбора и клиент
			// получает «нет доступных бэкендов» при здоровом стенде. Тумблер,
			// который действительно выключает фильтр, — способ это проверить и
			// обойти, не читая код.
			//
			// Дефолт 1: поведение не меняется, пока оператор не выключит.
			Name:        "LB_PREDICTION_FILTER",
			Type:        TypeBool,
			Default:     1,
			Group:       "queue",
			HotReload:   true,
			Description: "1 = узел, которому прогноз даёт меньше 5 минут до критического ресурса, исключается из выбора бэкенда; 0 = фильтр по прогнозу выключен (узел остаётся кандидатом).",
			Validate: func(v int64) error {
				if v != 0 && v != 1 {
					return fmt.Errorf("must be 0 or 1, got %d", v)
				}
				return nil
			},
			NormalizeEnv: func(v int64) int64 {
				if v != 0 {
					return 1
				}
				return 0
			},
		},
		{
			// R292 (2026-10-30): второй клиент больше не вырывает модель у
			// первого. См. destructive_guard_r292.go.
			Name:        "LB_DESTRUCTIVE_OP_WAIT_SEC",
			Type:        TypeDurationSec,
			Default:     5,
			Group:       "nctx",
			HotReload:   true,
			Description: "Сколько ждать завершения чужих стримов, прежде чем выгружать/перезагружать модель по новому запросу. 0 = не ждать (сразу отложить и ответить 503).",
			Validate: func(v int64) error {
				if v < 0 {
					return fmt.Errorf("must be >= 0, got %d", v)
				}
				return nil
			},
			NormalizeEnv: func(v int64) int64 {
				if v < 0 {
					return 5
				}
				return v
			},
		},
		{
			Name:        "LB_TOOLCALL_PROSE_MAX_BYTES",
			Type:        TypeBytes,
			Default:     512,
			Group:       "streaming",
			HotReload:   true,
			Description: "Предел «подозрительного на tool-вызов» текста, после которого он отдаётся клиенту как обычный текст. Меньше 64 байт игнорируется.",
			Validate: func(v int64) error {
				if v < 64 {
					return fmt.Errorf("too small: %d < 64", v)
				}
				return nil
			},
			NormalizeEnv: func(v int64) int64 {
				if v < 64 {
					return 512
				}
				return v
			},
		},
		{
			Name:        "LB_TOOLCALL_PROSE_GRACE_MS",
			Type:        TypeInt,
			Default:     300,
			Group:       "streaming",
			HotReload:   true,
			Description: "Сколько миллисекунд держать текст, похожий на начало tool-вызова, прежде чем отдать его клиенту как обычный текст.",
			Validate: func(v int64) error {
				if v < 0 {
					return fmt.Errorf("must be >= 0, got %d", v)
				}
				return nil
			},
			NormalizeEnv: func(v int64) int64 {
				if v < 0 {
					return 300
				}
				return v
			},
		},
		// R388 (2026-11-02): поведенческие выключатели, которые раньше менялись
		// ТОЛЬКО через env и пересоздание контейнера. Теперь их видно и можно
		// менять в панели «Горячие настройки» (WebUI рисует реестр целиком:
		// значение, источник, применение без рестарта).
		{
			Name:        "LB_LAYOUT_HEAL",
			Type:        TypeBool,
			Default:     1,
			Group:       "layout",
			HotReload:   true,
			Description: "Автолечение вырожденной раскладки: перезапуск rpc-server донора + перезагрузка простаивающей модели (R383). 0 — выключить.",
			Validate:    bool01R388,
		},
		{
			Name:        "LB_NCTX_GROW_FOR_STREAM",
			Type:        TypeBool,
			Default:     1,
			Group:       "nctx",
			HotReload:   true,
			Description: "Запрос с большим окном поднимает модель и на стриме (R385): перезагрузка + 503/Retry-After. 0 — прежнее поведение.",
			Validate:    bool01R388,
		},
		// R493-12n (2026-11-13, этап 3 плана «два мозга»): ОДНО решение об окне.
		//
		// ЖИВОЙ ДЕФЕКТ. Правило окна жило в двух местах: guard R301 (стриминг и
		// preflight до очереди) и preflight-движок (остальное). R447 («окна мало ≠
		// окно надо растить») и R466 («завышенная оценка не отменяет рост») были
		// вписаны в одно из них — один и тот же JSON на /api/chat обслуживался, а
		// на /v1/chat/completions получал 503.
		//
		// Значения: 0 — историческое поведение (обе машины как были), 1 — единое
		// решение (decideWindowR493 на всех путях), 2 — «тень»: единое считается,
		// расхождения пишутся WARN-строкой `R493-12n shadow divergence`, а
		// поведение остаётся историческим. Тень — режим для стенда: снять
		// расхождения на реальном трафике, потом включать 1.
		{
			Name:        "LB_WINDOW_UNIFIED",
			Type:        TypeInt,
			Default:     0,
			Group:       "nctx",
			HotReload:   true,
			Description: "Одно решение об окне на всех путях: 0 — историческое поведение (два места), 1 — единое (decideWindowR493), 2 — «тень» (считать единое, писать расхождения, вести себя исторически).",
			Validate: func(v int64) error {
				if v < 0 || v > 2 {
					return fmt.Errorf("must be 0 (legacy), 1 (unified) or 2 (shadow), got %d", v)
				}
				return nil
			},
		},
		// R493-12p (2026-11-13, этап 4 «два мозга»): ОДИН вход в очередь.
		//
		// ЖИВОЙ ФАКТ. Ожиданий два: единая admission-очередь (generic-путь, бюджет
		// max(LB_ADMISSION_WAIT_SEC, LB_AUTO_LOAD_WAIT_SEC) — до 900 c) и очередь
		// ожидания ПОЯВЛЕНИЯ бэкенда (путь обработчиков, LB_BACKEND_WAIT_SEC —
		// 120 c). Один и тот же кластер давал разное время ожидания в зависимости
		// от поверхности, а ожидающие на /v1 не были видны в /queue вовсе.
		//
		// 0 — историческое поведение (по умолчанию), 1 — путь обработчиков входит
		// в единую очередь (один бюджет, одна справедливость, одна видимость).
		// Режима «тень» нет осознанно: ожидание занимает слот и меняет состояние,
		// «посчитать в тени» его нельзя.
		{
			Name:        "LB_UNIFIED_ADMISSION",
			Type:        TypeBool,
			Default:     0,
			Group:       "queue",
			HotReload:   true,
			Description: "Один вход в очередь: 0 — путь обработчиков ждёт появления бэкенда своей очередью (LB_BACKEND_WAIT_SEC), 1 — входит в единую admission-очередь (общий бюджет и видимость в /queue).",
			Validate:    bool01R388,
		},
		// R493-12q (2026-11-13, этап 5 «два мозга»): ОДИН ПУТЬ ИНФЕРЕНСА.
		//
		// ЖИВОЙ ФАКТ. Инструмент генерации изображений (R84/R86) объявлялся и
		// исполнялся только на handler-пути: /v1/chat/completions и
		// /ollama/api/chat. «Голый» /api/chat идёт generic-путём и инструмента не
		// объявлял вовсе — один кластер вёл себя по-разному в зависимости от URL у
		// клиента (Open WebUI в штатном режиме: «модель не видит инструменты»).
		//
		// 0 — историческое поведение (по умолчанию), 1 — generic-путь объявляет и
		// исполняет инструмент тем же кодом, что handler-путь (паритет R86).
		// R493-12p-fix (2026-11-13): LB_BACKEND_WAIT_SEC был «мёртвой» горячей
		// настройкой — код читал его через settings.HotInt, но в реестре его не
		// было, поэтому панель показывала пустоту и правка из WebUI не применялась
		// (работал только env). Нашёл страж hot-настроек (см.
		// hot_settings_steward_test.go) — он падает на любом таком расхождении.
		{
			Name:        "LB_BACKEND_WAIT_SEC",
			Type:        TypeDurationSec,
			Default:     120,
			Group:       "queue",
			HotReload:   true,
			Description: "Сколько ждать появления подходящего бэкенда на пути обработчиков (/v1/*), прежде чем ответить 503. 0 — не ждать (прежнее поведение).",
			Validate: func(v int64) error {
				if v < 0 {
					return fmt.Errorf("must be >= 0 (0 disables the wait), got %d", v)
				}
				return nil
			},
		},
		// R493-12r (2026-11-13, этап 6 «два мозга»): ОДИН СТОК СТРИМА.
		//
		// ЖИВОЙ ФАКТ. Клиентский вывод писали, считали и флашили в каждом цикле
		// по-своему (NDJSON, SSE OpenAI, hijack): забытый перевод строки склеивает
		// кадры, забытый флаш превращает поток в «замер», забытый счётчик делает
		// сводку streamed_content_bytes ложью.
		//
		// 0 — историческое поведение (по умолчанию): цикл пишет напрямую.
		// 1 — цикл пишет через streamSinkR493 (одно правило кадра, один счётчик,
		// одна точка флаша; hijack-соединение — ещё одна реализация стока).
		{
			Name:        "LB_STREAM_PIPE",
			Type:        TypeBool,
			Default:     0,
			Group:       "streaming",
			HotReload:   true,
			Description: "Один сток стрима: 0 — каждый цикл пишет клиенту напрямую (свой счётчик и флаш), 1 — общий сток кадра (правило кадра, счётчик Written(), одна точка флаша).",
			Validate:    bool01R388,
		},
		{
			Name:        "LB_ONE_INFERENCE",
			Type:        TypeBool,
			Default:     0,
			Group:       "surface",
			HotReload:   true,
			Description: "Один путь инференса: 0 — /api/chat (generic) не объявляет image-инструмент, 1 — объявляет и исполняет его тем же циклом, что /v1 и /ollama/api/chat (паритет R86).",
			Validate:    bool01R388,
		},
		// R494 (2026-11-12, стенд A10 ×2): потолок ВЫРАВНИВАНИЯ окна копий.
		//
		// ЖИВОЙ ФАКТ. Один файл Qwen3.8-27B был поднят на мастере с окном 198656,
		// а на A10-126 — с 131072. Сессия, привязанная к мастеру, не могла уехать
		// на вторую копию (R459: «не хуже, чем у меня»), а сама вторая копия
		// предлагала сессии окно меньше нужного. Оператор просил либо
		// подтверждаемое предложение «поднять окно», либо настройку балансера.
		//
		// Здесь — НАСТРОЙКА, а не действие: балансер по ней только считает, до
		// какого окна копию вообще имеет смысл выравнивать, и отдаёт это в
		// предложении (/api/v1/window/align). Сама перезагрузка остаётся за
		// оператором: 27B грузится минутами, и молчаливая автоперезагрузка
		// выбила бы чужую сессию. Дефолт 131072 — копии ниже этого порога не
		// предлагаются к выравниванию, чтобы предложение не выглядело как
		// «поднимите до 198k на 8-гигабайтном узле». Дефолт 262144 — физический
		// предел типовой 27B-модели на этом стенде: при меньшем дефолте живой
		// случай из архива (198656 против 131072) не давал бы предложения вовсе,
		// потому что цель обрезалась бы до уже существующего узкого окна.
		{
			Name:        "LB_WINDOW_ALIGN_MAX_N_CTX",
			Type:        TypeInt,
			Default:     262144,
			Group:       "nctx",
			HotReload:   true,
			Description: "Потолок n_ctx, до которого имеет смысл выравнивать окна копий одной модели (R494). 0 — выравнивание не предлагается вовсе.",
			Validate: func(v int64) error {
				if v < 0 {
					return fmt.Errorf("must be >= 0 (0 disables the alignment suggestion), got %d", v)
				}
				return nil
			},
		},
		{
			Name:        "LB_OPENAI_AUTO_STREAM",
			Type:        TypeBool,
			Default:     0,
			Group:       "streaming",
			HotReload:   true,
			Description: "Non-stream запрос /v1 выполняется внутренним стримом к воркеру, клиенту — один JSON. По умолчанию выключено.",
			Validate:    bool01R388,
		},
		{
			Name:        "LB_AUTO_CONTINUE_ON_TRUNCATION",
			Type:        TypeBool,
			Default:     0,
			Group:       "auto_continue",
			HotReload:   true,
			Description: "Авто-продолжение обрезанного ответа (опция). Агентский ход не продолжается никогда (R187).",
			Validate:    bool01R388,
		},
		{
			Name:    "LB_OLLAMA_NATIVE_PATH",
			Type:    TypeBool,
			Default: 1,
			Group:   "surface",
			// HotReload=false ОСОЗНАННО: переключение формы ответа на живой
			// сессии путает начатый разговор. Панель показывает значение и
			// источник; менять — через env и рестарт (Set ответит ErrNotHot).
			HotReload:   false,
			Description: "Нативная поверхность Ollama: ответ воркера как есть (1) или перевод OpenAI→Ollama (0). Только с перезапуском.",
			Validate:    bool01R388,
		},
	}
}

// bool01R388 — общий валидатор булевых выключателей (0/1).
func bool01R388(v int64) error {
	if v != 0 && v != 1 {
		return fmt.Errorf("expected 0 or 1, got %d", v)
	}
	return nil
}
