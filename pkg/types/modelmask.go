// Package types — сопоставление имени модели с маской (R92).
//
// До R92 матчер жил только в internal/balancer (placement.go) и был недоступен
// другим пакетам. Логическому бэкенду (internal/backendgroup) нужен ровно тот же
// матчер: группу описывают маской модели, и расхождение двух реализаций дало бы
// «группа есть, а политика её не видит». Поэтому канонический матчер переехал
// сюда, а balancer оставил тонкие обёртки ради читаемости вызовов.
package types

import "strings"

// MatchModelMask — сопоставление имени модели с маской.
//
// Поддерживаются `*` (любая последовательность) и `?` (один символ), регистр не
// важен. Сравниваем и полное имя, и имя без расширения `.gguf` (cgpt-подобные
// клиенты присылают и то, и другое).
func MatchModelMask(pattern, model string) bool {
	pat := strings.ToLower(strings.TrimSpace(pattern))
	if pat == "" {
		return false
	}
	candidates := []string{strings.ToLower(strings.TrimSpace(model))}
	if trimmed := strings.TrimSuffix(candidates[0], ".gguf"); trimmed != candidates[0] {
		candidates = append(candidates, trimmed)
	}
	for _, candidate := range candidates {
		if candidate == "" {
			continue
		}
		if WildcardMatch(pat, candidate) {
			return true
		}
	}
	return false
}

// WildcardMatch — простой матчер `*`/`?` (path.Match не подходит: он
// останавливается на `/`, а имена моделей содержат `hf.co/...`).
func WildcardMatch(pattern, s string) bool {
	pi, si := 0, 0
	star, mark := -1, 0
	for si < len(s) {
		if pi < len(pattern) && (pattern[pi] == '?' || pattern[pi] == s[si]) {
			pi++
			si++
			continue
		}
		if pi < len(pattern) && pattern[pi] == '*' {
			star = pi
			mark = si
			pi++
			continue
		}
		if star >= 0 {
			pi = star + 1
			mark++
			si = mark
			continue
		}
		return false
	}
	for pi < len(pattern) && pattern[pi] == '*' {
		pi++
	}
	return pi == len(pattern)
}
