package i18n

import (
	"log"
	"sync"
)

// Register добавляет сообщения в каталог из отдельного файла.
//
// ЗАЧЕМ. Каталог пополняется батчами по областям кода. Держать всё в одном
// messages.go неудобно: параллельные правки одного файла конфликтуют. Поэтому
// каждая область (api, balancer, cppbackend, ...) живёт в своём файле
// messages_<область>.go и вызывает Register из init().
//
// ПРАВИЛА. Ключ должен быть уникальным: повторная регистрация того же ключа —
// ошибка программиста (иначе один текст молча затрёт другой). Дубликат не
// паникует в рантайме контейнера: пишем в лог и оставляем первый вариант;
// тест TestNoDuplicateKeys ловит это на сборке.
// duplicateMu/duplicateKeys хранят ключи, зарегистрированные дважды: это ошибка
// программиста, которую должен ловить тест (TestNoDuplicateKeys), а не тишина
// в рантайме контейнера.
var (
	duplicateMu   sync.Mutex
	duplicateKeys []string
)

func Register(catalog map[string]map[Lang]string) {
	for key, entry := range catalog {
		if _, exists := messages[key]; exists {
			log.Printf("[i18n] duplicate message key %q ignored (registered earlier)", key)
			duplicateMu.Lock()
			duplicateKeys = append(duplicateKeys, key)
			duplicateMu.Unlock()
			continue
		}
		messages[key] = entry
	}
}

// Duplicates — ключи, зарегистрированные повторно (для теста паритета каталога).
func Duplicates() []string {
	duplicateMu.Lock()
	defer duplicateMu.Unlock()
	out := make([]string, len(duplicateKeys))
	copy(out, duplicateKeys)
	return out
}
