// Package version — единый источник сведений о сборке для всех бинарников
// OllamaLegion (балансер, cppworker, агент).
//
// ЗАЧЕМ. Оператору нужно видеть в ПЕРВОЙ шапке логов, ЧТО именно запущено:
// тег образа и коммит. Без этого невозможно отличить «исправление не сработало»
// от «в контейнере старый образ» — на живом стенде это стоило нескольких
// итераций: правка была в исходниках, а в контейнере крутилась предыдущая
// сборка, и по логам это никак не читалось.
//
// ИСТОЧНИКИ (в порядке приоритета):
//  1. -ldflags "-X .../pkg/version.Commit=... -X .../pkg/version.BuildDate=..."
//     подставляет Dockerfile из build-args (см. docker/*/Dockerfile).
//  2. Переменные окружения APP_VERSION / GIT_COMMIT / BUILD_DATE — их
//     выставляет docker-compose или entrypoint. Так один и тот же образ,
//     запущенный с разными тегами, честно сообщает свой тег без пересборки.
//  3. "unknown" — вместо пустоты, чтобы строка шапки не «съезжала».
package version

import (
	"fmt"
	"os"
	"strings"
)

// Значения по умолчанию переопределяются через -ldflags -X.
var (
	// Version — дистрибутивный тег (`r83-submodule-v23`), обычно приходит из env.
	Version = ""
	// Commit — короткий SHA сборки.
	Commit = ""
	// BuildDate — дата сборки (ISO 8601).
	BuildDate = ""
)

// About — сведения о запущенном бинарнике.
type About struct {
	Version   string
	Commit    string
	BuildDate string
}

// Get собирает сведения о сборке из ldflags и окружения.
func Get() About {
	return About{
		Version:   firstNonEmpty(Version, os.Getenv("APP_VERSION"), os.Getenv("VERSION")),
		Commit:    firstNonEmpty(Commit, os.Getenv("GIT_COMMIT")),
		BuildDate: firstNonEmpty(BuildDate, os.Getenv("BUILD_DATE")),
	}
}

// String — одна строка для шапки логов: "r83-submodule-v23 (commit 5731414, 2026-09-28)".
// Неизвестные части опускаются, чтобы не печатать "unknown (commit unknown)".
func (a About) String() string {
	parts := make([]string, 0, 2)
	if a.Commit != "" {
		parts = append(parts, "commit "+a.Commit)
	}
	if a.BuildDate != "" {
		parts = append(parts, a.BuildDate)
	}
	ver := a.Version
	if ver == "" {
		ver = "unknown"
	}
	if len(parts) == 0 {
		return ver
	}
	return fmt.Sprintf("%s (%s)", ver, strings.Join(parts, ", "))
}

// Fields — сведения о сборке как поля JSON-ответа (R448).
//
// ЗАЧЕМ ОТДЕЛЬНЫЙ МЕТОД. Строка String() годится для шапки логов, но её нельзя
// разобрать клиенту: WebUI и внешний монитор должны видеть КОММИТ отдельным
// полем, иначе «работает ли на стенде именно этот код» снова проверяется только
// через docker. Ключи одинаковые у балансера и cppworker — иначе один и тот же
// вопрос требовал бы двух правил.
//
// Пустые значения не публикуются: поле «unknown» хуже отсутствующего поля,
// потому что выглядит как факт.
func (a About) Fields() map[string]interface{} {
	out := make(map[string]interface{}, 4)
	if a.Commit != "" {
		out["build_commit"] = a.Commit
	}
	if a.BuildDate != "" {
		out["build_date"] = a.BuildDate
	}
	if a.Version != "" {
		out["build_version"] = a.Version
	}
	// build — та же строка, что в шапке логов: «что запущено» одним взглядом.
	if s := a.String(); s != "" && s != "unknown" {
		out["build"] = s
	}
	return out
}

// firstNonEmpty возвращает первое непустое значение (пробелы обрезаются).
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if trimmed := strings.TrimSpace(v); trimmed != "" {
			return trimmed
		}
	}
	return ""
}
