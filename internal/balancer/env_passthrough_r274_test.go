// env_passthrough_r274_test.go — R274 (2026-10-29): сторож «переменная читается
// кодом, но не доходит до контейнера».
//
// ПОЧЕМУ ЭТОТ ТЕСТ СУЩЕСТВУЕТ. За серию R250–R272 одна и та же поломка
// повторилась ТРИ раза: переменная читается кодом (`os.Getenv`), оператор задаёт
// её в `deployments/.env`, но в `docker-compose.stack.yml` она не проброшена —
// значит в контейнере её нет, и переключатель молча не работает. Живые случаи:
//
//	LB_NCTX_GROW_FOR_AGENTS            — рост окна контекста для агентского хода;
//	LB_OPENAI_AUTO_STREAM_KEEPALIVE_SEC — «дышащее» тело не-стрим ответа;
//	LB_RELOAD_DEFER_MAX_SEC            — ожидание стримов перед перезагрузкой.
//
// Снаружи это выглядело как «настройка не работает», и каждый раз тратилось
// время на поиск причины в коде, где её не было. Поэтому проверка
// автоматическая: любая `LB_*`, которую читает балансер, обязана быть либо в
// compose, либо в `.env.example` (операторская памятка), либо в allowlist ниже
// с письменным обоснованием.
//
// Тест НЕ требует Docker и не поднимает стек: он читает файлы репозитория.
package balancer

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"
)

// envReadRe — литеральные имена LB_* в коде.
//
// Ловим НЕ только `os.Getenv("LB_X")`: половина переменных читается через
// обёртки и именованные константы (`env.GetInt`, `intFromEnv`, `envSeconds`,
// `EnvShareDiskReserveMB = "LB_..."`). Поэтому достаточно самого строкового
// литерала с именем переменной — он есть в любом из этих способов.
//
// Исключается internal/i18n: там имена переменных встречаются в текстах
// подсказок оператору, и считать это «чтением» значило бы делать вид, что
// переменная обрабатывается, когда она только упомянута в сообщении.
var envReadRe = regexp.MustCompile(`"(LB_[A-Z0-9_]+)"`)

// envDeclaredRe — объявление в compose (`- LB_X=...`) и в example (`LB_X=...`).
var envDeclaredRe = regexp.MustCompile(`(?m)^\s*-?\s*(LB_[A-Z0-9_]+)\s*=`)

// envPassthroughAllow — переменные, которым НЕ нужно быть в compose/example,
// с причиной. Правило: сюда попадает только то, что код читает, но оператор не
// настраивает (значение выводится или фиксировано раскладкой контейнера).
var envPassthroughAllow = map[string]string{
	// Порт публикуется compose (`${LB_PORT:-18080}:18080`), но ВНУТРИ контейнера
	// балансер всегда слушает 18080; код читает LB_PORT только чтобы собрать
	// self-URL для внутренних вызовов и имеет тот же фолбэк 18080.
	"LB_PORT": "порт публикуется compose; внутри контейнера фиксирован 18080",
}

// envDeclaredAllow — переменные, объявленные в секции балансера, но читаемые не
// Go-кодом балансера (иначе сторож ругался бы на нормальную раскладку).
var envDeclaredAllow = map[string]string{
	// Путь к config.json читает entrypoint.sh контейнера (скрипт, а не Go).
	"LB_CONFIG_PATH": "читается entrypoint.sh контейнера, не Go-кодом",
}

// findRepoRootR274 — корень репозитория (по go.mod) от текущего файла.
func findRepoRootR274(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	dir := filepath.Dir(thisFile)
	for i := 0; i < 6; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		dir = filepath.Dir(dir)
	}
	t.Skip("repo root with go.mod not found")
	return ""
}

// readLBEnvVarsFromCode — все LB_*, которые читает балансер.
func readLBEnvVarsFromCode(t *testing.T, root string) []string {
	t.Helper()
	seen := map[string]bool{}
	dirs := []string{
		filepath.Join(root, "internal"),
		filepath.Join(root, "cmd", "balancer"),
		filepath.Join(root, "pkg"),
	}
	for _, d := range dirs {
		err := filepath.WalkDir(d, func(path string, e os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if e.IsDir() {
				// i18n держит имена переменных в текстах подсказок, а не в коде
				// чтения; иначе сторож считал бы «обработанной» переменную,
				// которая только упомянута в сообщении.
				if e.Name() == "i18n" {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			data, rerr := os.ReadFile(path)
			if rerr != nil {
				return rerr
			}
			for _, m := range envReadRe.FindAllStringSubmatch(string(data), -1) {
				seen[m[1]] = true
			}
			return nil
		})
		if err != nil {
			t.Fatalf("обход %s: %v", d, err)
		}
	}
	out := make([]string, 0, len(seen))
	for v := range seen {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

// loadbalancerEnvBlock — только секция `environment:` службы loadbalancer.
//
// ЗАЧЕМ ОГРАНИЧЕНИЕ. В compose несколько служб, и `LB_*` объявляют ещё webui
// (LB_API_URL) и воркеры. Проверять «объявлено, но не читается» по всему файлу
// значило бы ругаться на переменные соседних контейнеров — а сторож, который
// врёт, удаляют первым же.
func loadbalancerEnvBlock(compose string) string {
	lines := strings.Split(compose, "\n")
	inService, inEnv := false, false
	var out []string
	// Заголовок службы — ровно два пробела отступа и имя с двоеточием:
	// "  loadbalancer:", "  webui:". Всё, что глубже, — поля самой службы.
	serviceHeader := regexp.MustCompile(`^  [A-Za-z0-9_.-]+:\s*$`)
	for _, l := range lines {
		trimmed := strings.TrimSpace(l)
		if serviceHeader.MatchString(l) {
			if inService {
				break // началась следующая служба
			}
			if trimmed == "loadbalancer:" {
				inService = true
			}
			continue
		}
		if !inService {
			continue
		}
		if trimmed == "environment:" {
			inEnv = true
			continue
		}
		if inEnv {
			if trimmed == "env_file:" || trimmed == "volumes:" || trimmed == "networks:" ||
				trimmed == "healthcheck:" || trimmed == "profiles:" || trimmed == "ports:" {
				inEnv = false
				continue
			}
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}

// missingFromSources — переменные из `read`, которых нет ни в одном источнике.
//
// Вынесено в чистую функцию, чтобы тест мог проверить САМ себя: если из compose
// убрать строку, функция обязана вернуть эту переменную (иначе сторож мёртв).
func missingFromSources(read []string, sources ...string) []string {
	joined := strings.Join(sources, "\n")
	var missing []string
	for _, v := range read {
		if _, ok := envPassthroughAllow[v]; ok {
			continue
		}
		if !strings.Contains(joined, v) {
			missing = append(missing, v)
		}
	}
	sort.Strings(missing)
	return missing
}

// declaredButUnread — объявленные в compose LB_*, которые код не читает:
// это либо опечатка, либо мёртвый переключатель.
func declaredButUnread(read []string, compose string) []string {
	readSet := map[string]bool{}
	for _, v := range read {
		readSet[v] = true
	}
	seen := map[string]bool{}
	var out []string
	for _, m := range envDeclaredRe.FindAllStringSubmatch(compose, -1) {
		v := m[1]
		if seen[v] || readSet[v] {
			continue
		}
		if _, ok := envPassthroughAllow[v]; ok {
			continue
		}
		if _, ok := envDeclaredAllow[v]; ok {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

// TestR274_EnvPassthrough — каждая LB_*, читаемая кодом, доходит до оператора.
func TestR274_EnvPassthrough(t *testing.T) {
	root := findRepoRootR274(t)

	compose, err := os.ReadFile(filepath.Join(root, "deployments", "docker-compose.stack.yml"))
	if err != nil {
		t.Skipf("compose не найден (%v); пропускаем", err)
	}
	example, err := os.ReadFile(filepath.Join(root, "deployments", ".env.example"))
	if err != nil {
		t.Skipf(".env.example не найден (%v); пропускаем", err)
	}

	read := readLBEnvVarsFromCode(t, root)
	if len(read) < 20 {
		t.Fatalf("найдено подозрительно мало LB_* (%d) — сломан сканер, а не проект", len(read))
	}

	if missing := missingFromSources(read, string(compose), string(example)); len(missing) > 0 {
		sort.Strings(missing)
		t.Fatalf("эти LB_* читаются кодом, но НЕ доходят до контейнера и не описаны оператору:\n  %s\n\n"+
			"Следствие: оператор задаёт значение в deployments/.env, а балансер его не видит — "+
			"переключатель молча не работает. Добавьте переменную в deployments/docker-compose.stack.yml "+
			"(секция loadbalancer, `${VAR:-default}`) или в deployments/.env.example, либо внесите её "+
			"в envPassthroughAllow с причиной.",
			strings.Join(missing, "\n  "))
	}

	if unread := declaredButUnread(read, loadbalancerEnvBlock(string(compose))); len(unread) > 0 {
		t.Fatalf("эти LB_* объявлены в секции балансера, но код их НЕ читает (мёртвый переключатель или опечатка):\n  %s",
			strings.Join(unread, "\n  "))
	}
}

// TestR274_GuardDetectsRemoval — самопроверка сторожа: если из compose убрать
// переменную, он обязан её найти. Без этого тест мог бы «зеленеть» на сломанном
// регекспе и не ловить ровно ту ошибку, ради которой написан.
func TestR274_GuardDetectsRemoval(t *testing.T) {
	root := findRepoRootR274(t)
	composePath := filepath.Join(root, "deployments", "docker-compose.stack.yml")
	compose, err := os.ReadFile(composePath)
	if err != nil {
		t.Skipf("compose не найден (%v); пропускаем", err)
	}
	example, err := os.ReadFile(filepath.Join(root, "deployments", ".env.example"))
	if err != nil {
		t.Skipf(".env.example не найден (%v); пропускаем", err)
	}

	const probe = "LB_RELOAD_DEFER_MAX_SEC"
	read := readLBEnvVarsFromCode(t, root)
	hasProbe := false
	for _, v := range read {
		if v == probe {
			hasProbe = true
			break
		}
	}
	if !hasProbe {
		t.Fatalf("%s больше не читается кодом — обновите пробу в этом тесте", probe)
	}

	// Убираем строку из ОБОИХ источников (в тексте, не в файле) — сторож обязан
	// сообщить о пропаже.
	composeWithout := removeLinesContaining(string(compose), probe)
	exampleWithout := removeLinesContaining(string(example), probe)

	if missing := missingFromSources([]string{probe}, composeWithout, exampleWithout); len(missing) != 1 {
		t.Fatalf("сторож НЕ заметил пропажу %s (missing=%v) — тест бесполезен", probe, missing)
	}
	// А на реальных файлах её быть не должно.
	if missing := missingFromSources([]string{probe}, string(compose), string(example)); len(missing) != 0 {
		t.Fatalf("%s отсутствует в реальных источниках: %v", probe, missing)
	}

	// Тот же принцип для «объявлено, но не читается».
	probeCompose := loadbalancerEnvBlock(string(compose)) +
		"\n      - LB_THIS_VAR_DOES_NOT_EXIST_R274=${LB_THIS_VAR_DOES_NOT_EXIST_R274:-}\n"
	if unread := declaredButUnread(read, probeCompose); len(unread) != 1 || unread[0] != "LB_THIS_VAR_DOES_NOT_EXIST_R274" {
		t.Fatalf("сторож не поймал объявленную, но нечитаемую переменную: %v", unread)
	}
}

// removeLinesContaining — текст без строк, содержащих подстроку.
func removeLinesContaining(text, sub string) string {
	lines := strings.Split(text, "\n")
	kept := make([]string, 0, len(lines))
	for _, l := range lines {
		if strings.Contains(l, sub) {
			continue
		}
		kept = append(kept, l)
	}
	return strings.Join(kept, "\n")
}
