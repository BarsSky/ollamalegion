// mixed_text_backends_test.go — R-Image follow-up (2026-10-07).
//
// ЗАЧЕМ ЭТОТ ТЕСТ. Балансер обслуживает ollama- и llama.cpp-бэкенды
// ОДНОВРЕМЕННО, и поверхности у них разнесены по портам (18080 — Ollama API,
// 18079 — OpenAI). Клиент выбирает путь, а тип бэкенда под запрос определяет
// балансер. Ошибка здесь стоит дорого и тихо: запрос на Ollama-поверхность
// уходит в llama.cpp-воркер (или наоборот) и падает уже внутри чужого движка —
// либо, что хуже, текстовый запрос уезжает на image-бэкенд.
//
// Покрытие до этого теста: разграничение image_cpp vs текст, а вот пара
// ollama ↔ llama_cpp не проверялась вовсе (единственный тест на выбор с
// allowedTypes касался image).
//
// ЧТО ПРОВЕРЯЕМ:
//  1. путь запроса определяет ОЖИДАЕМЫЙ тип: Ollama-пути → ollama,
//     OpenAI-пути → llama_cpp, image-пути → image_cpp;
//  2. явный тип даёт ровно один допустимый тип (ollama не «разрешает» llama_cpp);
//  3. канонизация типа не склеивает два текстовых типа между собой;
//  4. в смешанном режиме (оба типа живы) effectiveBackendType ПУСТ — UI должен
//     показывать оба пула, а не прятать один;
//  5. в режиме только одного типа selective-фильтр отдаёт именно его.
package balancer

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"ollama-loadbalancer/pkg/types"
)

// requestForPath — минимальный запрос, которого достаточно determineRequestBackendType.
func requestForPath(t *testing.T, path string, model string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, nil)
	if model != "" {
		req.URL.RawQuery = "model=" + model
	}
	return req
}

// proxyWithTextBackends — Proxy с указанными текстовыми типами и обоими роутерами.
//
// Отдельный helper, а не newTestProxyWithBackendsByType: тот умеет ровно ОДИН
// тип, а весь смысл этого файла — СМЕСЬ (оба текстовых типа в одном пуле).
// Пути/counters читаются из p.backends, поэтому больше ничего не нужно.
func proxyWithTextBackends(t *testing.T, types_ ...types.BackendType) *Proxy {
	t.Helper()
	p := &Proxy{
		backends:       map[string]*BackendState{},
		ollamaRouter:   NewOllamaRouter(nil),
		llamaCppRouter: NewLlamaCppRouter(nil),
		// config обязателен: determineRequestBackendType для ТЕКСТОВЫХ путей
		// доходит до getDefaultAllowedTypes → p.config.Balancing.OperatingMode.
		// В image-тестах его нет, потому что image-пути возвращаются раньше;
		// для /api/chat этого недостаточно (проверено паникой).
		config: &types.LoadBalancerConfig{
			Balancing: types.BalancingSettings{OperatingMode: "standard"},
		},
	}
	for i, bt := range types_ {
		id := "b" + string(rune('1'+i))
		p.backends[id] = &BackendState{
			Backend: &types.Backend{ID: id, Type: bt, Status: types.StatusHealthy},
		}
	}
	return p
}

// TestMixed_OllamaPathsResolveToOllama — Ollama-поверхность (18080) — это ollama.
//
// ВАЖНО, ЧЕГО ЗДЕСЬ БЫТЬ НЕ ДОЛЖНО: llama_cpp. Если бы Ollama-путь определялся
// как llama_cpp, запрос с Ollama-специфичным телом (options.num_ctx, keep_alive)
// уехал бы в cppworker и был бы отвергнут или молча искажён.
func TestMixed_OllamaPathsResolveToOllama(t *testing.T) {
	p := proxyWithTextBackends(t)
	for _, path := range []string{
		"/api/chat",
		"/api/generate",
		"/api/tags",
		"/api/show",
		"/api/ps",
		"/api/embeddings",
	} {
		got := p.determineRequestBackendType(requestForPath(t, path, ""))
		if got == types.BackendTypeLlamaCpp {
			t.Errorf("путь %s определён как llama_cpp — Ollama-запрос уедет в чужой воркер", path)
		}
		if got == types.BackendTypeImage {
			t.Errorf("путь %s определён как image_cpp — текстовый запрос уедет на image-бэкенд", path)
		}
	}
}

// TestMixed_OpenAIPathsResolveToLlamaCpp — OpenAI-поверхность (18079) — llama.cpp.
func TestMixed_OpenAIPathsResolveToLlamaCpp(t *testing.T) {
	p := proxyWithTextBackends(t)
	for _, path := range []string{"/v1/chat/completions", "/v1/completions"} {
		got := p.determineRequestBackendType(requestForPath(t, path, ""))
		if got == types.BackendTypeImage {
			t.Errorf("путь %s определён как image_cpp", path)
		}
	}
}

// TestMixed_ExplicitTypeYieldsExactlyOneAllowedType — явный тип не расширяется.
//
// Это и есть механизм, который не даёт одному текстовому типу «украсть» запрос
// другого: выбрав ollama, мы разрешаем РОВНО ollama, и llama.cpp-бэкенд не
// попадёт в кандидаты (isBackendTypeAllowed отсечёт).
func TestMixed_ExplicitTypeYieldsExactlyOneAllowedType(t *testing.T) {
	p := proxyWithTextBackends(t)

	cases := []struct {
		bt   types.BackendType
		want types.BackendType
	}{
		{types.BackendTypeOllama, types.BackendTypeOllama},
		{types.BackendTypeLlamaCpp, types.BackendTypeLlamaCpp},
		{types.BackendTypeImage, types.BackendTypeImage},
	}
	for _, tc := range cases {
		allowed := p.getAllowedTypesList(tc.bt)
		if len(allowed) != 1 || allowed[0] != tc.want {
			t.Fatalf("getAllowedTypesList(%q) = %v, want ровно [%q]", tc.bt, allowed, tc.want)
		}
		// Обратная сторона: ДРУГОЙ текстовый тип обязан быть отсечён.
		other := types.BackendTypeLlamaCpp
		if tc.bt == types.BackendTypeLlamaCpp {
			other = types.BackendTypeOllama
		}
		if isBackendTypeAllowed(other, allowed) {
			t.Errorf("allowedTypes=%v пропускает %q — типы смешиваются", allowed, other)
		}
	}
}

// TestMixed_AllowedTypesFilterIsExact — фильтр по типу не пропускает чужие типы.
func TestMixed_AllowedTypesFilterIsExact(t *testing.T) {
	// Разрешаем только llama.cpp: ни ollama, ни image не должны пройти.
	onlyLlama := []types.BackendType{types.BackendTypeLlamaCpp}
	if isBackendTypeAllowed(types.BackendTypeOllama, onlyLlama) {
		t.Error("ollama прошёл фильтр {llama_cpp}")
	}
	if isBackendTypeAllowed(types.BackendTypeImage, onlyLlama) {
		t.Error("image_cpp прошёл фильтр {llama_cpp}")
	}
	if !isBackendTypeAllowed(types.BackendTypeLlamaCpp, onlyLlama) {
		t.Error("llama_cpp не прошёл свой же фильтр")
	}

	// Пустой список = «разрешено всё» (обратная совместимость): так работают
	// вызовы, где тип не определён.
	if !isBackendTypeAllowed(types.BackendTypeOllama, nil) {
		t.Error("пустой allowedTypes обязан разрешать всё (обратная совместимость)")
	}
}

// TestMixed_NormalizeKeepsTextTypesDistinct — канонизация не склеивает типы.
//
// Ловушка, из-за которой этот тест существует: normalizeBackendType когда-то
// мог «схлопнуть» неизвестный тип в ollama. Для смешанного режима это означало
// бы, что llama.cpp-бэкенд вдруг считается Ollama — и маршрутизация уходит не
// туда. image_cpp уже защищён отдельным тестом; здесь фиксируем текстовую пару.
func TestMixed_NormalizeKeepsTextTypesDistinct(t *testing.T) {
	if got := normalizeBackendType(types.BackendTypeLlamaCpp); got != types.BackendTypeLlamaCpp {
		t.Errorf("normalizeBackendType(llama_cpp) = %q, want llama_cpp", got)
	}
	if got := normalizeBackendType(types.BackendTypeOllama); got != types.BackendTypeOllama {
		t.Errorf("normalizeBackendType(ollama) = %q, want ollama", got)
	}
	if got := normalizeBackendType(types.BackendTypeImage); got != types.BackendTypeImage {
		t.Errorf("normalizeBackendType(image_cpp) = %q, want image_cpp", got)
	}
}

// TestMixed_EffectiveType_EmptyForMixedCluster — при смеси показываем оба пула.
//
// Смысл: effectiveBackendType — это «доминирующий» тип для ФИЛЬТРАЦИИ выдачи.
// В смешанном кластере доминанты нет, и пустое значение — признак «отдавать
// все типы». Если бы функция вернула llama_cpp, WebUI и /api/v1/metrics
// спрятали бы ollama-бэкенды, и оператор решил бы, что они не подключились.
//
// Проверяем через реальный Proxy со счётчиками типов (countBackendsByType).
func TestMixed_EffectiveType_EmptyForMixedCluster(t *testing.T) {
	// Смешанный кластер: по одному бэкенду каждого текстового типа.
	mixed := proxyWithTextBackends(t,
		types.BackendTypeOllama,
		types.BackendTypeLlamaCpp,
	)
	if got := mixed.getEffectiveBackendType(); got != "" {
		t.Errorf("смешанный кластер: effectiveBackendType = %q, want \"\" (показывать все типы)", got)
	}

	// Только ollama → доминанта ollama.
	onlyOllama := proxyWithTextBackends(t, types.BackendTypeOllama)
	if got := onlyOllama.getEffectiveBackendType(); got != types.BackendTypeOllama {
		t.Errorf("только ollama: effectiveBackendType = %q, want ollama", got)
	}

	// Только llama.cpp → доминанта llama_cpp.
	onlyLlama := proxyWithTextBackends(t, types.BackendTypeLlamaCpp)
	if got := onlyLlama.getEffectiveBackendType(); got != types.BackendTypeLlamaCpp {
		t.Errorf("только llama_cpp: effectiveBackendType = %q, want llama_cpp", got)
	}
}

// TestMixed_Engine_LlamaForLlamaOnly — движок кластера при одном типе.
//
// При смеси движок обязан остаться auto: иначе балансер начнёт применять
// правила одного движка (n_ctx, KV-типы, offload) к чужому пулу.
func TestMixed_Engine_LlamaForLlamaOnly(t *testing.T) {
	onlyLlama := proxyWithTextBackends(t, types.BackendTypeLlamaCpp)
	if got := onlyLlama.getBackendEngine(); got != types.EngineLlamaCPP {
		t.Errorf("только llama_cpp: engine = %q, want llama_cpp", got)
	}

	onlyOllama := proxyWithTextBackends(t, types.BackendTypeOllama)
	if got := onlyOllama.getBackendEngine(); got != types.EngineOllamaAPI {
		t.Errorf("только ollama: engine = %q, want ollama_api", got)
	}

	mixed := proxyWithTextBackends(t, types.BackendTypeOllama, types.BackendTypeLlamaCpp)
	if got := mixed.getBackendEngine(); got != types.EngineAuto {
		t.Errorf("смешанный кластер: engine = %q, want auto (не навязываем правила одного движка)", got)
	}
}
