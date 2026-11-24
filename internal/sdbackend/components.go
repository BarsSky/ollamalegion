// components.go — R710 (2026-11-24): КАТАЛОГ базовых компонентов семейства
// (VAE / text encoder / CLIP / …): «чего не хватает» → «откуда это взять».
//
// ЗАЧЕМ. Вердикт совместимости (compat.go, R696) честно говорит «набор
// неполный: не хватает VAE, text encoder (LLM)», но у оператора не было ни
// одного способа это исправить из интерфейса. Живой случай (скриншоты
// пользователя): репозитории `Qwen-Image-Edit-2511` и
// `Qwen-Image-2.1-Turbo-Uncensored` содержат ТОЛЬКО diffusion-файлы, а подсказка
// UI советовала «добавьте файлами из другого репозитория (например VAE
// ae.safetensors)» — то есть отправляла оператора искать файл вручную, при том
// что страница не умела ни открыть другой репозиторий, ни добавить из него один
// файл к уже скачанному набору.
//
// ПОЧЕМУ ДАННЫЕ, А НЕ ЛОГИКА UI. Каталог — это ЗНАНИЕ О МИРЕ (какой репозиторий
// содержит стандартный VAE для qwen_image), а не про верстку. Если бы таблица
// жила в JS, её пришлось бы дублировать для страницы HF и для «Моделей на
// диске», а воркер (который скачивает файл) не смог бы проверить, что роль
// действительно рекомендована семейству. Здесь она живёт рядом с вердиктом,
// который её и требует.
//
// ЧТО ПРОВЕРЕНО И КАК. Каждая запись — реальный репозиторий и реальный файл,
// проверенные запросом к HF API (`/api/models/{repo}/tree/main?recursive=true`,
// тот же tree-API, что использует воркер в GET /api/hf/files) на дату правки;
// размер — фактический размер файла из ответа HF, переведённый в МиБ.
// `License` — cardData.license из `/api/models/{repo}` там, где он объявлен;
// `Note` — то, что нельзя выразить полем: gated-репозиторий (нужен HF-токен с
// принятой лицензией), «тот же файл, что у gated-оригинала», «для 2.1/2511
// нужен именно Qwen3-VL, а не Qwen2.5-VL» и т.п.
//
// ГРАНИЦЫ (осознанные):
//   - каталог НЕ «угадывает» семейство: неизвестное семейство → пустой каталог,
//     UI это честно показывает и оставляет ручной путь (`Выбрать из другого
//     репозитория…`). Пустой каталог — нормальный ответ, а не ошибка;
//   - каталог НЕ скачивает и НЕ проверяет доступность по сети на каждый запрос:
//     GET-ручка отдаёт данные мгновенно (она нужна в момент рендера вердикта), а
//     проверку существования делает POST установки через HF-проб (hf.go);
//   - `default` — «стандартный компонент семейства», а НЕ «единственно верный»:
//     для каждой роли в таблице может быть несколько записей, и UI предлагает
//     кнопку «по умолчанию» ровно для одной.
package sdbackend

import (
	"sort"
	"strings"

	"ollama-loadbalancer/pkg/types"
)

// ImageComponent — один рекомендуемый компонент семейства.
//
// JSON-контракт GET /api/image/components/catalog (и
// GET /api/v1/image/components/catalog на плоскости балансера). Имена полей
// фиксированы: их читает webui/js/modules/image-models-hf.js.
type ImageComponent struct {
	Family string `json:"family"` // qwen_image|flux|flux2|sd3|z_image|chroma|sdxl|sd15
	// Role — роль файла в профиле bundle (см. types.ImageFileRole*). Роль
	// diffusion в каталоге отсутствует сознательно: её файл — это сама модель,
	// и «добавить diffusion к набору» означало бы подменить/перезаписать его.
	Role  string `json:"role"`
	Label string `json:"label"` // человеческая подпись: "VAE (Qwen-Image)"
	Repo  string `json:"repo"`  // репозиторий HF ("Comfy-Org/Qwen-Image_ComfyUI")
	// Files — пути ФАЙЛОВ внутри репозитория (могут быть с подкаталогами:
	// "split_files/vae/qwen_image_vae.safetensors"). Скачиваются в каталог
	// bundle под своими basename — ровно так же, как bundle-загрузка.
	Files []string `json:"files"`
	// SizeMB — фактический суммарный размер файлов в МиБ (HF отдаёт байты,
	// переводим в МиБ и округляем вверх: «335 МБ» понятнее оператору, чем
	// 335304388 байт, и не занижено относительно реального объёма).
	SizeMB  int64 `json:"sizeMb"`
	Default bool  `json:"default"` // стандартный компонент семейства для этой роли
	// Note — то, что оператор обязан знать ДО нажатия: gated-репозиторий,
	// происхождение файла (переупаковка/официальный экспорт), для какой версии
	// семейства он подходит.
	Note string `json:"note,omitempty"`
	// License — SPDX-подобный id лицензии из карточки HF ("apache-2.0", "mit",
	// "openrail++", "other"). Пусто = лицензия не объявлена в карточке.
	License string `json:"license,omitempty"`
}

// componentCatalog — «семейство → рекомендуемые компоненты по ролям».
//
// ПОРЯДОК внутри семейства = порядок предпочтения (первая запись роли — та,
// которую UI предложит кнопкой «по умолчанию», если у неё Default=true).
//
// Источники и проверки (2026-11-24, HF tree API):
//
//	qwen_image — Comfy-Org/Qwen-Image_ComfyUI: split_files/vae/qwen_image_vae.safetensors
//	             (242 МиБ) и split_files/text_encoders/qwen_2.5_vl_7b_fp8_scaled/
//	             nvfp4/bf16; abenzerps/Qwen-Image-2.1-Uncensored-GGUF:
//	             vae/qwen_image_2.1_vae_bf16.safetensors (644 МиБ) и
//	             text_encoders/qwen3vl_8b_int8_convrot.safetensors (8.7 ГиБ) —
//	             ИМЕННО этот набор прошёл живой прогон docs/features/
//	             image-generation.ru.md §16.10.
//	flux       — lodestones/Chroma:ae.safetensors — тот же FLUX-VAE (тот же
//	             размер 335 304 388 Б, что у black-forest-labs/FLUX.1-dev, но
//	             репозиторий НЕ gated); comfyanonymous/flux_text_encoders:
//	             clip_l.safetensors (234.7 МиБ) и t5xxl_fp8_e4m3fn.safetensors;
//	             city96/t5-v1_1-xxl-encoder-gguf — квантованный T5, который
//	             sd.cpp грузит как --t5xxl.
//	flux2      — Comfy-Org/flux2-dev: split_files/vae/flux2-vae.safetensors и
//	             split_files/text_encoders/mistral_3_small_flux2_*.safetensors.
//	sd3        — stabilityai/stable-diffusion-3.5-large: vae/, text_encoders/
//	             clip_l|clip_g|t5xxl_fp8_e4m3fn (репозиторий gated → Note).
//	z_image    — Comfy-Org/z_image_turbo: split_files/vae/ae.safetensors и
//	             split_files/text_encoders/qwen_3_4b(.fp8_mixed).safetensors.
//	chroma     — lodestones/Chroma:ae.safetensors + T5-XXL (city96 GGUF).
//	sdxl/sd15  — VAE для «починить декодер» (stabilityai/sdxl-vae,
//	             stabilityai/sd-vae-ft-mse); этих ролей вердикт у all-in-one
//	             семейств не требует, но каталог отвечает и на ручной запрос.
var componentCatalog = map[string][]ImageComponent{
	"qwen_image": {
		{
			Role: "vae", Label: "VAE (Qwen-Image)", Repo: "Comfy-Org/Qwen-Image_ComfyUI",
			Files:   []string{"split_files/vae/qwen_image_vae.safetensors"},
			SizeMB:  242,
			Default: true,
			License: "apache-2.0",
			Note:    "Официальный экспорт VAE Qwen-Image для ComfyUI; подходит всему семейству (Qwen-Image, Qwen-Image-Edit 2509/2511, Qwen-Image 2512).",
		},
		{
			Role: "vae", Label: "VAE (Qwen-Image 2.1, bf16)", Repo: "abenzerps/Qwen-Image-2.1-Uncensored-GGUF",
			Files:   []string{"vae/qwen_image_2.1_vae_bf16.safetensors"},
			SizeMB:  645,
			License: "other",
			Note:    "bf16-VAE из набора Qwen-Image 2.1: этим файлом проверен живой прогон sd.cpp (docs/features/image-generation.ru.md §16.10). Неофициальная переупаковка — проверьте лицензию базовой модели.",
		},
		{
			Role: "llm", Label: "text encoder Qwen3-VL 8B (int8)", Repo: "abenzerps/Qwen-Image-2.1-Uncensored-GGUF",
			Files:   []string{"text_encoders/qwen3vl_8b_int8_convrot.safetensors"},
			SizeMB:  8711,
			Default: true,
			License: "other",
			Note:    "Текстовый энкодер Qwen-Image 2.1/2511 (флаг --llm). Именно этот файл проверен живым прогоном §16.10; bf16-версия того же репозитория весит 16.7 ГиБ. Неофициальная переупаковка — проверьте лицензию базовой модели.",
		},
		{
			Role: "llm", Label: "text encoder Qwen2.5-VL 7B (fp8)", Repo: "Comfy-Org/Qwen-Image_ComfyUI",
			Files:   []string{"split_files/text_encoders/qwen_2.5_vl_7b_fp8_scaled.safetensors"},
			SizeMB:  8745,
			License: "apache-2.0",
			Note:    "Энкодер первой Qwen-Image (2505). Для Qwen-Image 2.1 и Edit 2511 нужен Qwen3-VL — движок ответит «Conditioner model tensor … not in model metadata», если подставить этот файл.",
		},
	},
	"flux": {
		{
			Role: "vae", Label: "VAE FLUX (ae)", Repo: "lodestones/Chroma",
			Files:   []string{"ae.safetensors"},
			SizeMB:  320,
			Default: true,
			License: "apache-2.0",
			Note:    "Тот же ae.safetensors, что в black-forest-labs/FLUX.1-dev (совпадает размер 335 304 388 Б), но репозиторий не gated — скачивается без токена.",
		},
		{
			Role: "clip_l", Label: "CLIP-L", Repo: "comfyanonymous/flux_text_encoders",
			Files:   []string{"clip_l.safetensors"},
			SizeMB:  235,
			Default: true,
			License: "apache-2.0",
		},
		{
			Role: "t5xxl", Label: "T5-XXL (Q4_K_M, GGUF)", Repo: "city96/t5-v1_1-xxl-encoder-gguf",
			Files:   []string{"t5-v1_1-xxl-encoder-Q4_K_M.gguf"},
			SizeMB:  2762,
			Default: true,
			License: "apache-2.0",
			Note:    "sd.cpp грузит --t5xxl только квантованный GGUF; Q4_K_M — компромисс качество/размер. В том же репозитории Q3_K_S (2.0 ГиБ) и Q8_0 (4.8 ГиБ).",
		},
		{
			Role: "t5xxl", Label: "T5-XXL (fp8_e4m3fn, safetensors)", Repo: "comfyanonymous/flux_text_encoders",
			Files:   []string{"t5xxl_fp8_e4m3fn.safetensors"},
			SizeMB:  4667,
			License: "apache-2.0",
			Note:    "safetensors-вариант T5-XXL для движка, если квантованный GGUF не нужен.",
		},
		{
			Role: "clip_vision", Label: "CLIP-vision SigLIP (384)", Repo: "Comfy-Org/sigclip_vision_384",
			Files:   []string{"sigclip_vision_patch14_384.safetensors"},
			SizeMB:  817,
			License: "apache-2.0",
			Note:    "Флаг --clip_vision: нужен только для редактирования по референсу (FLUX Kontext/IP-Adapter). Обычной генерации не требуется.",
		},
	},
	"flux2": {
		{
			Role: "vae", Label: "VAE FLUX.2", Repo: "Comfy-Org/flux2-dev",
			Files:   []string{"split_files/vae/flux2-vae.safetensors"},
			SizeMB:  321,
			Default: true,
			License: "other",
			Note:    "VAE именно FLUX.2 (не ae.safetensors от FLUX.1).",
		},
		{
			Role: "llm", Label: "text encoder Mistral Small 3 (fp8)", Repo: "Comfy-Org/flux2-dev",
			Files:   []string{"split_files/text_encoders/mistral_3_small_flux2_fp8.safetensors"},
			SizeMB:  16796,
			Default: true,
			License: "other",
			Note:    "Энкодер FLUX.2 весит 16.8 ГиБ (bf16 — 33 ГиБ): скачивание долгое, место на диске проверьте заранее.",
		},
	},
	"sd3": {
		{
			Role: "vae", Label: "VAE SD3.5", Repo: "stabilityai/stable-diffusion-3.5-large",
			Files:   []string{"vae/diffusion_pytorch_model.safetensors"},
			SizeMB:  160,
			Default: true,
			License: "other",
			Note:    "Репозиторий gated: скачивание требует HF-токена с принятой лицензией Stability AI.",
		},
		{
			Role: "clip_l", Label: "CLIP-L", Repo: "stabilityai/stable-diffusion-3.5-large",
			Files:   []string{"text_encoders/clip_l.safetensors"},
			SizeMB:  235,
			Default: true,
			License: "other",
			Note:    "Репозиторий gated (нужен HF-токен). Тот же файл без gating: comfyanonymous/flux_text_encoders:clip_l.safetensors.",
		},
		{
			Role: "clip_g", Label: "CLIP-G", Repo: "stabilityai/stable-diffusion-3.5-large",
			Files:   []string{"text_encoders/clip_g.safetensors"},
			SizeMB:  1325,
			Default: true,
			License: "other",
			Note:    "Репозиторий gated (нужен HF-токен).",
		},
		{
			Role: "t5xxl", Label: "T5-XXL (fp8_e4m3fn)", Repo: "stabilityai/stable-diffusion-3.5-large",
			Files:   []string{"text_encoders/t5xxl_fp8_e4m3fn.safetensors"},
			SizeMB:  4667,
			Default: true,
			License: "other",
			Note:    "Репозиторий gated (нужен HF-токен).",
		},
	},
	"z_image": {
		{
			Role: "vae", Label: "VAE Z-Image (ae)", Repo: "Comfy-Org/z_image_turbo",
			Files:   []string{"split_files/vae/ae.safetensors"},
			SizeMB:  320,
			Default: true,
			License: "apache-2.0",
		},
		{
			Role: "llm", Label: "text encoder Qwen3-4B", Repo: "Comfy-Org/z_image_turbo",
			Files:   []string{"split_files/text_encoders/qwen_3_4b.safetensors"},
			SizeMB:  7672,
			Default: true,
			License: "apache-2.0",
			Note:    "bf16-энкодер (флаг --llm). В том же репозитории fp8_mixed (5.4 ГиБ) — меньше и быстрее качается.",
		},
		{
			Role: "llm", Label: "text encoder Qwen3-4B (fp8_mixed)", Repo: "Comfy-Org/z_image_turbo",
			Files:   []string{"split_files/text_encoders/qwen_3_4b_fp8_mixed.safetensors"},
			SizeMB:  5371,
			License: "apache-2.0",
		},
	},
	"chroma": {
		{
			Role: "vae", Label: "VAE Chroma (ae)", Repo: "lodestones/Chroma",
			Files:   []string{"ae.safetensors"},
			SizeMB:  320,
			Default: true,
			License: "apache-2.0",
		},
		{
			Role: "t5xxl", Label: "T5-XXL (Q4_K_M, GGUF)", Repo: "city96/t5-v1_1-xxl-encoder-gguf",
			Files:   []string{"t5-v1_1-xxl-encoder-Q4_K_M.gguf"},
			SizeMB:  2762,
			Default: true,
			License: "apache-2.0",
		},
	},
	"sdxl": {
		{
			Role: "vae", Label: "VAE SDXL (исправленный)", Repo: "stabilityai/sdxl-vae",
			Files:   []string{"diffusion_pytorch_model.safetensors"},
			SizeMB:  320,
			Default: true,
			License: "mit",
			Note:    "Отдельный VAE SDXL нужен, когда чекпойнт даёт мыло/артефакты декодера: all-in-one SDXL иначе берёт VAE из чекпойнта.",
		},
	},
	"sd15": {
		{
			Role: "vae", Label: "VAE SD1.5 (ft-mse)", Repo: "stabilityai/sd-vae-ft-mse",
			Files:   []string{"diffusion_pytorch_model.safetensors"},
			SizeMB:  320,
			Default: true,
			License: "mit",
			Note:    "Классическая замена VAE SD1.x (ft-mse): убирает артефакты декодера исходных чекпойнтов.",
		},
	},
}

// ComponentInstallMaxFiles — сколько файлов можно добавить одним запросом.
//
// Запас: у семейств реально 1–4 компонента, а большее число означает ошибку
// клиента (или попытку скачать «модель целиком» через компонентную ручку).
const ComponentInstallMaxFiles = 8

// ComponentInstallMaxSizeMB — потолок объёма одного компонентного запроса.
//
// ЗАЧЕМ: компонентная ручка пишет в каталог СУЩЕСТВУЮЩЕГО bundle, то есть может
// незаметно съесть диск. 20 ГиБ — верхняя граница реальных компонентов (самый
// большой в каталоге — энкодер FLUX.2, 16.8 ГиБ); всё, что больше, отвергается
// с кодом component_too_large, а не качается «пока не кончится место».
const ComponentInstallMaxSizeMB = 20 * 1024

// componentKindRoles — роли, которые вообще могут быть «компонентом» (то есть
// добавляются к уже существующему набору, а не заменяют его основу).
//
// diffusion исключён сознательно: его файл — это сама модель; «добавить» второй
// diffusion в bundle означает подменить рабочую диффузию (движок получает один
// --diffusion-model), и это не восстановление набора, а его замена.
var componentKindRoles = map[string]bool{
	types.ImageFileRoleVae:        true,
	types.ImageFileRoleClipL:      true,
	types.ImageFileRoleClipG:      true,
	types.ImageFileRoleT5xxl:      true,
	types.ImageFileRoleLLM:        true,
	types.ImageFileRoleClipVision: true,
	types.ImageFileRoleTaesd:      true,
	types.ImageFileRoleUpscaler:   true,
	types.ImageFileRoleControlNet: true,
	types.ImageFileRoleIPAdapter:  true,
}

// IsComponentRole — можно ли добавить эту роль в готовый набор (см. componentKindRoles).
func IsComponentRole(role string) bool { return componentKindRoles[strings.TrimSpace(role)] }

// componentRoleRank — порядок ролей в ответе (сначала то, без чего движок не
// стартует: VAE, затем энкодеры). Числа произвольны, важен порядок.
func componentRoleRank(role string) int {
	switch role {
	case types.ImageFileRoleVae:
		return 0
	case types.ImageFileRoleLLM:
		return 1
	case types.ImageFileRoleT5xxl:
		return 2
	case types.ImageFileRoleClipL:
		return 3
	case types.ImageFileRoleClipG:
		return 4
	case types.ImageFileRoleClipVision:
		return 5
	}
	return 9
}

// ComponentCatalog — компоненты семейства, опционально отфильтрованные по ролям.
//
// Порядок (это и есть «приоритет» из контракта API):
//  1. `Default=true` раньше остальных: кнопка «по умолчанию» должна быть первой;
//  2. внутри — порядок ролей componentRoleRank (VAE → LLM → T5 → CLIP): сначала
//     то, без чего движок не поднимется;
//  3. внутри роли — меньший размер раньше (дешевле и быстрее качает);
//  4. финальный tie-break — repo+files, чтобы ответ был ДЕТЕРМИНИРОВАН (на нём
//     держатся тесты и «одинаковый UI на двух воркерах»).
//
// Неизвестное семейство (в т.ч. "other") → пустой (не nil) срез: UI обязан
// различать «каталога нет» и «ошибка запроса», и пустой массив — это ответ.
func ComponentCatalog(family string, roles []string) []ImageComponent {
	family = strings.TrimSpace(family)
	filter := make(map[string]bool, len(roles))
	for _, r := range roles {
		r = strings.TrimSpace(r)
		if r != "" {
			filter[r] = true
		}
	}
	out := make([]ImageComponent, 0, 8)
	for _, c := range componentCatalog[family] {
		if len(filter) > 0 && !filter[c.Role] {
			continue
		}
		c.Family = family
		c.Files = append([]string(nil), c.Files...)
		out = append(out, c)
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Default != b.Default {
			return a.Default
		}
		if ra, rb := componentRoleRank(a.Role), componentRoleRank(b.Role); ra != rb {
			return ra < rb
		}
		if a.SizeMB != b.SizeMB {
			return a.SizeMB < b.SizeMB
		}
		if a.Repo != b.Repo {
			return a.Repo < b.Repo
		}
		return strings.Join(a.Files, ",") < strings.Join(b.Files, ",")
	})
	return out
}

// ComponentCatalogKnown — есть ли у семейства каталог вообще.
//
// false — это НЕ ошибка: для неизвестного семейства UI не показывает кнопки
// «по умолчанию» и оставляет ручной путь «Выбрать из другого репозитория…».
func ComponentCatalogKnown(family string) bool {
	return len(componentCatalog[strings.TrimSpace(family)]) > 0
}

// ComponentCatalogFamilies — семейства, для которых каталог заполнен (отсортировано).
//
// Нужен отчёту/диагностике и тесту «каталог не пуст ровно там, где обещали».
func ComponentCatalogFamilies() []string {
	out := make([]string, 0, len(componentCatalog))
	for f, list := range componentCatalog {
		if len(list) > 0 {
			out = append(out, f)
		}
	}
	sort.Strings(out)
	return out
}

// DefaultComponent — стандартный компонент семейства для роли (Default=true).
//
// Второй результат false означает «для этой роли стандартного компонента нет» —
// UI в этом случае НЕ рисует кнопку «Добавить X (по умолчанию)», но ручной путь
// (репозиторий + файлы) остаётся доступен.
func DefaultComponent(family, role string) (ImageComponent, bool) {
	for _, c := range componentCatalog[strings.TrimSpace(family)] {
		if c.Role == role && c.Default {
			c.Family = strings.TrimSpace(family)
			c.Files = append([]string(nil), c.Files...)
			return c, true
		}
	}
	return ImageComponent{}, false
}

// RequiredComponentRoles — роли, которых вердикт этого bundle требует и которых нет.
//
// Источник — сам вердикт (compat.Roles), а не таблица семейств: compat.go уже
// знает и про DiT-семейства без своей записи в таблице (там всё равно нужен
// VAE), и про «роль занята файлом с другим содержимым». Возвращает роли (id), а
// не подписи, потому что по ним ищется каталог.
//
// ПОЧЕМУ ФУНКЦИЯ, А НЕ МЕТОД (Фаза 4). С Типа-общего-пакета (internal/imagemodel)
// BundleCompat — псевдоним типа, а на псевдоним чужого типа методы определять
// нельзя. Логика осталась здесь: она зависит от каталога компонентов R710, а не
// от разбора заголовков.
func RequiredComponentRoles(c *BundleCompat) []string {
	if c == nil {
		return nil
	}
	out := make([]string, 0, 4)
	for _, r := range c.Roles {
		if !r.Required || r.Found {
			continue
		}
		if !IsComponentRole(r.Role) {
			continue // diffusion: его «добавлением» набор не чинится
		}
		out = append(out, r.Role)
	}
	sort.SliceStable(out, func(i, j int) bool {
		return componentRoleRank(out[i]) < componentRoleRank(out[j])
	})
	return out
}

// ComponentRoleTitle — короткая подпись роли для кнопок UI («VAE», «text
// encoder (LLM)»): та же, что в вердикте, чтобы кнопка и строка «не хватает: …»
// называли одно и то же одним словом.
func ComponentRoleTitle(role string) string { return CompatRoleLabel(role) }
