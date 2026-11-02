// node_labels_r393_test.go — R393 (2026-11-02): NODE_LABELS агента доезжает до
// записи бэкенда (до правки поле nodeLabels игнорировалось).
package api

import (
	"reflect"
	"testing"
)

func TestR393_MergeNodeLabels(t *testing.T) {
	cases := []struct {
		name   string
		labels []string
		node   string
		want   []string
	}{
		{
			name:   "операторские метки добавляются к меткам агента",
			labels: []string{"linux", "amd64", "gpu"},
			node:   "gpu,ollama",
			want:   []string{"linux", "amd64", "gpu", "ollama"},
		},
		{
			name:   "повтор без учёта регистра не дублируется",
			labels: []string{"GPU"},
			node:   "gpu,ollama",
			want:   []string{"GPU", "ollama"},
		},
		{
			name:   "пустые элементы и пробелы отбрасываются",
			labels: nil,
			node:   " gpu , , ollama ",
			want:   []string{"gpu", "ollama"},
		},
		{
			name:   "нет nodeLabels — только метки агента",
			labels: []string{"linux"},
			node:   "",
			want:   []string{"linux"},
		},
		{
			name:   "нет ничего",
			labels: nil,
			node:   "",
			want:   []string{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := mergeNodeLabelsR393(tc.labels, tc.node)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("mergeNodeLabelsR393(%v, %q) = %v, ожидалось %v",
					tc.labels, tc.node, got, tc.want)
			}
			// Исходный массив не должен меняться.
			if tc.labels != nil && len(tc.labels) > 0 {
				if tc.labels[0] == "ollama" || tc.labels[len(tc.labels)-1] == "ollama" {
					t.Fatalf("входной массив мутирован: %v", tc.labels)
				}
			}
		})
	}
}
