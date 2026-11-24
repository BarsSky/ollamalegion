// diskfree_unix.go — свободное место на Unix (образ сервиса — linux).
//
// Отдельным файлом с build-тегом, потому что syscall.Statfs есть только на
// Unix-ах, а сервис собирают и запускают локально на Windows (`go run`, тесты).
// Метрика та же, что у `df`: свободно для непривилегированного пользователя
// (Bavail), а не «свободно всего» (Bfree) — сервис работает под uid 1000.

//go:build !windows

package main

import "syscall"

// diskFreeBytes — свободные байты тома, на котором лежит path.
func diskFreeBytes(path string) (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	// Bsize на разных Unix-ах то int32, то int64 — приводим явно.
	return int64(st.Bavail) * int64(st.Bsize), nil
}
