package i18n

// message отдаёт перевод ключа на язык lang.
// Второй результат false означает «нет такого ключа вовсе» (в отличие от
// «ключ есть, но перевода на этот язык нет» — тогда отдастся ru-текст в Tf).
func message(key string, lang Lang) (string, bool) {
	entry, ok := messages[key]
	if !ok {
		return "", false
	}
	if msg, ok := entry[lang]; ok && msg != "" {
		return msg, true
	}
	if msg, ok := entry[RU]; ok && msg != "" {
		return msg, true
	}
	return "", false
}
