// Package api — lint regression test для compose-файлов развёртывания.
//
// ЗАЧЕМ. На живой паре из двух машин image-воркер второй машины регистрировался
// с host = имени контейнера («imageworker»), хотя BACKEND_HOST был задан и для
// cppworker всё работало. Причина — не код, а цепочка переменных в compose:
//
//   - SDWORKER_ADVERTISE_HOST=${SDWORKER_ADVERTISE_HOST:-${BACKEND_HOST:-imageworker}}
//   - AGENT_PUBLIC_HOST=${SDWORKER_ADVERTISE_HOST:-imageworker}      <-- ЛОВУШКА
//
// Интерполяция compose — СТАТИЧЕСКАЯ подстановка текста по окружению хоста и
// файлу `.env`. Вторая строка НЕ видит значения, которое первая присваивает
// контейнеру: `SDWORKER_ADVERTISE_HOST` в `.env` не задан, поэтому выражение
// всегда давало литерал `imageworker`. Итог: саморегистрация sdworker'а писала
// правильный адрес, а встроенный агент тут же перекрывал его именем контейнера
// (ветка «backend exists» в регистрации агента записывает Host из запроса).
// Балансер резолвил это имя в СВОЙ контейнер, и запросы к удалённой машине
// молча уходили на первую — «модели на диске есть», хотя у воркера их нет.
//
// ПРАВИЛО, которое проверяет этот тест: в блоке `environment:` переменная может
// интерполировать только то, что оператор задаёт в `.env`, либо то, что вложено
// в то же выражение. Ссылаться на переменную, которую присваивает ДРУГАЯ строка
// этого же сервиса, нельзя — она не подставится. Самоссылка (`VAR=${VAR:-...}`)
// разрешена: именно так операторский override и читается.
package api

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// composeEnvAssignRe — строка вида "  - NAME=value" внутри environment.
var composeEnvAssignRe = regexp.MustCompile(`^\s*-\s*([A-Z][A-Z0-9_]*)=(.*)$`)

// composeEnvMapRe — форма словаря: "      NAME: value".
var composeEnvMapRe = regexp.MustCompile(`^\s{6,}([A-Z][A-Z0-9_]*):\s*(.*)$`)

// composeServiceRe — заголовок сервиса: "  name:" на двух пробелах.
var composeServiceRe = regexp.MustCompile(`^  ([a-zA-Z0-9_.-]+):\s*$`)

// composeEnvRefRe — ссылка ${NAME...} внутри значения.
var composeEnvRefRe = regexp.MustCompile(`\$\{([A-Z][A-Z0-9_]*)`)

// _ = composeEnvRefRe — оставлен как документация формы ссылки; разбор идёт
// сбалансированным сканером envRefOccurrences, потому что регулярное выражение
// не умеет вложенные ${A:-${B:-c}}.
var _ = composeEnvRefRe

// operatorSettableNames — имена, которые оператор задаёт в образце `.env`.
// Отсутствие имени здесь означает: подставить его может только compose-файл.
func operatorSettableNames(repoRoot string) map[string]bool {
	names := map[string]bool{}
	files := []string{
		filepath.Join(repoRoot, "deployments", ".env.example"),
		filepath.Join(repoRoot, "deployments", ".env"),
		filepath.Join(repoRoot, "deployments", ".env.bundled-with-agent.example"),
	}
	re := regexp.MustCompile(`(?m)^\s*#?\s*([A-Z][A-Z0-9_]*)\s*=`)
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		for _, m := range re.FindAllStringSubmatch(string(data), -1) {
			names[m[1]] = true
		}
	}
	return names
}

// envSentinel — маркер «значение задаёт оператор». Пока переменная достижима для
// оператора, обе строки читают её из окружения одинаково, и расхождения нет.
const envSentinel = "\x00OPERATOR"

// splitEnvInner — разделяет внутренность ${...} на имя и модификатор.
// Понимает все формы compose: ${X}, ${X:-d}, ${X-d}, ${X:?err}, ${X:=d}, ${X:+a}.
func splitEnvInner(inner string) (name, def string, hasDef, required bool) {
	for i := 0; i < len(inner); i++ {
		if inner[i] != ':' && inner[i] != '-' && inner[i] != '?' && inner[i] != '=' && inner[i] != '+' {
			continue
		}
		op := inner[i]
		rest := i + 1
		if op == ':' && rest < len(inner) {
			op = inner[rest]
			rest++
		} else if op == ':' {
			continue
		}
		name = strings.TrimSpace(inner[:i])
		if op == '?' {
			return name, inner[rest:], false, true
		}
		if op == '+' {
			// ${X:+alt} — alt подставляется, когда X ЗАДАН.
			return name, inner[rest:], true, false
		}
		return name, inner[rest:], true, false
	}
	return strings.TrimSpace(inner), "", false, false
}

// expandEnvExpr — во что сведётся выражение при заданном наборе переменных
// окружения (set содержит имена, которые оператор ЗАДАЛ; остальные считаются
// незаданными). Понимает вложенные формы `${A:-${B:-c}}` и `${A}`.
func expandEnvExpr(expr string, set map[string]bool, depth int) string {
	if depth > 8 {
		return expr
	}
	var out strings.Builder
	for i := 0; i < len(expr); {
		if i+1 < len(expr) && expr[i] == '$' && expr[i+1] == '{' {
			// Находим парную закрывающую скобку с учётом вложенности.
			level := 1
			j := i + 2
			for j < len(expr) && level > 0 {
				switch expr[j] {
				case '{':
					level++
				case '}':
					level--
				}
				j++
			}
			name, def, hasDef, _ := splitEnvInner(expr[i+2 : j-1])
			switch {
			case set[name]:
				out.WriteString(envSentinel)
			case hasDef:
				out.WriteString(expandEnvExpr(def, set, depth+1))
			}
			// ${NAME} без значения и без оператора -> пустая строка.
			i = j
			continue
		}
		out.WriteByte(expr[i])
		i++
	}
	return out.String()
}

// collectVars — рекурсивно собирает ВСЕ имена переменных выражения, включая
// вложенные в значения по умолчанию.
//
// Тонкость, на которой lint сначала пропустил реальный дефект: для строки
// `${SDWORKER_ADVERTISE_HOST:-${BACKEND_HOST:-imageworker}}` недостаточно взять
// только верхние вхождения — внутренний `${BACKEND_HOST...}` тоже переменная, и
// именно от неё зависит, разъедутся ли значения. Без рекурсии перебор сценариев
// не включал BACKEND_HOST и дефект не воспроизводился.
func collectVars(expr string, out map[string]bool) {
	for i := 0; i < len(expr); i++ {
		if expr[i] != '$' || i+1 >= len(expr) || expr[i+1] != '{' {
			continue
		}
		level, j := 1, i+2
		for j < len(expr) && level > 0 {
			switch expr[j] {
			case '{':
				level++
			case '}':
				level--
			}
			j++
		}
		if level != 0 {
			return
		}
		name, def, hasDef, _ := splitEnvInner(expr[i+2 : j-1])
		if name != "" {
			out[name] = true
		}
		if hasDef {
			collectVars(def, out)
		}
		i = j - 1
	}
}

// referencedVars — имена переменных, встречающиеся в выражении (включая вложенные).
func referencedVars(expr string) []string {
	seen := map[string]bool{}
	collectVars(expr, seen)
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// divergeUnderSomeEnv — разъедутся ли назначение и ссылка хотя бы при одной
// комбинации «какие переменные оператор задал».
//
// Перебор нужен, потому что одного сценария «оператор не задал ничего» МАЛО:
// именно так lint сначала не поймал реальный дефект. Там назначение
// SDWORKER_ADVERTISE_HOST=${SDWORKER_ADVERTISE_HOST:-${BACKEND_HOST:-imageworker}}
// и ссылка ${SDWORKER_ADVERTISE_HOST:-imageworker} совпадают при пустом
// окружении (оба дают imageworker) и расходятся, как только оператор задал
// BACKEND_HOST, — а это и есть рабочий случай удалённой машины.
//
// Возвращает (разъехались, сценарий, значение назначения, значение ссылки).
func divergeUnderSomeEnv(expr, occ string, settable map[string]bool) (bool, []string, string, string) {
	vars := referencedVars(expr + " " + occ)
	if len(vars) > 6 {
		vars = vars[:6] // защита от взрыва; на практике переменных 1-3
	}
	for mask := 0; mask < (1 << len(vars)); mask++ {
		set := map[string]bool{}
		var scenario []string
		for bit, name := range vars {
			if !settable[name] {
				continue // незадаваемые оператором переменные в переборе не участвуют
			}
			if mask&(1<<bit) != 0 {
				set[name] = true
				scenario = append(scenario, name+"=<задана>")
			}
		}
		a := expandEnvExpr(expr, set, 0)
		b := expandEnvExpr(occ, set, 0)
		if a != b {
			return true, scenario, a, b
		}
	}
	return false, nil, "", ""
}

// envRefOccurrences — все сбалансированные выражения ${...} в значении.
// Возвращает подстроки целиком (с вложенными скобками), например
// "${A:-${B:-c}}".
func envRefOccurrences(value string) []string {
	var out []string
	for i := 0; i < len(value); i++ {
		if value[i] != '$' || i+1 >= len(value) || value[i+1] != '{' {
			continue
		}
		level, j := 1, i+2
		for j < len(value) && level > 0 {
			switch value[j] {
			case '{':
				level++
			case '}':
				level--
			}
			j++
		}
		if level == 0 {
			out = append(out, value[i:j])
			i = j - 1
		}
	}
	return out
}

// refName — имя переменной из выражения ${NAME...}.
func refName(occurrence string) string {
	inner := strings.TrimSuffix(strings.TrimPrefix(occurrence, "${"), "}")
	name, _, _, _ := splitEnvInner(inner)
	return name
}

// findInterpolationProblems — ищет в содержимом compose-файла места, где
// ссылка на переменную разъезжается с её назначением в том же сервисе.
// Вынесено отдельно, чтобы проверялось негативным тестом на синтетическом YAML:
// guard, который молча перестал ловить, хуже отсутствующего.
func findInterpolationProblems(rel, content string, settable map[string]bool) []string {
	var problems []string
	service := ""
	assignedExpr := map[string]string{} // переменная -> выражение, присвоенное в этом сервисе
	for i, line := range strings.Split(content, "\n") {
		if composeServiceRe.MatchString(line) {
			service = composeServiceRe.FindStringSubmatch(line)[1]
			assignedExpr = map[string]string{}
			continue
		}
		if service == "" || strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		var name, value string
		found := false
		for _, re := range []*regexp.Regexp{composeEnvAssignRe, composeEnvMapRe} {
			if m := re.FindStringSubmatch(line); m != nil {
				name, value, found = m[1], m[2], true
				break
			}
		}
		if !found {
			continue
		}
		for _, occ := range envRefOccurrences(value) {
			dep := refName(occ)
			expr, seen := assignedExpr[dep]
			// Не наша забота, если имя не разобралось, это самоссылка
			// (правильный идиом чтения override оператора) или переменная в
			// этом сервисе не назначалась.
			//
			// ВАЖНО: «оператор может задать эту переменную» — НЕ основание
			// пропустить проверку. Именно на этом lint сначала не поймал
			// реальный дефект: SDWORKER_ADVERTISE_HOST как раз задаётся
			// оператором, но назначение сводится к ${BACKEND_HOST:-...}, а
			// ссылка — к литералу imageworker. Сравниваем сведённые значения.
			if dep == "" || dep == name || !seen {
				continue
			}
			diverges, scenario, want, got := divergeUnderSomeEnv(expr, occ, settable)
			if !diverges {
				continue
			}
			problems = append(problems, fmt.Sprintf(
				"%s:%d служба %q: %s=%q, а эта же строка читает %s\n"+
					"      при окружении [%s] назначение даёт %q, а ссылка %q — значения разъедутся;\n"+
					"      compose подставляет статически по .env/окружению и НЕ видит значение соседней строки.\n"+
					"      строка: %s",
				rel, i+1, service, dep, expr, occ,
				strings.Join(scenario, ", "), want, got,
				strings.TrimSpace(line)))
		}
		if _, exists := assignedExpr[name]; !exists {
			assignedExpr[name] = value
		}
	}
	return problems
}

// TestComposeInterpolation_NoSelfServiceReferences — ни один compose-файл не
// должен интерполировать переменную так, что её значение в контейнере и в
// выражении разъезжаются.
func TestComposeInterpolation_NoSelfServiceReferences(t *testing.T) {
	repoRoot := findRepoRoot(t)
	deploymentsDir := filepath.Join(repoRoot, "deployments")

	composeFiles, err := filepath.Glob(filepath.Join(deploymentsDir, "docker-compose*.yml"))
	if err != nil || len(composeFiles) == 0 {
		t.Skip("compose-файлы не найдены; пропускаем lint (CI может запускаться из подкаталога)")
	}
	sort.Strings(composeFiles)

	settable := operatorSettableNames(repoRoot)

	var problems []string
	for _, file := range composeFiles {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("чтение %s: %v", file, err)
		}
		rel, _ := filepath.Rel(repoRoot, file)
		problems = append(problems, findInterpolationProblems(rel, string(data), settable)...)
	}

	if len(problems) > 0 {
		t.Fatalf("найдены ломаные цепочки интерполяции в compose (%d):\n  %s\n\n"+
			"Лечение: вложить источник в то же выражение, например\n"+
			"  AGENT_PUBLIC_HOST=${SDWORKER_ADVERTISE_HOST:-${BACKEND_HOST:-imageworker}}",
			len(problems), strings.Join(problems, "\n  "))
	}
}

// TestComposeInterpolation_DetectsBrokenChain — негативный тест самого guard'а.
// Оба случая взяты с живой пары машин: (1) image-воркер регистрировался с host =
// имени контейнера, хотя BACKEND_HOST был задан; (2) токен балансера читался из
// переменной, назначение которой игнорировало override оператора.
func TestComposeInterpolation_DetectsBrokenChain(t *testing.T) {
	settable := map[string]bool{
		"BACKEND_HOST":             true,
		"SDWORKER_ADVERTISE_HOST":  true,
		"CPPWORKER_API_TOKEN":      true,
		"CPPWORKER_BALANCER_TOKEN": true,
		"AGENT_PORT":               true,
	}

	broken := "services:\n" +
		"  imageworker:\n" +
		"    environment:\n" +
		"      - SDWORKER_ADVERTISE_HOST=${SDWORKER_ADVERTISE_HOST:-${BACKEND_HOST:-imageworker}}\n" +
		"      - AGENT_PUBLIC_HOST=${SDWORKER_ADVERTISE_HOST:-imageworker}\n"
	if got := findInterpolationProblems("broken.yml", broken, settable); len(got) != 1 {
		t.Fatalf("ломаная цепочка AGENT_PUBLIC_HOST не найдена (получено %d замечаний): %v", len(got), got)
	}

	tokenBroken := "services:\n" +
		"  cppworker-gpu:\n" +
		"    environment:\n" +
		"      - CPPWORKER_BALANCER_TOKEN=${CPPWORKER_API_TOKEN:?set it}\n" +
		"      - BALANCER_TOKEN=${CPPWORKER_BALANCER_TOKEN:-${CPPWORKER_API_TOKEN:-}}\n"
	if got := findInterpolationProblems("token.yml", tokenBroken, settable); len(got) != 1 {
		t.Fatalf("ломаная цепочка токена не найдена (получено %d замечаний): %v", len(got), got)
	}

	// Правильные формы: самоссылка и вложенный источник ошибкой не считаются,
	// как и совпадающие fallback'и у соседних строк.
	fixed := "services:\n" +
		"  imageworker:\n" +
		"    environment:\n" +
		"      - SDWORKER_ADVERTISE_HOST=${SDWORKER_ADVERTISE_HOST:-${BACKEND_HOST:-imageworker}}\n" +
		"      - AGENT_PUBLIC_HOST=${SDWORKER_ADVERTISE_HOST:-${BACKEND_HOST:-imageworker}}\n" +
		"  cppworker-gpu:\n" +
		"    environment:\n" +
		"      - CPPWORKER_BALANCER_TOKEN=${CPPWORKER_BALANCER_TOKEN:-${CPPWORKER_API_TOKEN:?set it}}\n" +
		"      - BALANCER_TOKEN=${CPPWORKER_BALANCER_TOKEN:-${CPPWORKER_API_TOKEN:-}}\n" +
		"      - AGENT_PORT=${AGENT_PORT:-18032}\n" +
		"      - AGENT_PUBLIC_PORT=${AGENT_PORT:-18032}\n"
	if got := findInterpolationProblems("fixed.yml", fixed, settable); len(got) != 0 {
		t.Fatalf("исправленные формы не должны считаться ошибкой, получено: %v", got)
	}
}
