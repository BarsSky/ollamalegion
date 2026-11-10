// Package version — единый источник сведений о сборке для всех бинарников
// OllamaLegion (балансер, cppworker, агент).
//
// ЗАЧЕМ. Оператору нужно видеть в ПЕРВОЙ шапке логов, ЧТО именно запущено:
// тег образа и коммит. Без этого невозможно отличить «исправление не сработало»
// от «в контейнере старый образ» — на живом стенде это стоило нескольких
// итераций: правка была в исходниках, а в контейнере крутилась предыдущая
// сборка, и по логам это никак не читалось.
//
// ИСТОЧНИКИ (в порядке приоритета, R486):
//  1. -ldflags "-X .../pkg/version.Commit=... -X .../pkg/version.BuildDate=..."
//     подставляет Dockerfile из build-args (см. docker/*/Dockerfile).
//  2. /app/identity.json — запечённый файл идентичности образа (R486). Он же
//     сверяется с бинарём на старте (VerifyR486), поэтому «в контейнере старый
//     образ» больше не выясняется по логам.
//  3. Переменные окружения APP_VERSION / VERSION / GIT_COMMIT / BUILD_DATE —
//     только для локальной разработки (`go run`), когда образа нет вовсе.
//  4. "unknown" — вместо пустоты, чтобы строка шапки не «съезжала».
package version

import (
	"fmt"
	"os"
	"strings"
)

// Значения по умолчанию переопределяются через -ldflags -X.
var (
	// Version — дистрибутивный тег (`r83-submodule-v23`).
	Version = ""
	// ImageTag — тег docker-образа, из которого собран бинарник.
	ImageTag = ""
	// Service — имя сервиса, для которого собран бинарник (balancer, cppworker,
	// agent, sdworker, imageworker). Сверяется с identity.json (R486).
	Service = ""
	// Commit — короткий SHA сборки.
	Commit = ""
	// CommitFull — полный SHA сборки.
	CommitFull = ""
	// CommitDate — дата коммита (ISO 8601), а не дата сборки.
	CommitDate = ""
	// BuildDate — дата сборки (ISO 8601).
	BuildDate = ""
	// Author — кто собрал образ (git user.name/email или BUILD_AUTHOR).
	Author = ""
	// LlamaCommit — коммит llama.cpp, слинкованного в образ.
	LlamaCommit = ""
	// LlamaDate — дата того же коммита llama.cpp.
	LlamaDate = ""
	// RPCProto — версия протокола RPC из ggml-rpc.h (RPC_PROTO_MAJOR.MINOR.PATCH).
	RPCProto = ""
)

// About — сведения о запущенном бинарнике.
type About struct {
	Service     string
	Version     string
	ImageTag    string
	Commit      string
	CommitFull  string
	CommitDate  string
	BuildDate   string
	Author      string
	LlamaCommit string
	LlamaDate   string
	RPCProto    string
}

// Get собирает сведения о сборке из ldflags, запечённого identity.json и env.
//
// R486: запечённые значения ПРИОРИТЕТНЕЕ env. Раньше env перекрывал всё, и один
// и тот же образ «честно сообщал» чужую версию — именно так образ с коммитом
// 850923d7 выдавал себя за нужный релиз.
func Get() About {
	id, _ := loadIdentityR486()
	a := About{
		Service:     firstNonEmpty(Service, id.Service),
		Version:     firstNonEmpty(Version, id.Version, os.Getenv("APP_VERSION"), os.Getenv("VERSION")),
		ImageTag:    firstNonEmpty(ImageTag, id.ImageTag),
		Commit:      firstNonEmpty(Commit, id.Commit, os.Getenv("GIT_COMMIT")),
		CommitFull:  firstNonEmpty(CommitFull, id.CommitFull),
		CommitDate:  firstNonEmpty(CommitDate, id.CommitDate),
		BuildDate:   firstNonEmpty(BuildDate, id.BuildDate, os.Getenv("BUILD_DATE")),
		Author:      firstNonEmpty(Author, id.Author),
		LlamaCommit: firstNonEmpty(LlamaCommit, id.LlamaCommit),
		LlamaDate:   firstNonEmpty(LlamaDate, id.LlamaDate),
		RPCProto:    firstNonEmpty(RPCProto, id.RPCProto),
	}
	// R486: «unknown» — это отсутствие значения, а не факт сборки. Раньше
	// ARG GIT_COMMIT=unknown доезжал до /api/version как build_commit:"unknown".
	a.Service = cleanStampR486(a.Service)
	a.Version = cleanStampR486(a.Version)
	a.ImageTag = cleanStampR486(a.ImageTag)
	a.Commit = cleanStampR486(a.Commit)
	a.CommitFull = cleanStampR486(a.CommitFull)
	a.CommitDate = cleanStampR486(a.CommitDate)
	a.BuildDate = cleanStampR486(a.BuildDate)
	a.Author = cleanStampR486(a.Author)
	a.LlamaCommit = cleanStampR486(a.LlamaCommit)
	a.LlamaDate = cleanStampR486(a.LlamaDate)
	a.RPCProto = cleanStampR486(a.RPCProto)
	return a
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
	out := make(map[string]interface{}, 12)
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
	// R486: остальное запечено в образ и публикуется отдельными полями. Поле
	// «какой коммит llama.cpp слинкован» и «какая версия протокола RPC» нужно
	// для разбора рассинхрона узлов раскладки без чтения env стенда.
	if a.CommitFull != "" {
		out["build_commit_full"] = a.CommitFull
	}
	if a.CommitDate != "" {
		out["build_commit_date"] = a.CommitDate
	}
	if a.Author != "" {
		out["build_author"] = a.Author
	}
	if a.ImageTag != "" {
		out["build_image_tag"] = a.ImageTag
	}
	if a.Service != "" {
		out["build_service"] = a.Service
	}
	if a.LlamaCommit != "" {
		out["llama_commit"] = a.LlamaCommit
	}
	if a.LlamaDate != "" {
		out["llama_date"] = a.LlamaDate
	}
	if a.RPCProto != "" {
		out["rpc_proto"] = a.RPCProto
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
