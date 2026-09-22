package tests

import "sync/atomic"

// R66d (2026-09-22): потокобезопасные читатели счётчиков mockOllamaServer.
//
// Зачем: счётчики в mockOllamaServer инкрементируются в HTTP-хендлере (то есть
// в чужой горутине), а тесты читали их напрямую — под -race это ловилось как
// DATA RACE (например, "Write at ... tests.(*mockOllamaServer).handleRequest()
// proxy_mock_test.go:54"). Поля переведены на int64 + atomic, а чтение идёт
// через эти методы, чтобы в тестах остались обычные int-сравнения
// (assert.Equal(t, 1, mock.GenerateCalls())).

func (m *mockOllamaServer) GenerateCalls() int { return int(atomic.LoadInt64(&m.generateCount)) }
func (m *mockOllamaServer) ChatCalls() int     { return int(atomic.LoadInt64(&m.chatCount)) }
func (m *mockOllamaServer) EmbedCalls() int    { return int(atomic.LoadInt64(&m.embedCount)) }
func (m *mockOllamaServer) Embed2Calls() int   { return int(atomic.LoadInt64(&m.embed2Count)) }
func (m *mockOllamaServer) TagsCalls() int     { return int(atomic.LoadInt64(&m.tagsCount)) }
func (m *mockOllamaServer) PsCalls() int       { return int(atomic.LoadInt64(&m.psCount)) }
func (m *mockOllamaServer) VersionCalls() int  { return int(atomic.LoadInt64(&m.versionCount)) }
func (m *mockOllamaServer) ShowCalls() int     { return int(atomic.LoadInt64(&m.showCount)) }
func (m *mockOllamaServer) CreateCalls() int   { return int(atomic.LoadInt64(&m.createCount)) }
func (m *mockOllamaServer) PullCalls() int     { return int(atomic.LoadInt64(&m.pullCount)) }
func (m *mockOllamaServer) DeleteCalls() int   { return int(atomic.LoadInt64(&m.deleteCount)) }
func (m *mockOllamaServer) CopyCalls() int     { return int(atomic.LoadInt64(&m.copyCount)) }
func (m *mockOllamaServer) PushCalls() int     { return int(atomic.LoadInt64(&m.pushCount)) }
