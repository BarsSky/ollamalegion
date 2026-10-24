// image_tool_response_r120.go — R120 (2026-10-15): чтение ответа воркера без
// ложного «invalid JSON».
//
// ЖИВОЙ ДЕФЕКТ (найден на живой проверке пункта 2 плана). Инструмент
// generate_image просит воркер в url-режиме, но воркер до R120 присылал и
// ссылку, и полный base64 картинки. Тело обрезалось по imageToolResponseLimitBytes
// НА СЕРЕДИНЕ JSON, json.Unmarshal падал, и пользователь терял УЖЕ
// СГЕНЕРИРОВАННУЮ картинку с диагнозом «image backend returned invalid JSON:
// unexpected end of JSON input» — то есть виноватым выглядел воркер, хотя тело
// обрезал сам балансер. Живой лог:
//
//	image tool: генерация не удалась
//	error="image backend returned invalid JSON: unexpected end of JSON input"
//
// Здесь два независимых улучшения:
//  1. предел чтения поднят до размера, в который влезает реальная картинка в
//     base64 (1024x1024 PNG ≈ 1.5–4 MB, 2048x2048 ≈ до ~16 MB), — чтобы крупный,
//     но ВАЛИДНЫЙ ответ не стоил пользователю генерации;
//  2. обрезка распознаётся и называется своим именем: код + числа + подсказка
//     доезжают до модели так же, как отказ VRAM-гейта (image_tool_gate_r119.go).
package balancer

import (
	"fmt"
	"io"
)

// imageToolErrResponseTooLarge — машинный код «ответ воркера не влез в предел
// чтения». Отдельный код, а не общий image_backend_error: причина и лечение
// другие (размер/режим ответа, а не отказ движка).
const imageToolErrResponseTooLarge = "backend_response_too_large"

// imageToolResponseLimitBytes — предел чтения ответа воркера.
//
// R120: было 1 MB. В url-режиме ответ — метаданные и ссылка (килобайты), и 1 MB
// хватало; но воркер мог вернуть картинку и в base64, а такой ответ для
// 768x512 — уже ~766 KB, для 1024x1024 — ~4 MB, и он гарантированно обрезался.
// 32 MB покрывают реальные размеры (до 2048x2048 включительно) и при этом
// остаются жёсткой границей: битый сервис на порту не выльет в память гигабайты.
//
// R120 (продолжение): переменная, а не константа, — тесты обязаны проверять
// ОБНАРУЖЕНИЕ обрезки, не выделяя под это десятки мегабайт.
var imageToolResponseLimitBytes int64 = 32 << 20

// readImageToolResponse — прочитать тело ответа воркера и ЧЕСТНО сказать,
// обрезано ли оно пределом чтения.
//
// Читаем limit+1 байт: получить ровно limit+1 — единственный надёжный признак
// того, что тело длиннее предела (по одному len==limit этого не отличить от
// ответа ровно в предел).
func readImageToolResponse(body io.Reader) (raw []byte, truncated bool, err error) {
	limit := imageToolResponseLimitBytes
	raw, err = io.ReadAll(io.LimitReader(body, limit+1))
	if err != nil {
		return nil, false, err
	}
	if int64(len(raw)) > limit {
		return raw[:limit], true, nil
	}
	return raw, false, nil
}

// imageToolResponseTooLargeError — ошибка обрезки ответа воркера.
//
// Формулировка отвечает на три вопроса оператора/модели: что случилось (ответ
// больше предела и обрезан), почему так вышло (воркер вернул пиксели вместо
// ссылки) и что делать (просить url-режим).
func imageToolResponseTooLargeError(limit int64) *imageToolError {
	return &imageToolError{
		Code: imageToolErrResponseTooLarge,
		Message: fmt.Sprintf("ответ воркера больше предела чтения %d байт и был "+
			"обрезан на середине JSON: вероятно, воркер вернул картинку в base64, "+
			"а не ссылкой", limit),
		Hint: "запросите у воркера url-режим (responseFormat:\"url\") — тогда в ответе " +
			"будет ссылка на сохранённый PNG, а не пиксели; если воркер старый и " +
			"url-режим игнорирует, уменьшите размер картинки (width/height)",
	}
}
