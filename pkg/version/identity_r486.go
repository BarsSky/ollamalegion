// identity_r486.go — R486 (2026-11-10): идентичность сборки ЗАПЕКАЕТСЯ в образ,
// а не приходит из env.
//
// ЖИВОЙ ДЕФЕКТ, РАДИ КОТОРОГО ЭТО НАПИСАНО. Образы балансера и воркера были
// собраны и проштампованы ПРЕДЫДУЩИМ коммитом: в deployments/.env лежали
// BALANCER_GIT_COMMIT=850923d7 / CPPWORKER_GIT_COMMIT=361490ec, scripts/
// release-all.ps1 вообще не передавал GIT_COMMIT, поэтому в Dockerfile
// срабатывал дефолт ARG GIT_COMMIT=unknown, а pkg/version молча принимал
// «unknown». Оператор узнал об этом случайно — по /api/version после выкатки.
//
// ПОЧЕМУ env — неправильный источник. Значение в .env это НАМЕРЕНИЕ оператора
// («какой коммит я думаю, что собираю»), а не свойство образа. Намерение
// расходится с фактом молча, потому что ни одна проверка их не сверяет:
//   - scripts/check-image-tags.ps1 сравнивает только строки `image:`;
//   - release-manifest.json берёт gitCommit из репозитория, а не из образа;
//   - pkg/version.Get() принимает пустое/«unknown» без единого слова.
//
// ЧТО ЗДЕСЬ. Единственный запечённый манифест сборки `/app/identity.json`
// (кладётся в КАЖДЫЙ образ, включая nginx-овский webui и imageworker, где
// Go-бинарника с -ldflags нет) и проверка старта: значения из -ldflags обязаны
// совпадать с файлом, иначе контейнер не поднимается. Это ловит ровно тот
// класс ошибки — «в образе чужой/устаревший штамп».
//
// Порядок источников в Get() теперь такой (см. version.go):
//
//	-ldflags  >  /app/identity.json  >  APP_VERSION/VERSION/GIT_COMMIT/BUILD_DATE
//
// env остался только для локальной разработки (`go run ./cmd/balancer`): в
// образе все ключевые поля приходят запечёнными, и env их не перекрывает —
// иначе один и тот же образ снова мог бы «честно сообщать» чужую версию.
package version

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
)

// identityDefaultPathR486 — путь внутри образа. Переопределяется
// IDENTITY_FILE (нужно тестам и локальному запуску вне контейнера).
const identityDefaultPathR486 = "/app/identity.json"

// identityFileR486 — формат /app/identity.json. Ключи стабильные: файл читают
// и Go-бинарники, и entrypoint webui, и человек через docker cp.
type identityFileR486 struct {
	Service     string `json:"service"`
	Version     string `json:"version"`
	ImageTag    string `json:"image_tag"`
	Commit      string `json:"commit"`
	CommitFull  string `json:"commit_full"`
	CommitDate  string `json:"commit_date"`
	BuildDate   string `json:"build_date"`
	Author      string `json:"author"`
	LlamaCommit string `json:"llama_commit"`
	LlamaDate   string `json:"llama_date"`
	RPCProto    string `json:"rpc_proto"`
}

var (
	identityOnceR486     sync.Once
	identityR486         identityFileR486
	identityFoundR486    bool
	identityUsedPathR486 string
)

// IdentityPathR486 — путь, по которому реально искали файл (для текстов ошибок).
func IdentityPathR486() string {
	loadIdentityR486()
	return identityUsedPathR486
}

// loadIdentityR486 читает файл один раз за процесс: он не меняется.
func loadIdentityR486() (identityFileR486, bool) {
	identityOnceR486.Do(func() {
		path := strings.TrimSpace(os.Getenv("IDENTITY_FILE"))
		if path == "" {
			path = identityDefaultPathR486
		}
		identityUsedPathR486 = path
		data, err := os.ReadFile(path)
		if err != nil {
			return
		}
		var id identityFileR486
		if err := json.Unmarshal(data, &id); err != nil {
			return
		}
		identityR486 = id.normalizedR486()
		identityFoundR486 = true
	})
	return identityR486, identityFoundR486
}

// resetIdentityCacheR486 — только для тестов: файл ищется заново.
func resetIdentityCacheR486() {
	identityOnceR486 = sync.Once{}
	identityR486 = identityFileR486{}
	identityFoundR486 = false
	identityUsedPathR486 = ""
}

func (id identityFileR486) normalizedR486() identityFileR486 {
	id.Service = cleanStampR486(id.Service)
	id.Version = cleanStampR486(id.Version)
	id.ImageTag = cleanStampR486(id.ImageTag)
	id.Commit = cleanStampR486(id.Commit)
	id.CommitFull = cleanStampR486(id.CommitFull)
	id.CommitDate = cleanStampR486(id.CommitDate)
	id.BuildDate = cleanStampR486(id.BuildDate)
	id.Author = cleanStampR486(id.Author)
	id.LlamaCommit = cleanStampR486(id.LlamaCommit)
	id.LlamaDate = cleanStampR486(id.LlamaDate)
	id.RPCProto = cleanStampR486(id.RPCProto)
	return id
}

// cleanStampR486 — «unknown»/пусто/пробелы не считаются значением: поле с
// текстом «unknown» выглядит как факт и уже один раз стоило разбора.
func cleanStampR486(v string) string {
	v = strings.TrimSpace(v)
	if strings.EqualFold(v, "unknown") {
		return ""
	}
	return v
}

// BakedIdentityR486 — запечённая идентичность (пустая карта, если образа нет).
// Используется диагностикой, чтобы архив нёс файл, а не пересказ env.
func BakedIdentityR486() map[string]string {
	id, ok := loadIdentityR486()
	if !ok {
		return nil
	}
	out := map[string]string{
		"path": identityUsedPathR486,
	}
	put := func(k, v string) {
		if v != "" {
			out[k] = v
		}
	}
	put("service", id.Service)
	put("version", id.Version)
	put("image_tag", id.ImageTag)
	put("commit", id.Commit)
	put("commit_full", id.CommitFull)
	put("commit_date", id.CommitDate)
	put("build_date", id.BuildDate)
	put("author", id.Author)
	put("llama_commit", id.LlamaCommit)
	put("llama_date", id.LlamaDate)
	put("rpc_proto", id.RPCProto)
	return out
}

// VerifyR486 — сверка запечённой идентичности с самим бинарём.
//
// Правила:
//   - файла нет → это локальная сборка (go run / go build без образа), не ошибка;
//   - файл есть, а бинарник собран без -ldflags → ошибка: идентичность недоказуема;
//   - commit бинаря и файла расходятся → ошибка: в образ положили чужой бинарь;
//   - service в файле и у процесса расходятся → ошибка: образ собран для другого
//     сервиса (например, бинарь воркера в образе балансера).
func VerifyR486(service string) error {
	id, ok := loadIdentityR486()
	if !ok {
		return nil
	}
	var problems []string
	if cleanStampR486(Commit) == "" {
		problems = append(problems, "бинарник собран без штампа: -X pkg/version.Commit пуст, а образ несёт "+identityUsedPathR486)
	} else if id.Commit != "" && !strings.EqualFold(id.Commit, cleanStampR486(Commit)) {
		problems = append(problems, fmt.Sprintf("commit бинаря %q != commit образа %q", cleanStampR486(Commit), id.Commit))
	}
	if cleanStampR486(BuildDate) == "" && id.BuildDate != "" {
		problems = append(problems, "бинарник собран без -X pkg/version.BuildDate, а образ несёт дату сборки")
	}
	if id.Service != "" && service != "" && !strings.EqualFold(id.Service, service) {
		problems = append(problems, fmt.Sprintf("образ собран для сервиса %q, запускается %q", id.Service, service))
	}
	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("R486: идентичность образа не совпадает с бинарём (%s): %s",
		identityUsedPathR486, strings.Join(problems, "; "))
}

// MustVerifyR486 — проверка на старте сервиса.
//
// Почему fatal, а не warn: «в контейнере старый/чужой образ» — ровно тот дефект,
// который раньше жил незамеченным и выяснялся по логам постфактум. Контейнер,
// который не может доказать свою идентичность, не должен принимать запросы.
//
// os.Exit осознанно: пакет не зависит от логгеров сервисов (у них разные), а
// сообщение обязано попасть в stderr до любой инициализации.
func MustVerifyR486(service string) {
	if err := VerifyR486(service); err != nil {
		fmt.Fprintf(os.Stderr, "FATAL: %v\n", err)
		os.Exit(1)
	}
}
