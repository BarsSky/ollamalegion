//go:build llama_stub

// exact_prompt_tokens_r473_test.go — R473 (2026-11-06): оценка промпта заменяется
// ФАКТОМ от токенизатора модели, когда он доступен.
//
// ЖИВОЕ ЗАВЫШЕНИЕ эвристики на этом стенде: 49731 при реальных 9816; 36601 при
// ~20.7k (Cline считает по нашему же usage). Из завышенного числа рождались
// отказы 413 (до R466), отменённый рост (до R472) и перезагрузки на окно больше
// нужного (40k там, где хватало ~33k).
package balancer

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestR473_ExactReplacesEstimate — успешный ответ воркера заменяет оценку точным
// числом плюс запас на преамбулу шаблона.
func TestR473_ExactReplacesEstimate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/tokenize" {
			t.Errorf("неожиданный путь %q", r.URL.Path)
		}
		var req map[string]string
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("тело не JSON: %v", err)
		}
		if req["model"] != "m" || req["text"] == "" {
			t.Errorf("в теле нет модели/текста: %v", req)
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"tokens": 1234, "source": "tokenizer", "model_loaded": true,
		})
	}))
	defer srv.Close()

	p := &Proxy{}
	p.ReplaceModelProfiles(nil)
	n, ok := p.callTokenizeR473(srv.URL, "m", "текст промпта")
	if !ok {
		t.Fatal("точный подсчёт не принят")
	}
	if n != 1234 {
		t.Fatalf("tokens = %d, want 1234", n)
	}
}

// TestR473_EstimateSourceIsNotTrusted — воркер ответил source=estimate: это не
// факт, брать его вместо оценки нельзя (иначе мы просто меняем одну эвристику на
// другую).
func TestR473_EstimateSourceIsNotTrusted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"tokens": 999, "source": "estimate", "model_loaded": false,
		})
	}))
	defer srv.Close()

	p := &Proxy{}
	if _, ok := p.callTokenizeR473(srv.URL, "m", "x"); ok {
		t.Fatal("source=estimate принят как факт — это ложь клиенту и решениям")
	}
}

// TestR473_HTTPFailuresFallBack — 404/500/мусор: остаёмся при эвристике, без
// паники и без «нуля вместо окна».
func TestR473_HTTPFailuresFallBack(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		// статус после указателей: fieldalignment (govet)
		status int
	}{
		{"старый воркер без ручки", `{"error":"404 page not found"}`, http.StatusNotFound},
		{"внутренняя ошибка", `boom`, http.StatusInternalServerError},
		{"мусор в теле", `not json`, http.StatusOK},
		{"токенизатор вернул 0", `{"tokens":0,"source":"tokenizer"}`, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			p := &Proxy{}
			if _, ok := p.callTokenizeR473(srv.URL, "m", "x"); ok {
				t.Fatalf("%s: отказ воркера принят как точный подсчёт", tc.name)
			}
		})
	}
}

// TestR473_SwitchOff — рубильник выключает точный подсчёт (прежнее поведение).
func TestR473_SwitchOff(t *testing.T) {
	t.Setenv(exactTokensSwitchEnvR473, "0")
	if exactTokensEnabledR473() {
		t.Error("LB_PREFLIGHT_EXACT_TOKENS=0 обязан выключать точный подсчёт")
	}
	t.Setenv(exactTokensSwitchEnvR473, "")
	if !exactTokensEnabledR473() {
		t.Error("по умолчанию точный подсчёт включён")
	}
	t.Setenv(exactTokensSwitchEnvR473, "абракадабра")
	if !exactTokensEnabledR473() {
		t.Error("неизвестное значение = включено (и WARN в лог)")
	}
}

// TestR473_SampleIncludesTools — образец включает tools[]: у агентских клиентов
// схемы инструментов занимают больше места, чем сами сообщения.
func TestR473_SampleIncludesTools(t *testing.T) {
	tools := []map[string]any{{"type": "function", "function": map[string]any{"name": "editor"}}}
	sample := exactSampleWithToolsR473("привет", tools)
	if !strings.Contains(sample, "привет") || !strings.Contains(sample, `"editor"`) {
		t.Fatalf("в образце нет текста или tools: %s", sample)
	}
	if got := exactSampleWithToolsR473("привет", nil); got != "привет" {
		t.Fatalf("без tools образец = %q, want исходный текст", got)
	}
	if got := exactSampleWithToolsR473("", []map[string]any{}); got != "" {
		t.Fatalf("пустые tools не должны добавлять мусор: %q", got)
	}
}

// TestR473_SmallSampleSkipsCall — мелкий промпт не ходит в сеть: там завышение не
// влияет на решения, а запрос на горячем пути не нужен.
func TestR473_SmallSampleSkipsCall(t *testing.T) {
	p := &Proxy{}
	meta := &RequestMeta{ModelName: "m", ExactSampleText: "короткий промпт"}
	if _, ok := p.exactPromptTokensR473(meta); ok {
		t.Fatal("мелкий образец не должен вызывать токенизатор")
	}
}

// TestR473_CacheServesSecondCall — мету одного запроса собирают ДВА пути (guard
// роста и preflight), поэтому живой лог показывал два одинаковых вызова
// токенизатора. Кэш обязан отдать второе число без HTTP.
func TestR473_CacheServesSecondCall(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"tokens": 4242, "source": "tokenizer", "model_loaded": true,
		})
	}))
	defer srv.Close()

	// Уникальный образец: тесты не должны делить записи кэша между собой.
	sample := strings.Repeat("кэш-проверка-", exactTokensMinSampleR473)
	key := exactTokensKeyR473("cache-test-model", sample)
	exactTokensCacheMu.Lock()
	delete(exactTokensCacheData, key)
	exactTokensCacheMu.Unlock()

	p := &Proxy{}
	first, ok := p.callTokenizeR473(srv.URL, "cache-test-model", sample)
	if !ok || first != 4242 {
		t.Fatalf("первый вызов: tokens=%d ok=%v", first, ok)
	}
	exactTokensCachePutR473(key, first+exactTokensOverheadTokensR473)

	if cached, ok := exactTokensCacheGetR473(key); !ok || cached != 4242+exactTokensOverheadTokensR473 {
		t.Fatalf("из кэша не пришло число: %d ok=%v", cached, ok)
	}
	if calls != 1 {
		t.Fatalf("HTTP-вызовов = %d, want 1 (второй обязан обслужить кэш)", calls)
	}
}

// TestR473_CacheKeyIsModelAndText — разные модели и разные тексты не делят запись:
// иначе чужое число токенов попадёт в решение об окне.
func TestR473_CacheKeyIsModelAndText(t *testing.T) {
	sample := "текст промпта"
	k1 := exactTokensKeyR473("model-a", sample)
	k2 := exactTokensKeyR473("model-b", sample)
	k3 := exactTokensKeyR473("model-a", sample+" другой")
	if k1 == k2 || k1 == k3 || k2 == k3 {
		t.Fatalf("ключи совпали: %q %q %q", k1, k2, k3)
	}
}

// TestR473_CacheEvictsOldest — переполнение вытесняет самую старую запись, а не
// растёт без предела (кэш живёт в процессе).
func TestR473_CacheEvictsOldest(t *testing.T) {
	exactTokensCacheMu.Lock()
	exactTokensCacheData = map[string]exactTokensCacheEntryR473{}
	exactTokensCacheMu.Unlock()

	for i := 0; i < exactTokensCacheMaxR473+5; i++ {
		exactTokensCachePutR473("evict-"+strings.Repeat("x", i), i)
	}
	exactTokensCacheMu.Lock()
	size := len(exactTokensCacheData)
	exactTokensCacheMu.Unlock()
	if size > exactTokensCacheMaxR473 {
		t.Fatalf("записей в кэше %d, предел %d", size, exactTokensCacheMaxR473)
	}
	if _, ok := exactTokensCacheGetR473("evict-"); ok {
		t.Fatal("самая старая запись должна быть вытеснена")
	}
}

// TestR473_ParallelCallersShareOneRequest — живой остаток: guard роста и preflight
// собирают мету ПАРАЛЛЕЛЬНО (разница 0.8 мс при HTTP 1–2 мс), и кэш от этого не
// спасал — оба шли в воркер. Теперь второй ждёт первого (single-flight).
func TestR473_ParallelCallersShareOneRequest(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		// Задержка делает окно гонки широким: без single-flight оба вызова
		// промахнутся по кэшу.
		time.Sleep(50 * time.Millisecond)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"tokens": 777, "source": "tokenizer", "model_loaded": true,
		})
	}))
	defer srv.Close()

	sample := strings.Repeat("параллельный-образец-", exactTokensMinSampleR473)
	key := exactTokensKeyR473("parallel-model", sample)
	exactTokensCacheMu.Lock()
	delete(exactTokensCacheData, key)
	delete(exactTokensInflightR473, key)
	exactTokensCacheMu.Unlock()

	p := &Proxy{}
	const n = 4
	results := make([]int, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			// Ровно тот контракт, что у настоящих путей: join-or-start, затем publish.
			if tokens, ok, shared := exactTokensJoinOrStartR473(key); shared {
				if ok {
					results[idx] = tokens
				}
				return
			}
			tokens, ok := 0, false
			defer exactTokensPublishR473(key, &tokens, &ok)
			nTokens, tokOK := p.callTokenizeR473(srv.URL, "parallel-model", sample)
			if !tokOK {
				return
			}
			tokens, ok = nTokens+exactTokensOverheadTokensR473, true
			exactTokensCachePutR473(key, tokens)
			results[idx] = tokens
		}(i)
	}
	wg.Wait()

	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("HTTP-вызовов токенизатора = %d, want 1 (single-flight)", got)
	}
	want := 777 + exactTokensOverheadTokensR473
	for i, got := range results {
		if got != want {
			t.Errorf("участник %d получил %d, want %d (все обязаны получить одно число)", i, got, want)
		}
	}
}
