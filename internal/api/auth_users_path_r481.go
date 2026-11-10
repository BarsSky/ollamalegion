// auth_users_path_r481.go — R481: где по умолчанию живёт users.json.
//
// ЗАЧЕМ. Живая жалоба: «не даёт создавать пользователей — у контейнера нет прав
// на редактирование и создание config/users.json». Причина не в правах внутри
// тома, а в ПУТИ по умолчанию: если LB_USERS_FILE не задан, код брал
// `config/users.json`, а в Docker-образе ../config смонтирован READ-ONLY
// (`../config:/app/config:ro`). В варианте compose
// cppworker-bundled-with-agent переменная LB_USERS_FILE не передавалась вовсе,
// поэтому вход работал только за счёт env-учётки в памяти, а «Добавить
// пользователя» получал permission denied.
//
// ПРАВИЛО. В контейнере с томом /app/data (все поставляемые compose-файлы)
// дефолт — /app/data/users.json. Локальный запуск без тома сохраняет прежний
// путь config/users.json, чтобы поведение разработки не менялось.
package api

import "os"

// dirExists — проверка «каталог есть». Переменная (а не прямой os.Stat), чтобы
// тест мог проверить оба дефолта без создания /app/data.
var dirExists = func(dir string) bool {
	st, err := os.Stat(dir)
	return err == nil && st.IsDir()
}

// defaultUsersFileR481 — путь users.json по умолчанию.
func defaultUsersFileR481(exists func(string) bool) string {
	if exists != nil && exists("/app/data") {
		return "/app/data/users.json"
	}
	return "config/users.json"
}
