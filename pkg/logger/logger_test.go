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
func TestInit(t *testing.T) {
	// Сбрасываем логгер для тестирования инициализации
	origLogger := log
	log = nil
	once = sync.Once{}

	Init("info")

	assert.NotNil(t, log)
	assert.Equal(t, log, Get())

	// Восстанавливаем
	log = origLogger
}

// TestInitDebug - проверка инициализации в debug режиме
func TestInitDebug(t *testing.T) {
	origLogger := log
	log = nil
	once = sync.Once{}

	Init("debug")

	assert.NotNil(t, log)
	assert.Equal(t, log, Get())

	log = origLogger
}

// TestInitError - проверка инициализации в error режиме
func TestInitError(t *testing.T) {
	origLogger := log
	log = nil
	once = sync.Once{}

	Init("error")

	assert.NotNil(t, log)

	log = origLogger
}

// TestSync - проверка синхронизации логгера
func TestSync(t *testing.T) {
	// Убедимся что логгер инициализирован
	if log == nil {
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