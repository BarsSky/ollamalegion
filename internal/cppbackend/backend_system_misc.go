package cppbackend

import (
	"ollama-loadbalancer/internal/i18n"
	"ollama-loadbalancer/internal/memfit"
	"ollama-loadbalancer/pkg/logger"
	"time"
)

// GetSystemRAMGB — экспортированная обёртка для использования из других пакетов
func GetSystemRAMGB() uint64 {
	return getSystemRAMGB()
}

// SetIdleUnloadTimeout — R119 (2026-10-15): применить значение idleUnloadMinutes
// к ЖИВОМУ менеджеру автовыгрузки.
//
// ЗАЧЕМ. Оператор задаёт idleUnloadMinutes из WebUI
// (PUT /api/v1/cppworker/config/update), и до этой правки менялся только
// currentConfig: таймаут «зашивался» в менеджер в момент его создания, поэтому UI
// показывал новые минуты, а выгрузка продолжала происходить по старому значению.
// Живая проверка на стенде: PUT idleUnloadMinutes=3 → конфиг отдаёт 3, а живой
// менеджер продолжает считать по 10 минутам (и интервал проверки — по 5 минутам).
//
// Поведение:
//   - d > 0: менеджер создаётся (если его не было — например, значение было 0 и
//     автовыгрузка вообще не запускалась), получает новый таймаут и запускается;
//   - d <= 0: менеджер останавливается и снимается (0 = выключено).
//
// Возвращает действующий таймаут после вызова (0 = выключено): вызывающий код
// отдаёт его в ответе, чтобы UI показывал ФАКТ, а не только отправленное значение.
func (b *Backend) SetIdleUnloadTimeout(d time.Duration) time.Duration {
	if b == nil {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	if d <= 0 {
		if b.idleUnloader != nil {
			b.idleUnloader.Stop()
			b.idleUnloader = nil
			logger.Get().Infow(i18n.T("cppbackend.backend.idle_unload_disabled_runtime"))
		}
		return 0
	}

	if b.idleUnloader == nil {
		b.idleUnloader = NewIdleUnloadManager(b, d)
		b.idleUnloader.Start()
		logger.Get().Infow(i18n.T("cppbackend.backend.idle_unload_enabled_runtime"),
			"timeout", d.String())
		return d
	}

	b.idleUnloader.SetIdleTimeout(d)
	b.idleUnloader.Start() // идемпотентно: уже запущенный цикл не дублируется
	logger.Get().Infow(i18n.T("cppbackend.backend.idle_unload_timeout_changed"),
		"timeout", d.String(), "check_interval", b.idleUnloader.CheckInterval().String())
	return b.idleUnloader.IdleTimeout()
}

// IdleUnloadStatus — действующий таймаут автовыгрузки и признак «цикл запущен».
// Нужен диагностике и WebUI: currentConfig может расходиться с фактом, а
// оператору нужно показывать факт.
func (b *Backend) IdleUnloadStatus() (time.Duration, bool) {
	if b == nil {
		return 0, false
	}
	b.mu.Lock()
	u := b.idleUnloader
	b.mu.Unlock()
	if u == nil {
		return 0, false
	}
	return u.IdleTimeout(), u.Running()
}

// restoredWeightsBytesR306 — сколько ВЕСОВ модели уже занято в VRAM.
//
// Возвращаем ТОЛЬКО веса (GGUF), а не весь VRAMUsage: KV-кэш текущего окна при
// перезагрузке освободится, но в расчёте потолка он и не вычитается — Ceilings
// считает окно с нуля. Смешивать эти две вещи значило бы завысить потолок на
// размер текущего KV.
//
// Доля — по числу слоёв на GPU: при частичной выгрузке в RAM ушли не все веса, и
// вернуть в бюджет полный размер файла значило бы пообещать окно, которого не
// будет. Если на GPU не ушло ни одного слоя — возвращать нечего.
func restoredWeightsBytesR306(spec memfit.ModelSpec, loaded *ModelInfo) memfit.Bytes {
	if loaded == nil || loaded.State != StateLoaded || !spec.SizeBytes.Positive() {
		return 0
	}
	layers := loaded.NLayers
	gpuLayers := loaded.GPULayers
	if layers > 0 && gpuLayers > 0 && gpuLayers < layers {
		return spec.SizeBytes.Scale(int64(gpuLayers), int64(layers))
	}
	if gpuLayers == 0 && layers > 0 {
		return 0
	}
	return spec.SizeBytes
}
