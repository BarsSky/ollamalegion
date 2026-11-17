//go:build llama_stub

// env_consumers_r499_test.go — R499 / аудит S2 (2026-11-19): «у каждой LB_* из
// .env.example есть потребитель».
//
// ЗАЧЕМ. Класс S-1/S-2/S-3/S-5 из аудита — переменная объявлена (в .env.example,
// в конфиге, в UI) и не влияет ни на что. Оператор выставляет значение, сервис его
// не читает, и «не работает» выясняется чтением исходников. Яркий пример:
// LB_GPU_MAX_TEMP читалась из env и умирала в конфиге (закрыто в R499 через
// slot_manager), LB_QUEUE_WORKERS/TIMEOUT оставались только полями JSON.
//
// ПОЧЕМУ ПРОВЕРКА «ЕСТЬ НЕОБКОММЕНТИРОВАННОЕ УПОМИНАНИЕ», А НЕ «ЕСТЬ ЧТЕНИЕ».
// В коде несколько способов читать env (os.Getenv, os.LookupEnv, env.GetInt,
// intFromEnv, settings.HotInt по реестру настроек, косвенно через const), и
// строгий regexp по вызову давал 30+ ложных срабатываний на живых переменных
// (проверено). Здесь проверяется то, что действительно можно утверждать по
// исходнику: имя обязано встречаться в БОЕВОМ коде вне комментариев. Список имён
// для диагностического архива (diag_summary_r476.go) потребителем НЕ считается —
// это перечисление, а не чтение.
//
// Обратная проверка тоже есть: имя, которое встречается в боевом коде, но не
// описано ни в .env.example, ни в реестре feature-switches, — оператор о нём не
// узнает.
package balancer

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

const (
	envExampleR499 = "../../deployments/.env.example"
	// Имена в этом файле — перечисление для архива диагностики, не потребитель.
	diagNamesFileR499 = "diag_summary_r476.go"
)

var envNameReR499 = regexp.MustCompile(`(?:^|[^A-Za-z0-9_])((?:LB|CPPWORKER)_[A-Z0-9_]+)(?:[^A-Za-z0-9_]|$)`)

// prodGoFilesR499 — боевые Go-файлы (без тестов).
func prodGoFilesR499(t *testing.T) []string {
	t.Helper()
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatalf("корень репозитория: %v", err)
	}
	var out []string
	err = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "c", "testdata", "webui":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		out = append(out, path)
		return nil
	})
	if err != nil {
		t.Fatalf("обход репозитория: %v", err)
	}
	return out
}

// envExampleNamesR499 — имена переменных, объявленных в .env.example (включая
// закомментированные: это тоже рекомендация оператору).
func envExampleNamesR499(t *testing.T) map[string]bool {
	t.Helper()
	raw, err := os.ReadFile(envExampleR499)
	if err != nil {
		t.Fatalf("не читается %s: %v", envExampleR499, err)
	}
	out := map[string]bool{}
	declRe := regexp.MustCompile(`^\s*#?\s*((?:LB|CPPWORKER)_[A-Z0-9_]+)=`)
	for _, line := range strings.Split(string(raw), "\n") {
		if m := declRe.FindStringSubmatch(line); m != nil {
			out[m[1]] = true
		}
	}
	return out
}

// consumedNamesR499 — имена, встречающиеся в боевом коде вне комментариев.
func consumedNamesR499(t *testing.T) map[string]string {
	t.Helper()
	found := map[string]string{}
	for _, path := range prodGoFilesR499(t) {
		if filepath.Base(path) == diagNamesFileR499 {
			continue
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(raw), "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "/*") || strings.HasPrefix(trimmed, "*") {
				continue
			}
			for _, m := range envNameReR499.FindAllStringSubmatch(trimmed, -1) {
				if _, ok := found[m[1]]; !ok {
					rel, _ := filepath.Rel("../..", path)
					found[m[1]] = rel
				}
			}
		}
	}
	return found
}

// TestR499_EveryEnvExampleVarHasConsumer — главная проверка S2.
//
// Проверяются ТОЛЬКО `LB_*`: `CPPWORKER_*` на этом уровне часто потребляются не
// Go-кодом, а compose/entrypoint (например CPPWORKER_MEM_LIMIT → mem_limit в
// docker-compose.stack.yml), и «нет потребителя в Go» там ничего не доказывает.
func TestR499_EveryEnvExampleVarHasConsumer(t *testing.T) {
	declared := envExampleNamesR499(t)
	if len(declared) < 100 {
		t.Fatalf("в .env.example найдено %d переменных — стюард проверяет не тот файл", len(declared))
	}
	consumed := consumedNamesR499(t)

	var dead []string
	for name := range declared {
		if !strings.HasPrefix(name, "LB_") {
			continue // compose-уровень: потребитель вне Go-кода
		}
		if _, ok := consumed[name]; !ok {
			dead = append(dead, name)
		}
	}
	sort.Strings(dead)
	if len(dead) > 0 {
		t.Errorf("эти переменные объявлены в %s, но НЕ встречаются в боевом коде "+
			"(кроме комментариев/списка диагностики) — оператор настроит их и ничего не произойдёт:\n    %s\n"+
			"    Класс S-2/S-5 аудита: «настройка без потребителя». Либо провести значение в код, "+
			"либо удалить переменную из .env.example, конфига и доков.",
			envExampleR499, strings.Join(dead, "\n    "))
	}
}

// TestR499_EveryReadVarIsDocumented — обратная сторона: переменную, которую код
// читает, оператор должен найти в .env.example или в реестре переключателей.
func TestR499_EveryReadVarIsDocumented(t *testing.T) {
	declared := envExampleNamesR499(t)
	consumed := consumedNamesR499(t)

	rawDoc, err := os.ReadFile(featureSwitchesDocR387)
	if err != nil {
		t.Fatalf("не читается реестр feature-switches: %v", err)
	}
	doc := string(rawDoc)

	var undocumented []string
	for name, file := range consumed {
		if declared[name] || strings.Contains(doc, name) {
			continue
		}
		if _, allowed := undocumentedAllowedR499[name]; allowed {
			continue
		}
		undocumented = append(undocumented, name+" ("+file+")")
	}
	sort.Strings(undocumented)
	if len(undocumented) > 0 {
		t.Errorf("эти переменные читаются боевым кодом, но не описаны ни в %s, ни в %s:\n    %s",
			envExampleR499, featureSwitchesDocR387, strings.Join(undocumented, "\n    "))
	}
}

// undocumentedAllowedR499 — переменные, которые код читает, но которые пока не
// описаны ни в .env.example, ни в реестре переключателей.
//
// ПОЧЕМУ СПИСОК, А НЕ МОЛЧАНИЕ. Обратная проверка нашла реальный пробел (25 имён):
// оператор не узнает о них ни из .env.example, ни из feature-switches. Выдумать
// для них документацию в этом коммите значило бы смешать S2 (код) с отдельной
// docs-задачей. Поэтому список ЯВНЫЙ, с причиной: тест падает на КАЖДОМ новом
// недокументированном имени, а эти 25 ждут описания (следующий шаг — S2b: внести
// их в .env.example с пояснением, что и когда включать).
var undocumentedAllowedR499 = map[string]string{
	"CPPWORKER_BACKEND_ID":                 "внутренняя идентификация воркера (задаётся compose, не оператором)",
	"CPPWORKER_CAPACITY_FROM_SLOTS":        "внутренняя ёмкость воркера",
	"CPPWORKER_CUDA_ARCH_BUILT":            "печать сборки (identity)",
	"CPPWORKER_LLAMA_BUILD_BAKED":          "печать сборки (identity)",
	"CPPWORKER_LLAMA_BUILD_DATE_BAKED":     "печать сборки (identity)",
	"CPPWORKER_LOAD_STALE_SEC":             "внутренний порог устаревания прогресса загрузки",
	"CPPWORKER_LOAD_WAIT_MAX_SEC":          "внутренний бюджет ожидания загрузки",
	"CPPWORKER_RPC_NODE_HTTP_PORT":         "порт RPC-узла (compose)",
	"CPPWORKER_URL":                        "адрес воркера (compose)",
	"LB_GPU_HEADROOM_PERCENT":              "запас VRAM; документируется вместе с настройками ресурсов",
	"LB_IMAGE_MODEL_PROFILES_PATH":         "путь каталога профилей image-моделей",
	"LB_IMAGE_RESOURCES_PATH":              "путь файла политики image-ресурсов",
	"LB_IMAGE_TOOL":                        "флаг инструмента image (внутренний)",
	"LB_IMAGE_TOOL_INJECT":                 "флаг инъекции инструмента image (внутренний)",
	"LB_LLAMA_UPSTREAM_TIMEOUT_SEC":        "таймаут апстрима llama.cpp (opt-in)",
	"LB_NCTX_PREFLIGHT_ENABLED":            "описан в docs/api/env-vars*.md, ждёт .env.example",
	"LB_NCTX_PREFLIGHT_MAX_WAIT_SEC":       "описан в docs/api/env-vars*.md, ждёт .env.example",
	"LB_NCTX_PREFLIGHT_WAIT_BUFFER_SEC":    "внутренний буфер ожидания preflight",
	"LB_NCTX_PREFLIGHT_WAIT_MULTIPLIER":    "внутренний множитель ожидания preflight",
	"LB_PREFLIGHT_EXACT_TOKENS_TIMEOUT_MS": "таймаут точного подсчёта токенов",
	"LB_SELECTION_ORDER":                   "описан в docs/api/env-vars*.md и в R493-доке; в .env.example — закомментирован",
	"LB_SHARE_DISK_RESERVE_MB":             "резерв диска для шаринга моделей",
	"LB_TLS_HOST":                          "TLS-хост (режим TLS)",
	"LB_TLS_PORT":                          "TLS-порт (режим TLS)",
	"LB_URL":                               "внутренний адрес балансера для агента",
}
