// image_tool_result.go — R637 (2026-11-19): результат: каталог-результат инструмента.
//
// ЗАЧЕМ ВЫНЕСЕНО. `image_tool.go` держал 1671 строку: настройки и цель
// инструмента, схему для клиента, разбор вызовов, загрузку модели, генерацию и
// помощники. Здесь — один из слоёв; поведение не менялось.
package balancer

import "strings"

// firstImageURL — ссылка на первую картинку в ответе генерации.
func firstImageURL(doc map[string]interface{}) string {
	data, ok := doc["data"].([]interface{})
	if !ok || len(data) == 0 {
		return ""
	}
	item, ok := data[0].(map[string]interface{})
	if !ok {
		return ""
	}
	url, _ := item["url"].(string)
	return strings.TrimSpace(url)
}
