// capture_buffer_r489.go — R489.1 (2026-11-11): буфер перехвата логов, который
// безопасен и на запись, и на чтение.
//
// ЧТО СЛУЧИЛОСЬ. Первая версия R489 обернула writer в мьютекс (записи
// сериализуются), но тест читает `buf.String()` напрямую, а параллельные
// горутины в это время пишут через перехват. Гонка переехала с «писатель против
// писателя» на «писатель против читателя» — CI (self-hosted, -race) снова
// покраснел: Write в bytes.Buffer.grow против String в bytes.Buffer.String.
//
// КОРРЕКТНОЕ РЕШЕНИЕ. Владелец блокировки — сам буфер: и `Write`, и `String`,
// и `Len`/`Reset` берут один мьютекс. `CaptureTo` узнаёт такую обёртку и НЕ
// оборачивает её второй раз.
package logger

import (
	"bytes"
	"sync"
)

// CaptureBuffer — потокобезопасный буфер для перехвата логов в тестах.
type CaptureBuffer struct {
	buf bytes.Buffer
	mu  sync.Mutex
}

// NewCaptureBuffer — пустой буфер перехвата.
func NewCaptureBuffer() *CaptureBuffer {
	return &CaptureBuffer{}
}

// Write — реализация io.Writer (его вызывает zapcore).
func (c *CaptureBuffer) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.Write(p)
}

// Sync — требование zapcore.WriteSyncer; синхронизировать нечего.
func (c *CaptureBuffer) Sync() error { return nil }

// String — снимок накопленного текста под тем же мьютексом, что и запись.
func (c *CaptureBuffer) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.String()
}

// Len — длина накопленного текста.
func (c *CaptureBuffer) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.Len()
}

// Reset — очистить буфер.
func (c *CaptureBuffer) Reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.buf.Reset()
}
