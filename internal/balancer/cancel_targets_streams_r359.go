// cancel_targets_streams_r359.go — R359 (2026-10-31): адресаты отмены берутся ещё и
// из ЖИВОГО учёта стримов, а не только из кэша поллера.
//
// ЖИВОЙ ДЕФЕКТ (аудит abort, пункт A3). `cancelTargets` выбирал адресатов по кэшу
// поллера (`llamaMetrics.LoadedModels`, до 30 с устаревания) и требовал
// `Status == healthy`. Отсюда два сценария «Stop не сработал»:
//
//  1. ЛАГ КЭША. Модель только что поднялась на бэкенде B (или переехала с A на B), в
//     кэше она ещё числится на A. Отмена уходит на A, а на B — нет: генерация на B
//     продолжается, клиент видит «остановил, а оно пишет дальше».
//  2. ФИЛЬТР ЗДОРОВЬЯ. Если у бэкенда статус не healthy (например ollama_unavailable у
//     llama.cpp-бэкенда без Ollama-порта), он вообще не получал приказ — хотя стрим на
//     нём идёт, то есть он очевидно работает.
//
// ОБА СЛУЧАЯ РЕШАЕТ ОДИН ИСТОЧНИК ПРАВДЫ: балансер САМ ведёт учёт активных клиентских
// стримов по паре (backend, model) — active_streams_r271.go. Если стрим есть, бэкенд
// обслуживает эту модель прямо сейчас, независимо от кэша и статуса health.
//
// Поэтому адресаты = (кэш ∧ healthy) ∪ (есть активный стрим по этой модели). Первое
// сохраняет прежнее поведение (отмена по модели, которая загружена, но сейчас молчит),
// второе закрывает оба дефекта выше. Пустой model по-прежнему означает «всем».
package balancer

import "strings"

// backendsWithActiveStreamForR359 — бэкенды, на которых ПРЯМО СЕЙЧАС идёт клиентский
// стрим по этой модели (сравнение имён — мягкое, как у modelNameMatches: клиент может
// назвать модель с расширением/тегом).
//
// Порядок детерминированный (сортировка), чтобы ответ API не «прыгал» между вызовами.
func (p *Proxy) backendsWithActiveStreamForR359(model string) []string {
	if p == nil || model == "" {
		return nil
	}
	s := &p.streamsR271
	s.mu.Lock()
	keys := make([]string, 0, len(s.byModel))
	for key, n := range s.byModel {
		if n <= 0 {
			continue
		}
		keys = append(keys, key)
	}
	s.mu.Unlock()

	seen := make(map[string]struct{}, len(keys))
	out := make([]string, 0, len(keys))
	for _, key := range keys {
		backendID, streamedModel := splitStreamRefKeyR359(key)
		if backendID == "" || streamedModel == "" {
			continue
		}
		if !modelNameMatches(streamedModel, model) {
			continue
		}
		if _, ok := seen[backendID]; ok {
			continue
		}
		seen[backendID] = struct{}{}
		out = append(out, backendID)
	}
	// Детерминированный порядок: map обходится случайно, а ответ API читает человек.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// splitStreamRefKeyR359 — разобрать ключ учёта стрима ("backend|model").
//
// Ключ собирает streamRefKeyR271: backendID + "\x00" + model. Разделитель — NUL, он не
// может встретиться ни в имени бэкенда, ни в имени модели, поэтому режем по ПЕРВОМУ
// вхождению без эвристик.
func splitStreamRefKeyR359(key string) (backendID, model string) {
	i := strings.IndexByte(key, 0)
	if i < 0 {
		return "", ""
	}
	return key[:i], key[i+1:]
}
