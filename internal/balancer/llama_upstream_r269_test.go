package balancer

// llama_upstream_r269_test.go — R269 (2026-10-29): отслеживание актуальной
// стабильной версии llama.cpp.
//
// Что фиксируем:
//   - сравнение версий НЕ угадывает: «устарело» выдаётся только когда есть чем
//     сравнивать (тег bNNNN / дата / тот же коммит), иначе unknown;
//   - оффлайн не ломает ничего: ошибка сети оставляет прежнее «последнее
//     известное» и не превращается в панику или спам;
//   - кэш переживает рестарт (значение читается из файла, без сети);
//   - агрегат по кластеру правильно помечает отстающие узлы.

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ollama-loadbalancer/pkg/types"
)

// TestR269_CompareLlamaBuilds — таблица решений сравнения.
func TestR269_CompareLlamaBuilds(t *testing.T) {
	cases := []struct {
		name    string
		current string
		latest  string
		want    string
	}{
		{"same tag", "b5000", "b5000", llamaRelationUpToDate},
		{"older tag", "b4321", "b5000", llamaRelationBehind},
		{"newer tag", "b5100", "b5000", llamaRelationAhead},
		{"tag inside build string", "llama.cpp b4321", "b4322", llamaRelationBehind},
		{"same commit", "abcdef1234", "abcdef1234", llamaRelationUpToDate},
		{"different commits without versions", "abcdef1234", "1234567abc", llamaRelationUnknown},
		{"date older", "2026-09-01", "2026-10-01", llamaRelationBehind},
		{"date newer", "2026-10-02", "2026-10-01", llamaRelationAhead},
		{"empty current", "", "b5000", llamaRelationUnknown},
		{"empty latest", "b5000", "", llamaRelationUnknown},
		{"garbage both sides", "сборка от оператора", "release", llamaRelationUnknown},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, reason := CompareLlamaBuilds(c.current, c.latest)
			if got != c.want {
				t.Fatalf("CompareLlamaBuilds(%q,%q) = %q (%s), want %q",
					c.current, c.latest, got, reason, c.want)
			}
			if reason == "" {
				t.Errorf("причина сравнения пустая — оператор не поймёт вердикт")
			}
		})
	}
}

// TestR269_ParseLlamaBuild — терпимость к форматам строки сборки.
func TestR269_ParseLlamaBuild(t *testing.T) {
	cases := []struct {
		raw    string
		tag    string
		commit string
		date   string
	}{
		{"b4321", "b4321", "", ""},
		{"b4321-abcdef1", "b4321", "abcdef1", ""},
		{"build 2026-09-01 (b4321)", "b4321", "", "2026-09-01"},
		{"abcdef1234", "", "abcdef1234", ""},
		{"", "", "", ""},
	}
	for _, c := range cases {
		p := parseLlamaBuild(c.raw)
		if p.Tag != c.tag || p.Commit != c.commit {
			t.Errorf("parseLlamaBuild(%q) = tag %q commit %q, want tag %q commit %q",
				c.raw, p.Tag, p.Commit, c.tag, c.commit)
		}
		gotDate := ""
		if !p.Date.IsZero() {
			gotDate = p.Date.Format("2006-01-02")
		}
		if gotDate != c.date {
			t.Errorf("parseLlamaBuild(%q) date = %q, want %q", c.raw, gotDate, c.date)
		}
	}
}

// TestR269_OfflineKeepsPreviousValue — ошибка сети не обнуляет знание.
func TestR269_OfflineKeepsPreviousValue(t *testing.T) {
	// Сервер, который всегда отвечает 500 — имитация «GitHub недоступен».
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	oldBase := llamaUpstreamAPIBase
	llamaUpstreamAPIBase = srv.URL
	defer func() { llamaUpstreamAPIBase = oldBase }()

	dir := t.TempDir()
	tr := &llamaUpstreamTracker{path: filepath.Join(dir, llamaUpstreamFileName), stopCh: make(chan struct{})}
	// Сначала «успешное» знание.
	tr.recordSuccess(llamaUpstreamConfig{Repo: defaultLlamaUpstreamRepo}, "b5000", "", time.Now().UTC())

	p := &Proxy{llamaUpstream: tr}
	// Затем неудачный опрос.
	t.Setenv("LB_LLAMA_UPSTREAM_CHECK_ENABLED", "1")
	t.Setenv("LB_LLAMA_UPSTREAM_REPO", defaultLlamaUpstreamRepo)
	tr.maybeRefresh(p, true)

	latest, _, errText, ok := p.llamaUpstreamLatest()
	if !ok || latest == "" {
		t.Fatalf("прежнее значение потеряно после ошибки сети (ok=%v latest=%q)", ok, latest)
	}
	if errText == "" {
		t.Errorf("ошибка сети не записана в состояние — диагностика потеряна")
	}
	st := p.LlamaBuildStatusFor("нет-такого")
	if st.Relation != llamaRelationUnknown {
		t.Errorf("нет бэкенда, relation = %q, want unknown", st.Relation)
	}
}

// TestR269_SuccessAndCacheSurvivesRestart — значение пишется на диск и читается
// новым трекером (эмуляция рестарта балансера).
func TestR269_SuccessAndCacheSurvivesRestart(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"tag_name":"b5000","published_at":"2026-10-20T10:00:00Z","target_commitish":"master"}`))
	}))
	defer srv.Close()

	oldBase := llamaUpstreamAPIBase
	llamaUpstreamAPIBase = srv.URL
	defer func() { llamaUpstreamAPIBase = oldBase }()

	dir := t.TempDir()
	path := filepath.Join(dir, llamaUpstreamFileName)
	cfg := llamaUpstreamConfig{Enabled: true, Repo: defaultLlamaUpstreamRepo, TTLSec: 3600, TimeoutSec: 5}

	tr := &llamaUpstreamTracker{path: path, stopCh: make(chan struct{})}
	p := &Proxy{llamaUpstream: tr}
	tr.maybeRefresh(p, true)

	latest, _, _, ok := p.llamaUpstreamLatest()
	if !ok || latest != "b5000" {
		t.Fatalf("после опроса latest = %q (ok=%v), want b5000", latest, ok)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("кэш не записан на диск: %v", err)
	}

	// «Рестарт»: новый трекер читает только файл, без сети.
	tr2 := &llamaUpstreamTracker{path: path, stopCh: make(chan struct{})}
	tr2.loadFromDisk()
	p2 := &Proxy{llamaUpstream: tr2}
	latest2, _, _, ok2 := p2.llamaUpstreamLatest()
	if !ok2 || latest2 != "b5000" {
		t.Fatalf("после рестарта latest = %q (ok=%v), want b5000 (кэш не пережил рестарт)",
			latest2, ok2)
	}

	// Сверка с версией узла: узел на b4321 должен быть behind.
	p2.backends = map[string]*BackendState{
		"n1": {Backend: &types.Backend{ID: "n1", Type: types.BackendTypeLlamaCpp, BuildInfo: "b4321"}},
		"n2": {Backend: &types.Backend{ID: "n2", Type: types.BackendTypeLlamaCpp, BuildInfo: "b5000"}},
	}
	if st := p2.LlamaBuildStatusFor("n1"); st.Relation != llamaRelationBehind {
		t.Errorf("узел b4321 при latest b5000: relation = %q, want behind (%s)", st.Relation, st.Reason)
	}
	if st := p2.LlamaBuildStatusFor("n2"); st.Relation != llamaRelationUpToDate {
		t.Errorf("узел b5000 при latest b5000: relation = %q, want up_to_date", st.Relation)
	}

	snap := p2.LlamaUpstreamSnapshot()
	if len(snap.Behind) != 1 || snap.Behind[0] != "n1" {
		t.Errorf("агрегат behind = %v, want [n1]", snap.Behind)
	}
	if snap.Relation != llamaRelationBehind {
		t.Errorf("агрегат relation = %q, want behind", snap.Relation)
	}
	if len(snap.Backends) != 2 {
		t.Errorf("агрегат: узлов %d, want 2 (llama_cpp только)", len(snap.Backends))
	}
	_ = cfg
}

// TestR269_DisabledCheck — выключенная проверка не выдаёт уведомление.
func TestR269_DisabledCheck(t *testing.T) {
	t.Setenv("LB_LLAMA_UPSTREAM_CHECK_ENABLED", "0")
	tr := &llamaUpstreamTracker{stopCh: make(chan struct{})}
	p := &Proxy{llamaUpstream: tr}
	if p.LlamaUpstreamEnabled() {
		t.Fatal("LB_LLAMA_UPSTREAM_CHECK_ENABLED=0 не отключает проверку")
	}
	st := p.LlamaBuildStatusFor("n1")
	if st.Relation != llamaRelationUnknown {
		t.Errorf("при выключенной проверке relation = %q, want unknown", st.Relation)
	}
	snap := p.LlamaUpstreamSnapshot()
	if snap.Enabled {
		t.Error("агрегат говорит enabled=true при выключенной проверке")
	}
}

// TestR269_NilSafety — урезанный прокси (нет трекера) не паникует.
func TestR269_NilSafety(t *testing.T) {
	var p *Proxy
	if _, _, _, ok := p.llamaUpstreamLatest(); ok {
		t.Error("nil-прокси вернул ok=true")
	}
	if rel, _ := CompareLlamaBuilds("", ""); rel != llamaRelationUnknown {
		t.Errorf("пустые версии дали %q", rel)
	}
	p2 := &Proxy{}
	if st := p2.LlamaBuildStatusFor("x"); st.Relation != llamaRelationUnknown {
		t.Errorf("пустой прокси: relation = %q", st.Relation)
	}
	_ = p2.LlamaUpstreamSnapshot()
}

// TestR305_CompareBuildsByDateAndPrefix — R305 (2026-10-30).
//
// ЖИВЫЕ ФАКТЫ, из-за которых тест появился:
//   - узел сообщает короткий коммит, GitHub — полный; строгое равенство строк
//     объявляло один и тот же коммит разным («same commit» не срабатывал);
//   - у сборки узла не было ДАТЫ, поэтому сравнение с релизом заканчивалось
//     `unknown: different commits, no comparable version`, хотя ответ был:
//     сборка от 2026-09-01 старше релиза от 2026-10-20.
func TestR305_CompareBuildsByDateAndPrefix(t *testing.T) {
	cases := []struct {
		name          string
		current       string
		latest        string
		wantRelation  string
		wantReasonHas string
	}{
		{
			name:          "короткий коммит узла и полный коммит релиза — один коммит",
			current:       "0eadefebd",
			latest:        "v0.8.0 (0eadefebd255bfb5d)",
			wantRelation:  llamaRelationUpToDate,
			wantReasonHas: "same commit",
		},
		{
			name:          "дата сборки старше даты релиза — behind, а не unknown",
			current:       "9be874c3 2026-09-01",
			latest:        "v0.8.0 (8642541b255bfb5d) 2026-10-20",
			wantRelation:  llamaRelationBehind,
			wantReasonHas: "build date earlier",
		},
		{
			name:          "дата сборки позже релиза — ahead (сборка из master)",
			current:       "9be874c3 2026-10-25",
			latest:        "v0.8.0 (8642541b255bfb5d) 2026-10-20",
			wantRelation:  llamaRelationAhead,
			wantReasonHas: "build date later",
		},
		{
			name:          "разные коммиты без дат — честное unknown",
			current:       "0eadefebd",
			latest:        "v0.8.0 (8642541b255bfb5d)",
			wantRelation:  llamaRelationUnknown,
			wantReasonHas: "no comparable version",
		},
	}
	for _, c := range cases {
		rel, why := CompareLlamaBuilds(c.current, c.latest)
		if rel != c.wantRelation {
			t.Errorf("%s: relation=%q (%s), want %q", c.name, rel, why, c.wantRelation)
		}
		if !strings.Contains(why, c.wantReasonHas) {
			t.Errorf("%s: reason=%q, ожидалось вхождение %q", c.name, why, c.wantReasonHas)
		}
	}
}
