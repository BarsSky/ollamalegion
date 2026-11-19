// image_tool_target.go — R637 (2026-11-19): выбор цели инструмента: imageToolTarget, imageToolTargetFor, imageToolDecision.
//
// ЗАЧЕМ ВЫНЕСЕНО. `image_tool.go` держал 1671 строку: настройки и цель
// инструмента, схему для клиента, разбор вызовов, загрузку модели, генерацию и
// помощники. Здесь — один из слоёв; поведение не менялось.
package balancer

import (
	"context"
	"ollama-loadbalancer/internal/i18n"
	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

// imageToolTarget — бэкенд и модель, которыми балансер исполнит вызов.
type imageToolTarget struct {
	BackendID string
	Model     string
	// Models — все модели кластера (enum в схеме инструмента).
	Models []string
	// Families — семейства моделей кластера (enum параметра family у
	// list_image_models).
	Families []string
	// AllowLoad — вызову разрешено поднимать не загруженную модель.
	AllowLoad bool
	// catalog — снимок каталога, из которого выбрана модель. Нужен, чтобы
	// выбор модели в generateImageForTool не переспрашивал кластер.
	catalog *ImageCatalog
}

// imageToolTargetFor — гейт доступности: nil, если исполнять некому.
//
// ПОЧЕМУ ЧЕРЕЗ ensureFreshFor, А НЕ metricsSnapshot: решение принимается
// per-request, и протухший снимок стоит дорого в обе стороны — объявим
// инструмент по выгруженной модели и получим ошибку уже после вызова. Догрузка
// ленивая и переиспользует один опрос для подряд идущих запросов (staleAfter=5s).
//
// R85 (2026-10-06) — ГЕЙТ ИЗМЕНИЛСЯ. Было: здоровый image_cpp + ЗАГРУЖЕННАЯ
// модель. Стало: здоровый image_cpp + ЕСТЬ ЧТО ГРУЗИТЬ (в снимке есть модели) при
// LB_IMAGE_TOOL_ALLOW_LOAD=on. Причина: требование «модель уже в VRAM» не давало
// текстовой модели поднять нужную модель под запрос пользователя, а без
// каталога она вообще не знала, из чего выбирать.
//
// При ALLOW_LOAD=off правило прежнее (нужна загруженная модель) — это ровно то
// поведение, которое уже описано в документации и проверено тестами.
//
// Тонкая обёртка над imageToolDecision: здесь причина отказа не нужна, но нужна
// там, где решение логируется (см. logImageToolDecision).
func (p *Proxy) imageToolTargetFor(ctx context.Context) *imageToolTarget {
	target, _ := p.imageToolDecision(ctx)
	return target
}

// Причины отказа объявить инструмент (R86, 2026-10-06). Нужны, чтобы ответить
// на вопрос «почему модель не видит инструменты» ОДНОЙ строкой лога, а не
// раскопками исходников: раньше отказ жил на debug-уровне без причины.
const (
	imageToolSkipDisabled     = "tool_disabled"
	imageToolSkipNoBackend    = "no_image_backend"
	imageToolSkipNoModels     = "no_models_to_load"
	imageToolSkipAllowLoadOff = "no_loaded_model_and_allow_load_off"
	imageToolSkipClientNone   = "client_tool_choice_none"
	imageToolSkipClientOwns   = "client_owns_tool"
	// imageToolSkipNoClientTools — R141: режим auto, а клиент не прислал ни
	// одного tools[] — объявлять нечего и незачем (см. imageToolEnvInject).
	imageToolSkipNoClientTools = "auto_mode_client_sent_no_tools"
	// R216: LB_IMAGE_TOOL_INJECT=off — инструмент не объявляем никогда.
	imageToolSkipInjectOff = "inject_mode_off"
)

// imageToolDecision — гейт доступности ВМЕСТЕ с причиной отказа.
//
// Возвращает (nil, "") если инструмент объявлять можно; иначе (nil, причина) —
// причина для лога (см. imageToolSkipReason).
func (p *Proxy) imageToolDecision(ctx context.Context) (*imageToolTarget, string) {
	if p == nil {
		return nil, imageToolSkipDisabled
	}
	cfg := p.imageToolSettings()
	if !cfg.Enabled {
		return nil, imageToolSkipDisabled
	}
	if p.imageResources() == nil {
		return nil, imageToolSkipDisabled
	}
	states := p.filterBackendsByType(types.BackendTypeImage)
	if len(states) == 0 {
		logger.Get().Debugw(i18n.T("balancer.image_tool.no_backend_debug"))
		return nil, imageToolSkipNoBackend
	}

	// Каталог — единственный источник и имён, и состояний: он собран из тех же
	// снимков, что судит VRAM-гейт, поэтому «каталог показывает not_loaded» и
	// «гейт откажет без загрузки» не могут разойтись.
	catalog := p.ImageCatalogFor(ctx)

	var models, families []string
	var target *imageToolTarget
	for _, m := range catalog.Models {
		if m.Name != "" && !containsString(models, m.Name) {
			models = append(models, m.Name)
		}
		if m.Family != "" && !containsString(families, m.Family) {
			families = append(families, m.Family)
		}
	}

	// Приоритет — ЗАГРУЖЕННАЯ модель: генерация по ней не требует ни загрузки,
	// ни ожидания. Иначе (и только при разрешённой автозагрузке) — первая модель
	// каталога: её поднимет executeOneImageToolCall.
	for _, m := range catalog.Models {
		if m.Loaded {
			target = &imageToolTarget{BackendID: m.BackendID, Model: m.Name}
			break
		}
	}
	if target == nil && cfg.AllowLoad {
		if len(catalog.Models) > 0 {
			target = &imageToolTarget{BackendID: catalog.Models[0].BackendID, Model: catalog.Models[0].Name}
		}
	}

	if target == nil {
		if !cfg.AllowLoad {
			// Прежнее поведение: без загруженной модели исполнять нечего, потому
			// что LB_IMAGE_TOOL_ALLOW_LOAD=off запрещает поднимать её из вызова.
			logger.Get().Debugw(i18n.T("balancer.image_tool.no_loaded_model_debug"),
				"env", imageToolEnvAllowLoad)
			return nil, imageToolSkipAllowLoadOff
		}
		logger.Get().Debugw(i18n.T("balancer.image_tool.no_models_debug"),
			"confident_backends", len(catalog.Backends))
		return nil, imageToolSkipNoModels
	}

	sortStringsInPlace(models)
	sortStringsInPlace(families)
	target.Models = models
	target.Families = families
	target.AllowLoad = cfg.AllowLoad
	target.catalog = catalog
	return target, ""
}
