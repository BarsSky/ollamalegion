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
	}
}
