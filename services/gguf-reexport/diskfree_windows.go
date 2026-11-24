// diskfree_windows.go — свободное место на Windows (локальный `go run` и тесты).
//
// На Windows syscall.Statfs нет, а тянуть ради одной цифры golang.org/x/sys в
// сервис, который сознательно не имеет зависимостей кроме zap, не хочется:
// kernel32!GetDiskFreeSpaceExW — три строки через syscall.NewLazyDLL.
// Цифра та же, что в «Свойства диска»: свободно текущему пользователю.

//go:build windows

package main

import (
	"syscall"
	"unsafe"
)

var (
	kernel32DLL            = syscall.NewLazyDLL("kernel32.dll")
	procGetDiskFreeSpaceEx = kernel32DLL.NewProc("GetDiskFreeSpaceExW")
)

// diskFreeBytes — свободные байты тома, на котором лежит path.
func diskFreeBytes(path string) (int64, error) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	var free, total, totalFree uint64
	r1, _, callErr := procGetDiskFreeSpaceEx.Call(
		uintptr(unsafe.Pointer(p)),
		uintptr(unsafe.Pointer(&free)),
		uintptr(unsafe.Pointer(&total)),
		uintptr(unsafe.Pointer(&totalFree)),
	)
	if r1 == 0 {
		return 0, callErr
	}
	return int64(free), nil
}
