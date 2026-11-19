// image_tool_resolve.go — R637 (2026-11-19): разрешение цели: resolveImageToolTarget и загруженная image-модель.
//
// ЗАЧЕМ ВЫНЕСЕНО. `image_tool.go` держал 1671 строку: настройки и цель
// инструмента, схему для клиента, разбор вызовов, загрузку модели, генерацию и
// помощники. Здесь — один из слоёв; поведение не менялось.
package balancer

import (
	"context"
	"ollama-loadbalancer/internal/i18n"
	"ollama-loadbalancer/pkg/logger"
	"strings"
)

// imageToolResolved — какая модель и на каком бэкенде будет исполнять вызов.
type imageToolResolved struct {
	BackendID string
	Model     string
	// Loaded — модель уже в VRAM (загрузка не нужна).
	Loaded bool
	State  string
	// NotFound — запрошенного имени нет в каталоге воркера. Нужно, чтобы вместо
	// непонятного 404 от движка отдать модели список доступных имён.
	NotFound bool
}

// resolveImageToolTarget — выбрать бэкенд и модель под конкретный вызов.
//
// ЗАЧЕМ ПЕРЕСМАТРИВАТЬ, А НЕ БРАТЬ target. По умолчанию используется модель из
// гейта (загруженная либо первая доступная). Но модель могла назвать ДРУГУЮ
// модель в аргументе model: тогда её надо найти в каталоге — и по имени, и по
// состоянию, и на живом бэкенде. Без этого «model=flux-schnell-q3-k» при
// загруженной sd15 означал бы «движок проигнорирует параметр и нарисует не то».
//
// Фолбэк при неизвестном имени: остаёмся на модели из гейта и НЕ отказываем —
// каталог мог не догрузиться, а генерация уже возможна. Имя при этом уходит в
// payload: воркер сам скажет, знает ли он такую модель (и код загрузки ниже
// вернёт честную ошибку).
func (p *Proxy) resolveImageToolTarget(ctx context.Context, target *imageToolTarget, requested string) imageToolResolved {
	res := imageToolResolved{BackendID: target.BackendID, Model: target.Model}
	if target.catalog != nil {
		for i := range target.catalog.Models {
			if m := &target.catalog.Models[i]; m.Name == res.Model {
				res.Loaded, res.State = m.Loaded, m.State
				break
			}
		}
	}
	requested = strings.TrimSpace(requested)
	if requested == "" || requested == res.Model {
		return res
	}

	catalog := p.ImageCatalogFor(ctx)
	for i := range catalog.Models {
		m := &catalog.Models[i]
		if m.Name != requested {
			continue
		}
		return imageToolResolved{
			BackendID: m.BackendID,
			Model:     m.Name,
			Loaded:    m.Loaded,
			State:     m.State,
		}
	}
	// Модель не найдена в каталоге: пробуем снимок напрямую (каталог мог быть
	// отдан из кэша до появления модели). Если и там нет — оставляем выбор гейта,
	// но с запрошенным ИМЕНЕМ, чтобы ошибка воркера была про конкретную модель.
	if res2 := p.imageResources(); res2 != nil {
		if snap := res2.ensureFreshFor(ctx, target.BackendID); snap != nil && snap.contractOK {
			for i := range snap.models {
				if snap.models[i].Name == requested {
					return imageToolResolved{
						BackendID: target.BackendID,
						Model:     requested,
						Loaded:    snap.models[i].State == imageStateLoaded,
						State:     snap.models[i].State,
					}
				}
			}
		}
	}
	logger.Get().Warnw(i18n.T("balancer.image_tool.resolve_missing_debug"),
		"requested", requested, "known", len(catalog.Models))
	return imageToolResolved{BackendID: target.BackendID, Model: requested, State: imageStateNotLoaded, NotFound: true}
}

// loadedImageModelForTarget — уже загруженная модель каталога (для подмены, когда
// модель назвала несуществующее имя).
//
// Выбираем ПЕРВУЮ загруженную: генерация по ней не требует ни загрузки, ни
// ожидания, то есть картинка придёт сразу. Каталог берём из target, чтобы не
// переспрашивать кластер.
func (p *Proxy) loadedImageModelForTarget(target *imageToolTarget) *imageToolResolved {
	if target == nil || target.catalog == nil {
		return nil
	}
	for i := range target.catalog.Models {
		m := &target.catalog.Models[i]
		if m.Loaded {
			return &imageToolResolved{
				BackendID: m.BackendID,
				Model:     m.Name,
				Loaded:    true,
				State:     m.State,
			}
		}
	}
	return nil
}

// imageToolMinPromptChars — минимальная длина промпта, на котором SD даёт
// осмысленную картинку. Порог намеренно низкий: «a cat» (5 символов) — валидный
// промпт, а «лес» (3) уже даёт узнаваемое. Отсекаем именно вырожденные случаи
// (пусто, «.», «x», «аааа»), на которых движок заливает всё одним цветом.
const imageToolMinPromptChars = 3
