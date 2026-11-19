// image_tool_catalog.go — R637 (2026-11-19): описания каталога: строки, имена, семья.
//
// ЗАЧЕМ ВЫНЕСЕНО. `image_tool.go` держал 1671 строку: настройки и цель
// инструмента, схему для клиента, разбор вызовов, загрузку модели, генерацию и
// помощники. Здесь — один из слоёв; поведение не менялось.
package balancer

import (
	"context"
	"fmt"
	"strings"
)

// imageToolCatalogResult — результат list_image_models для tool-сообщения.
//
// ФОРМА ВЫБРАНА ПОД ЧТЕНИЕ МОДЕЛЬЮ: строка summary (одна строка на модель) —
// самое дешёвое по токенам представление, а полный JSON уходит в models[] для
// тех клиентов/агентов, которые захотят разобрать его программно. Никакой
// генерации и обращения к GPU здесь нет — только чтение уже собранного каталога.
func (p *Proxy) imageToolCatalogResult(ctx context.Context, family string) map[string]interface{} {
	family = strings.ToLower(strings.TrimSpace(family))

	catalog := p.ImageCatalogFor(ctx)
	models := make([]ImageCatalogModel, 0, len(catalog.Models))
	var sb strings.Builder
	for _, m := range catalog.Models {
		if family != "" && !strings.EqualFold(m.Family, family) {
			continue
		}
		models = append(models, m)
		sb.WriteString(imageCatalogModelLine(m))
		sb.WriteByte('\n')
	}

	out := map[string]interface{}{
		"status": "ok",
		"count":  len(models),
		"models": models,
		"limits": catalog.Limits,
		// loadedModel — что уже в VRAM: если оно подходит, генерация начнётся
		// сразу, без ожидания загрузки.
		"hasLoadedModel": catalog.HasLoadedModel,
	}
	if family != "" {
		out["family"] = family
	}
	if len(models) == 0 {
		// ФИЛЬТР НЕ ДАЛ НИЧЕГО (живой случай 2026-10-06): модель вызвала каталог с
		// family="stable-diffusion", получила пустой ответ и дальше придумала имя
		// модели из своих знаний ("stable-diffusion:1.5"), которого на воркере нет.
		// Поэтому в пустом ответе ОБЯЗАТЕЛЬНО перечисляем доступные имена и
		// говорим, что фильтр нужно снять.
		available := imageCatalogNames(catalog)
		if len(available) > 0 {
			out["status"] = "empty"
			out["availableModels"] = available
			out["summary"] = fmt.Sprintf(
				"По фильтру family=%q моделей нет. ДОСТУПНЫЕ модели (используй имя ровно как есть, без выдумывания): %s. Вызови list_image_models без параметра family, чтобы увидеть их описания и требования к VRAM.",
				family, strings.Join(available, ", "))
		} else {
			out["summary"] = "В каталоге нет моделей ни на одном image-бэкенде. Загрузите модель на image-воркер (WebUI → Image-модели → HuggingFace) или через POST /api/v1/image/backends/{id}/models/load."
		}
	} else {
		out["summary"] = fmt.Sprintf("Доступные image-модели (%d)%s:\n%s",
			len(models), familySuffix(family), strings.TrimRight(sb.String(), "\n"))
	}
	if loaded := imageCatalogLoadedLine(catalog); loaded != "" {
		out["loadedModel"] = loaded
	}
	return out
}

// imageCatalogModelLine — одна строка каталога для модели.
func imageCatalogModelLine(m ImageCatalogModel) string {
	parts := []string{"- " + m.Name}
	if m.Family != "" {
		parts = append(parts, "семейство "+m.Family)
	}
	state := m.State
	if state == "" {
		state = "unknown"
	}
	if m.Loaded {
		state += " (уже в VRAM)"
	}
	parts = append(parts, "состояние: "+state)
	if m.VramEstimateMB > 0 {
		parts = append(parts, fmt.Sprintf("~%d MB VRAM", m.VramEstimateMB))
	} else {
		parts = append(parts, "VRAM: неизвестно")
	}
	parts = append(parts, fmt.Sprintf("дефолт: %dx%d, %d шагов, cfg %g",
		m.Defaults.Width, m.Defaults.Height, m.Defaults.Steps, m.Defaults.CFGScale))
	if m.Strengths != "" {
		parts = append(parts, m.Strengths)
	}
	return strings.Join(parts, " | ")
}

// imageCatalogLoadedLine — что сейчас загружено (пусто, если ничего).
func imageCatalogLoadedLine(catalog *ImageCatalog) string {
	if catalog == nil {
		return ""
	}
	for _, m := range catalog.Models {
		if m.Loaded {
			return m.Name
		}
	}
	return ""
}

// imageCatalogNames — имена моделей каталога (для подсказки в tool-результате).
func imageCatalogNames(catalog *ImageCatalog) []string {
	if catalog == nil {
		return nil
	}
	names := make([]string, 0, len(catalog.Models))
	for _, m := range catalog.Models {
		if m.Name != "" {
			names = append(names, m.Name)
		}
	}
	return names
}
