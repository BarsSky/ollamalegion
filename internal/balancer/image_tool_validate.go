// image_tool_validate.go — R637 (2026-11-19): валидация промпта и ошибки неизвестной модели.
//
// ЗАЧЕМ ВЫНЕСЕНО. `image_tool.go` держал 1671 строку: настройки и цель
// инструмента, схему для клиента, разбор вызовов, загрузку модели, генерацию и
// помощники. Здесь — один из слоёв; поведение не менялось.
package balancer

import (
	"context"
	"errors"
	"fmt"
	"ollama-loadbalancer/internal/i18n"
	"strings"
	"unicode/utf8"
)

// validateImageToolPrompt — отсечь промпт, на котором получится мусор.
//
// ЗАЧЕМ: живой случай — модель вызвала инструмент с prompt_len=6, пользователь
// получил красный квадрат и справедливо решил, что «генерация сломана». Ошибка в
// tool-сообщении честнее: модель видит текст и может исправиться, а пользователь
// не получает картинку-заглушку под видом результата.
func validateImageToolPrompt(prompt string) error {
	trimmed := strings.TrimSpace(prompt)
	if trimmed == "" {
		return errors.New(i18n.T("balancer.image_tool.prompt_empty"))
	}
	if utf8.RuneCountInString(trimmed) < imageToolMinPromptChars {
		return fmt.Errorf(
			i18n.T("balancer.image_tool.prompt_too_short"),
			trimmed)
	}
	// Один и тот же символ/пара повторов («ааааа», «....») — тоже заливка.
	if isDegeneratePrompt(trimmed) {
		return fmt.Errorf(
			i18n.T("balancer.image_tool.prompt_degenerate"),
			trimmed)
	}
	return nil
}

// isDegeneratePrompt — промпт из одного повторяющегося символа.
func isDegeneratePrompt(s string) bool {
	runes := []rune(s)
	if len(runes) == 0 {
		return true
	}
	for _, r := range runes {
		if r != runes[0] {
			return false
		}
	}
	return true
}

// imageToolUnknownModelError — понятная ошибка «такой модели нет».
//
// ЗАЧЕМ СВОЙ ТЕКСТ: ошибка воркера «image model not found: stable-diffusion:1.5»
// ничего не говорит модели о том, ЧТО ЖЕ доступно, и следующий её шаг — снова
// угадывать имя (живой случай 2026-10-06). Перечисляем доступные имена прямо в
// tool-сообщении: модель может исправиться на следующем turn.
func (p *Proxy) imageToolUnknownModelError(ctx context.Context, requested string) string {
	names := imageCatalogNames(p.ImageCatalogFor(ctx))
	if len(names) == 0 {
		return i18n.T("balancer.image_tool.unknown_model_none", requested)
	}
	return i18n.T("balancer.image_tool.unknown_model_list",
		requested, strings.Join(names, ", "))
}
