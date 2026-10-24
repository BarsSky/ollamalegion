// link_bench_r129.go — R129 (2026-10-16, пункт 3 разбора стенда): СКОРОСТЬ обмена
// по сети между узлами.
//
// ЧЕГО НЕ ХВАТАЛО. Оператор просил «какая между ними скорость обмена по сети».
// Доступность узла (ProbeRPCServers) и задержка TCP — это не пропускная
// способность: узел может отвечать за 2 мс и при этом отдавать 3 МБ/с, и по такой
// сети раскладка слоёв будет тормозить.
//
// ПОЧЕМУ НЕЛЬЗЯ ИЗМЕРИТЬ НА ПОРТУ rpc-server. У него нет ни ручки здоровья, ни
// эхо-протокола: он говорит на своём бинарном протоколе, и «залить в него 8 МБ»
// не значит ничего измерить. Поэтому замер идёт по HTTP-поверхности САМОГО
// cppworker (ручка /api/v1/cppworker/link-bench, которая читает тело и отбрасывает
// его) — это тот же физический путь между машинами, по которому пойдёт трафик
// раскладки, и измерение честное: переданные байты / затраченное время.
//
// ЧЕСТНО О СМЫСЛЕ ЧИСЛА. Это пропускная способность TCP-потока между узлами
// (то, что ограничивает обмен активациями раскладки), а не «скорость llama.cpp
// RPC»: сам протокол мы не воспроизводим. В отчёте так и подписано.
package cppbackend

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// LinkSpeedResult — результат замера пропускной способности до узла.
type LinkSpeedResult struct {
	Endpoint string `json:"endpoint"`
	// MB — сколько мегабайт отправлено, DurationMS — сколько это заняло.
	MB         int   `json:"mb"`
	DurationMS int64 `json:"durationMs"`
	// MBps — мегабайт в секунду (округление до 0.1).
	MBps float64 `json:"mbps"`
	// Error — причина, если замер не удался (не «0 МБ/с»: ноль означал бы
	// измеренную нулевую скорость, а это ложь).
	Error string `json:"error,omitempty"`
}

// linkBenchPayload — буфер для отправки. Держим один на процесс: аллокация
// десятков мегабайт на каждый замер ради одних и тех же байт бессмысленна.
var linkBenchPayload = make([]byte, 1<<20)

func init() {
	// Заполняем не нулями: сжимающие/оптимизирующие звенья на пути не должны
	// «ускорить» передачу нулевого потока.
	for i := range linkBenchPayload {
		linkBenchPayload[i] = byte(i*7 + 13)
	}
}

// MeasureLinkSpeed — отправить megabytes МБ на endpoint (HTTP POST) и посчитать
// пропускную способность.
//
// endpoint — ПОЛНЫЙ URL приёмника, например
// "http://192.0.2.10:18092/api/v1/cppworker/link-bench".
//
// Возвращает результат ВСЕГДА: при ошибке заполнено Error, а MBps остаётся нулём
// и вызывающий обязан показать причину, а не «0 МБ/с».
func MeasureLinkSpeed(ctx context.Context, endpoint string, megabytes int, timeout time.Duration) LinkSpeedResult {
	res := LinkSpeedResult{Endpoint: endpoint, MB: megabytes}
	if megabytes <= 0 {
		res.Error = "не задан объём замера (мегабайты)"
		return res
	}
	if strings.TrimSpace(endpoint) == "" {
		res.Error = "не задан адрес приёмника"
		return res
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	// Таймаут замера — на весь обмен, поэтому контекст с общим сроком.
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Тело собираем из мегабайтных блоков: ContentLength обязан совпадать с тем,
	// сколько байт реально отдаст reader, иначе Go рвёт соединение
	// («ContentLength=4194304 with Body length 1048576») — первая версия именно
	// так и падала.
	readers := make([]io.Reader, 0, megabytes)
	for i := 0; i < megabytes; i++ {
		readers = append(readers, bytes.NewReader(linkBenchPayload))
	}
	body := io.MultiReader(readers...)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, body)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	// ContentLength обязателен: без него Go отправит chunked, и приёмник не сможет
	// отличить «медленно» от «не знаем сколько ждать».
	req.ContentLength = int64(megabytes) << 20

	client := &http.Client{Timeout: timeout}
	started := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	defer resp.Body.Close()
	// Ответ маленький (числа), но дочитываем: иначе соединение не переиспользуется.
	_, _ = io.Copy(io.Discard, resp.Body)
	elapsed := time.Since(started)

	if resp.StatusCode >= 400 {
		res.Error = fmt.Sprintf("приёмник ответил HTTP %d", resp.StatusCode)
		return res
	}
	res.DurationMS = elapsed.Milliseconds()
	if elapsed > 0 {
		res.MBps = float64(megabytes) / elapsed.Seconds()
		// Округляем до 0.1: больше знаков в UI смысла не имеют.
		res.MBps = float64(int(res.MBps*10+0.5)) / 10
	}
	return res
}
