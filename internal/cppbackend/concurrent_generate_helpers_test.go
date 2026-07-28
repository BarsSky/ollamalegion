// concurrent_generate_helpers_test.go — helpers for concurrent_generate_test.go.
//go:build llama_stub

package cppbackend

import "os"

// openFileCreate — тонкая обёртка вокруг os.Create для тестов
// (вынесена в helper, чтобы тесты не импортировали os напрямую).
func openFileCreate(path string) (*os.File, error) {
	return os.Create(path)
}
