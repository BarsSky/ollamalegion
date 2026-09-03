package logger

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"go.uber.org/zap"
)

// TestGet - проверка получения логгера
func TestGet(t *testing.T) {
	logger := Get()
	assert.NotNil(t, logger)
}

// TestInit - проверка инициализации
//
// Round 52.2 (2026-08-24) заменил `var log *zap.SugaredLogger` +
// `sync.Once` на `atomic.Pointer[zap.SugaredLogger]`. Этот тест
// был обновлён чтобы использовать `logPtr.Store/Load` вместо прямого
// присваивания. До Round 52.2-fix тест не компилировался (`undefined:
// log` и `undefined: once`) — это блокировало `go test ./...` 4+ месяца.
func TestInit(t *testing.T) {
	// Сбрасываем логгер для тестирования инициализации
	origLogger := logPtr.Load()
	logPtr.Store(nil)

	Init("info")

	assert.NotNil(t, logPtr.Load())
	assert.Equal(t, logPtr.Load(), Get())

	// Восстанавливаем
	logPtr.Store(origLogger)
}

// TestInitDebug - проверка инициализации в debug режиме
func TestInitDebug(t *testing.T) {
	origLogger := logPtr.Load()
	logPtr.Store(nil)

	Init("debug")

	assert.NotNil(t, logPtr.Load())
	assert.Equal(t, logPtr.Load(), Get())

	logPtr.Store(origLogger)
}

// TestInitError - проверка инициализации в error режиме
func TestInitError(t *testing.T) {
	origLogger := logPtr.Load()
	logPtr.Store(nil)

	Init("error")

	assert.NotNil(t, logPtr.Load())

	logPtr.Store(origLogger)
}

// TestSync - проверка синхронизации логгера
func TestSync(t *testing.T) {
	// Убедимся что логгер инициализирован
	if logPtr.Load() == nil {
		Init("info")
	}

	// Синхронизация не должна паниковать
	assert.NotPanics(t, func() {
		Sync()
	})
}

// TestZapLoggerInterface - проверка интерфейса zap логгера
func TestZapLoggerInterface(t *testing.T) {
	logger := Get()

	// Проверяем что логгер реализует zap.SugaredLogger
	var _ *zap.SugaredLogger = logger

	assert.NotNil(t, logger)
}

// TestInit_StoreConcurrent — R52.2 follow-up: atomic.Pointer.Store/Load
// lock-free. Smoke test что Init() может быть вызван параллельно из
// нескольких goroutine без data race.
func TestInit_StoreConcurrent(t *testing.T) {
	const goroutines = 8
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			Init("info")
		}()
	}
	wg.Wait()
	// Sanity: после всех Init() Get() возвращает не-nil.
	assert.NotNil(t, Get())
}
