package main

// R101 (S1 плана 2026-10-07): тесты разбора CPPWORKER_RPC_SERVERS.
//
// Проверяем то, что оператор вводит руками: список узлов либо принимается
// целиком, либо отвергается с указанием, что именно не так. Частично принятый
// список опаснее отказа — мастер молча считал бы на части узлов.

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseRPCServersR101(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		want    []string
		wantErr string
	}{
		{
			name: "два узла",
			raw:  "192.0.2.11:50052,192.0.2.12:50052",
			want: []string{"192.0.2.11:50052", "192.0.2.12:50052"},
		},
		{
			name: "пробелы вокруг элементов",
			raw:  " 192.0.2.11:50052 , host.example:60000 ",
			want: []string{"192.0.2.11:50052", "host.example:60000"},
		},
		{
			name: "один узел",
			raw:  "10.0.0.5:50052",
			want: []string{"10.0.0.5:50052"},
		},
		{name: "пусто", raw: "   ", wantErr: "пустой список"},
		{name: "нет порта", raw: "192.0.2.11", wantErr: "элемент 1"},
		{name: "пустой host", raw: ":50052", wantErr: "пустой host"},
		{name: "порт не число", raw: "host:abc", wantErr: "порт не число"},
		{name: "порт вне диапазона", raw: "host:70000", wantErr: "вне диапазона"},
		{name: "нулевой порт", raw: "host:0", wantErr: "вне диапазона"},
		{name: "лишняя запятая", raw: "host:50052,,host2:50052", wantErr: "элемент 2 пуст"},
		{name: "дубликат узла", raw: "host:50052,host:50052", wantErr: "дважды"},
		{
			name:    "ошибка называет позицию во втором элементе",
			raw:     "good:50052,bad",
			wantErr: "элемент 2",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseRPCServersR101(tc.raw)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("ожидалась ошибка с %q, получен список %v", tc.wantErr, got)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("ошибка %q не содержит %q", err.Error(), tc.wantErr)
				}
				if got != nil {
					t.Errorf("при ошибке список должен быть пуст (ничего не принято частично): %v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("неожиданная ошибка: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("получено %v, ожидалось %v", got, tc.want)
			}
		})
	}
}

func TestParseRPCServersR101_TooManyNodes(t *testing.T) {
	parts := make([]string, 0, maxRPCServersR101+1)
	for i := 0; i <= maxRPCServersR101; i++ {
		parts = append(parts, "host"+string(rune('a'+i))+":50052")
	}
	_, err := parseRPCServersR101(strings.Join(parts, ","))
	if err == nil {
		t.Fatal("список из лишних узлов должен отвергаться (защита от опечатки в compose)")
	}
	if !strings.Contains(err.Error(), "максимум") {
		t.Errorf("ошибка должна называть предел: %v", err)
	}
}
