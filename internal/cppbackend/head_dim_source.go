// head_dim_source.go — R705 (2026-11-24): откуда взята ширина головы K/V.
//
// ПРЕДМЕТ. До R705 ширина головы бралась из формулы n_embd/n_heads на ВСЕХ
// путях, включая загруженную модель. В GGUF есть точные
// <arch>.attention.key_length/value_length, и llama.cpp читает именно их
// (c/llama.cpp/src/llama-model.cpp:1659-1663 → llama_hparams.n_embd_head_k_full),
// поэтому для Qwen3-4B публиковалось 80 вместо 128, для gemma-4-E4B — 320
// вместо 512, для Inkling-2-Mini — 96 вместо 128. KV/токен занижался (Qwen3-4B —
// на 60 %), «сколько окна влезет» завышалось, и решения балансера (R659/AutoTune)
// принимались по неверной геометрии.
//
// ЧТО ЗДЕСЬ. Одно место, где решается, откуда число и как это назвать наружу:
//
//	bridge.HeadDimSourceGGUF     — ширина из модели (<arch>.attention.key_length
//	                               через llama_hparams или GGUF-заголовок);
//	bridge.HeadDimSourceComputed — прежняя формула n_embd/n_heads (оценка);
//	""                           — числа нет вовсе (неизвестно).
//
// Имя источника публикуется как head_dim_source в /api/models и /api/show и
// уезжает в балансер, чтобы «оценка» больше не выглядела «фактом».
package cppbackend

import "ollama-loadbalancer/c/bridge"

// resolveHeadDimSourceR705 приводит сообщённый мостом источник к явному значению.
//
// Правило обратной совместимости: старый мост не сообщает источник (""), но
// присылает ненулевое HeadDimK — это ровно прежняя формула n_embd/n_heads,
// поэтому такой случай помечается computed, а не gguf. Нулевая ширина означает
// «числа нет»: источник не выдаётся ни за факт, ни за оценку.
func resolveHeadDimSourceR705(reported string, headDim int) string {
	if headDim <= 0 {
		return ""
	}
	switch reported {
	case bridge.HeadDimSourceGGUF, bridge.HeadDimSourceComputed:
		return reported
	}
	return bridge.HeadDimSourceComputed
}

// headDimSourceForKeyLengthR705 — источник для пути «читаем GGUF-заголовок»:
// ключ <arch>.attention.key_length есть → gguf, иначе ширина считается формулой.
func headDimSourceForKeyLengthR705(keyLength, headDim int) string {
	if headDim <= 0 {
		return ""
	}
	if keyLength > 0 {
		return bridge.HeadDimSourceGGUF
	}
	return bridge.HeadDimSourceComputed
}

// headDimVForR705 — ширина головы V: <arch>.attention.value_length, иначе равна K
// (как в llama.cpp: value_length опционален, по умолчанию n_embd/n_head).
func headDimVForR705(valueLength, headDimK int) int {
	if valueLength > 0 {
		return valueLength
	}
	return headDimK
}
