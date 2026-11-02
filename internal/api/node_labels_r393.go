// node_labels_r393.go — R393 (2026-11-02): метки узла от агента доезжают до
// записи бэкенда.
//
// ЖИВОЙ ДЕФЕКТ. Агент присылает метки двумя полями: `labels` (массив
// ос/архитектура/платформа: linux, amd64, gpu) и `nodeLabels` (операторские,
// строкой "gpu,ollama"). Второе балансер не разбирал вовсе — NODE_LABELS из
// compose не влиял ни на что, и в WebUI у Ollama-агента не было метки `ollama`,
// хотя оператор её задал. То есть параметр существовал, но молча ничего не делал.
package api

import "strings"

// mergeNodeLabelsR393 — объединить массив меток агента с операторской строкой.
//
// Правила: пустые элементы и повторы (без учёта регистра) отбрасываются;
// порядок — сначала метки агента, затем операторские; исходный массив не
// мутируется (он пришёл из JSON-запроса и может использоваться ещё раз).
func mergeNodeLabelsR393(labels []string, nodeLabels string) []string {
	out := make([]string, 0, len(labels)+4)
	seen := make(map[string]struct{}, len(labels)+4)
	add := func(v string) {
		v = strings.TrimSpace(v)
		if v == "" {
			return
		}
		key := strings.ToLower(v)
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		out = append(out, v)
	}
	for _, l := range labels {
		add(l)
	}
	for _, l := range strings.Split(nodeLabels, ",") {
		add(l)
	}
	return out
}
