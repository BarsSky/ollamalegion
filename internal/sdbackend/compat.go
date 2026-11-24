// compat.go — R696 (2026-11-23): вердикт «сможет ли imageworker загрузить bundle».
//
// С СЕССИИ ФАЗЫ 4 (2026-11-24) ПРАВИЛА ЖИВУТ В `internal/imagemodel`. Причина
// переезда не косметическая: тот же вердикт обязан считать сервис ре-экспорта
// (`services/gguf-reexport`, цель image_bundle — «набор файлов → bundle»), а он
// не грузит llama.cpp и собирается без cgo/C-моста. Держать правила в пакете,
// который тянет cgo, значило бы либо запретить сервису переиспользовать R696
// (и раздвоить эвристику), либо заставить сервис собираться с сабмодулем.
//
// ЗДЕСЬ ОСТАЛИСЬ ТОЛЬКО ПРЕЖНИЕ ИМЕНА: воркеры, балансер, WebUI-контракт и
// тесты (internal/sdbackend/compat_test.go) обращаются к ним как раньше, и
// переименование ради переезда файла было бы правкой полрепозитория без пользы.
// Логика, поведение и тексты вердикта не менялись — это видно по тестам
// (compat_test.go зелёный без единой правки).
//
// Вердикты:
//
//	ready       — набор полный, движок получит корректные флаги;
//	incomplete  — семейство известно, но нет обязательных ролей (VAE/TE);
//	unsupported — движок заведомо не поднимет набор;
//	unknown     — по заголовкам судить не о чем.
package sdbackend

import (
	"ollama-loadbalancer/internal/imagemodel"
	"ollama-loadbalancer/pkg/types"
)

// Типы вердикта — псевдонимы типов общего пакета (методы BundleCompat, включая
// EngineModeLabel и CompatReasonLine, живут там же).
type (
	// BundleCompat — вердикт по набору файлов.
	BundleCompat = imagemodel.BundleCompat
	// CompatFile — один файл набора: роль в профиле и роль по содержимому.
	CompatFile = imagemodel.CompatFile
	// CompatRole — комплектность одной роли.
	CompatRole = imagemodel.CompatRole
)

const (
	CompatReady       = imagemodel.CompatReady
	CompatIncomplete  = imagemodel.CompatIncomplete
	CompatUnsupported = imagemodel.CompatUnsupported
	CompatUnknown     = imagemodel.CompatUnknown

	CompatEngineModel     = imagemodel.CompatEngineModel
	CompatEngineDiffusion = imagemodel.CompatEngineDiffusion
	CompatEngineUnknown   = imagemodel.CompatEngineUnknown
)

// BundleDir — каталог bundle'а по имени модели.
func BundleDir(modelsDir, name string) string { return imagemodel.BundleDir(modelsDir, name) }

// BuildBundleCompat — вердикт по набору файлов профиля (файлы читаются с диска).
func BuildBundleCompat(p *types.ImageModelProfile, bundleDir string) *BundleCompat {
	return imagemodel.BuildBundleCompat(p, bundleDir)
}

// VerifyBundleProbes — вердикт по УЖЕ снятым заголовкам файлов.
func VerifyBundleProbes(p *types.ImageModelProfile, probes map[string]*imagemodel.HFProbeResult, probeErrors map[string]string) *BundleCompat {
	return imagemodel.VerifyBundleProbes(p, probes, probeErrors)
}

// CompatRoleLabel — человеческая подпись роли (для вердикта и UI).
func CompatRoleLabel(role string) string { return imagemodel.CompatRoleLabel(role) }

// LoadBlockedMessage — текст ошибки API, когда загрузка остановлена до запуска
// движка (вердикт unsupported).
func LoadBlockedMessage(c *BundleCompat) string { return imagemodel.LoadBlockedMessage(c) }

// LoadBlockedHint — что делать оператору с заблокированной загрузкой.
func LoadBlockedHint() string { return imagemodel.LoadBlockedHint() }

// IsImageWeightFile — файл весов по расширению (экспорт фильтра синтеза).
func IsImageWeightFile(name string) bool { return imagemodel.IsImageWeightFile(name) }

// probeLocalCached — прежнее имя кэша заголовков (используется синтезом профиля
// в models.go).
func probeLocalCached(path string) (*imagemodel.HFProbeResult, error) {
	return imagemodel.ProbeLocalCached(path)
}

// isWeightFileName — прежнее имя того же фильтра для внутренних вызовов.
func isWeightFileName(name string) bool { return imagemodel.IsImageWeightFile(name) }
