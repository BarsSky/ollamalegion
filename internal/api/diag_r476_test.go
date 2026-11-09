//go:build llama_stub

// diag_r476_test.go — R476 (2026-11-07): сводка и архив диагностики.
//
// Проверяем не «вызывается без паники», а именно то, из-за чего это делалось:
// флаги видны и разделены на «задано/не задано», секреты в выгрузку не
// попадают, а диагностический запрос к воркеру несёт его токен.
package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"ollama-loadbalancer/pkg/logger"
)

// TestR476_FlagsSplitSetAndUnset — оператор должен видеть, что значение пришло
// из .env, а не из дефолта кода: иначе разбор идёт по несуществующей настройке.
func TestR476_FlagsSplitSetAndUnset(t *testing.T) {
	t.Setenv("LB_REQUEST_TRACE", "full")
	t.Setenv("LB_NCTX_RELOAD_MAX_N_CTX", "")
	os.Unsetenv("LB_NCTX_RELOAD_MAX_N_CTX")

	snap := diagFlagsSnapshotR476()
	set, _ := snap["set"].(map[string]string)
	unset, _ := snap["unset"].([]string)

	if set["LB_REQUEST_TRACE"] != "full" {
		t.Fatalf("заданный флаг не попал в set: %v", set)
	}
	if !containsStrR476(unset, "LB_NCTX_RELOAD_MAX_N_CTX") {
		t.Fatalf("незаданный флаг не попал в unset: %v", unset)
	}
}

// TestR476_FlagsHaveNoSecrets — белый список: токены и пароли в сводку не
// попадают, иначе архив нельзя передавать.
func TestR476_FlagsHaveNoSecrets(t *testing.T) {
	t.Setenv("LB_API_TOKEN", "super-secret-token")
	t.Setenv("LB_ADMIN_PASSWORD", "super-secret-password")
	t.Setenv("LB_AUTH_TOKENS", "shared-secret")

	snap := diagFlagsSnapshotR476()
	raw, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, secret := range []string{"super-secret-token", "super-secret-password", "shared-secret"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("секрет %q попал в диагностические флаги: %s", secret, raw)
		}
	}
}

// TestR476_SummaryPayloadWithoutProxy — сводка строится и на «пустом» сервере
// (нет proxy/config): страница диагностики не должна падать в тестах и на
// старте, когда состояние ещё не собрано.
func TestR476_SummaryPayloadWithoutProxy(t *testing.T) {
	t.Setenv("LB_REQUEST_TRACE", "summary")
	s := &Server{}
	payload := s.diagSummaryPayloadR476()
	for _, key := range []string{"generated_at", "version", "flags", "model_profiles"} {
		if _, ok := payload[key]; !ok {
			t.Fatalf("в сводке нет %q: %v", key, payload)
		}
	}
	if _, ok := payload["cluster"]; ok {
		t.Fatal("cluster без proxy не должен появляться")
	}
	if _, err := json.Marshal(payload); err != nil {
		t.Fatalf("сводка не сериализуется: %v", err)
	}
}

// TestR476_BundleFlagsText — текстовый файл флагов: и set, и unset видимы.
func TestR476_BundleFlagsText(t *testing.T) {
	t.Setenv("LB_REQUEST_TRACE", "full")
	os.Unsetenv("LB_NCTX_GROW_FOR_STREAM")

	text := diagBundleFlagsTextR476()
	if !strings.Contains(text, "LB_REQUEST_TRACE=full") {
		t.Fatalf("нет заданного флага:\n%s", text)
	}
	if !strings.Contains(text, "LB_NCTX_GROW_FOR_STREAM= (unset)") {
		t.Fatalf("нет пометки unset:\n%s", text)
	}
	if !strings.Contains(text, "unset = переменная не задана") {
		t.Fatalf("нет пояснения про unset:\n%s", text)
	}
}

// TestR476_BundleHTTPCarriesWorkerToken — без X-API-Token воркер отвечает 401,
// и архив остался бы без его части.
func TestR476_BundleHTTPCarriesWorkerToken(t *testing.T) {
	var gotToken string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotToken = r.Header.Get("X-API-Token")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	body, err := diagBundleHTTPGetR476(srv.URL, "worker-token-476")
	if err != nil {
		t.Fatalf("запрос не удался: %v", err)
	}
	if gotToken != "worker-token-476" {
		t.Fatalf("X-API-Token = %q, ожидался токен воркера", gotToken)
	}
	if !strings.Contains(string(body), "ok") {
		t.Fatalf("тело ответа потеряно: %s", body)
	}
}

// TestR476_BundleHTTPErrorIsError — «воркер не ответил» обязан быть ошибкой,
// иначе архив молча останется без его части.
func TestR476_BundleHTTPErrorIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer srv.Close()

	if _, err := diagBundleHTTPGetR476(srv.URL, ""); err == nil {
		t.Fatal("401 должен вернуть ошибку")
	}
}

// TestR476_BalancerLogsFromBroker — в архив попадают реальные записи брокера,
// а не пустота (та же ошибка, что была с пустым буфером до R390).
func TestR476_BalancerLogsFromBroker(t *testing.T) {
	b := logger.NewLogBroker(50)
	logger.SetBroker(b)
	defer logger.SetBroker(nil)

	b.Publish(logger.LogEntry{Level: "info", Source: "balancer", Message: "request trace request_id=abc123"})

	text := diagBundleBalancerLogsR476()
	if !strings.Contains(text, "request_id=abc123") {
		t.Fatalf("в выгрузке нет записи брокера:\n%s", text)
	}
	if !strings.Contains(text, "INFO") {
		t.Fatalf("в выгрузке нет уровня записи:\n%s", text)
	}
}

func containsStrR476(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

// TestR476_BundleSinceParsing — `?since=` принимает длительность и RFC3339,
// а мусор не превращается в пустой архив.
func TestR476_BundleSinceParsing(t *testing.T) {
	if !diagBundleSinceCutoffR476("").IsZero() {
		t.Fatal("пустой since должен означать «без фильтра»")
	}
	if !diagBundleSinceCutoffR476("не-время").IsZero() {
		t.Fatal("мусор в since должен означать «без фильтра», а не пустой архив")
	}
	if d := time.Since(diagBundleSinceCutoffR476("2h")); d < 119*time.Minute || d > 121*time.Minute {
		t.Fatalf("since=2h дал cutoff %v назад", d)
	}
	ts := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	if got := diagBundleSinceCutoffR476(ts.Format(time.RFC3339)); !got.Equal(ts) {
		t.Fatalf("RFC3339 разобран как %v, ожидалось %v", got, ts)
	}
}

// TestR476_BalancerLogsSinceFilters — свой буфер тоже фильтруется по since.
func TestR476_BalancerLogsSinceFilters(t *testing.T) {
	b := logger.NewLogBroker(50)
	logger.SetBroker(b)
	defer logger.SetBroker(nil)

	b.Publish(logger.LogEntry{Time: time.Now().Add(-3 * time.Hour), Level: "info", Message: "OLD-BAL-476"})
	b.Publish(logger.LogEntry{Time: time.Now(), Level: "info", Message: "NEW-BAL-476"})

	text := diagBundleBalancerLogsSinceR476(time.Now().Add(-time.Hour))
	if strings.Contains(text, "OLD-BAL-476") {
		t.Fatalf("старая запись не отфильтрована: %s", text)
	}
	if !strings.Contains(text, "NEW-BAL-476") {
		t.Fatalf("свежая запись потеряна: %s", text)
	}
}
