//go:build llama_stub

// rpc_session_lost_r224_test.go — R224 (2026-10-22).
//
// ЖИВОЙ ДЕФЕКТ. Узел раскладки умирает ВО ВРЕМЯ генерации: llama.cpp на мастер
// падает в ошибку декодирования, а C-мост подписывает её «likely KV-cache
// overflow ... or GPU OOM». Оператор ищет OOM и контекст, хотя нужно поднять узел
// и перезагрузить модель. Плюс модель остаётся загруженной с мёртвым
// RPC-устройством — генерации падают, пока её не выгрузят руками.
//
// Контракт R224:
//  1. сбой инференса при живых узлах — это НЕ потеря сессии, ошибка не подменяется;
//  2. узел не отвечает по TCP → RPCSessionLostError с прямым текстом «это не OOM»;
//  3. потеря сессии запускает авто-выгрузку модели (один раз на окно охлаждения);
//  4. отмена (abort) и классифицированные n_ctx/prompt ошибки не перекрываются;
//  5. авто-выгрузку можно выключить (CPPWORKER_RPC_SESSION_AUTO_UNLOAD=0).
package cppbackend

import (
	"errors"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"ollama-loadbalancer/c/bridge"
)

// r224Backend — бэкенд без загрузки модели: диагностика работает с конфигом и TCP.
func r224Backend(servers []string) (*Backend, *[]string) {
	var unloaded []string
	b := &Backend{
		cfg: Config{RPCEnabled: true, RPCServers: servers},
		unloadForRecovery: func(name string) error {
			unloaded = append(unloaded, name)
			return nil
		},
	}
	return b, &unloaded
}

func r224DeadEndpoint(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close() // порт закрыт: TCP-проба обязана увидеть отказ
	return addr
}

func r224LiveEndpoint(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	// Живой узел по правилу rpcNodeAnswers — тот, который держит соединение молча
	// (ждёт запроса). Соединение сразу закрывать нельзя: это трактуется как
	// «за портом не узел раскладки».
	var mu sync.Mutex
	var held []net.Conn
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			held = append(held, conn)
			mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		mu.Lock()
		defer mu.Unlock()
		for _, c := range held {
			_ = c.Close()
		}
	})
	return ln.Addr().String()
}

func TestR224_LiveNodesKeepOriginalError(t *testing.T) {
	b, unloaded := r224Backend([]string{r224LiveEndpoint(t)})
	// Заведомо «не наша» ошибка без классификации.
	orig := errors.New("bridge_batched_decode failed (code=1): llama_decode failed")
	got := b.diagnoseInferenceFailure(nil, "gemma", orig)
	if got != orig {
		t.Fatalf("при живом узле ошибка подменена: %v", got)
	}
	if len(*unloaded) != 0 {
		t.Fatalf("при живом узле запущена авто-выгрузка: %v", *unloaded)
	}
}

func TestR224_DeadNodeBecomesSessionLostAndUnloads(t *testing.T) {
	addr := r224DeadEndpoint(t)
	b, unloaded := r224Backend([]string{addr})
	orig := errors.New("stream inference failed with code 1: llama_decode failed during generation step")
	got := b.diagnoseInferenceFailure(nil, "gemma", orig)

	var lost *RPCSessionLostError
	if !errors.As(got, &lost) {
		t.Fatalf("ошибка не типизирована как RPCSessionLostError: %T %v", got, got)
	}
	if lost.Endpoint != addr {
		t.Errorf("endpoint %q, ожидался %q", lost.Endpoint, addr)
	}
	text := lost.Error()
	for _, want := range []string{"потеряна", "не OOM"} {
		if !strings.Contains(text, want) {
			t.Errorf("в тексте нет %q: %s", want, text)
		}
	}
	if !errors.Is(got, orig) {
		t.Error("исходная причина потеряна: errors.Is не проходит")
	}

	// Выгрузка асинхронная — ждём её появления.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(*unloaded) == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if len(*unloaded) != 1 || (*unloaded)[0] != "gemma" {
		t.Fatalf("авто-выгрузка не запущена: %v", *unloaded)
	}
}

func TestR224_NoLayoutNoDiagnosis(t *testing.T) {
	b := &Backend{cfg: Config{RPCEnabled: false, RPCServers: []string{r224DeadEndpoint(t)}}}
	orig := errors.New("some decode failure")
	if got := b.diagnoseInferenceFailure(nil, "gemma", orig); got != orig {
		t.Fatalf("без раскладки ошибка подменена: %v", got)
	}
}

func TestR224_ClassifiedErrorsNotOverridden(t *testing.T) {
	b, unloaded := r224Backend([]string{r224DeadEndpoint(t)})
	cases := map[string]error{
		"abort":         bridge.ErrAborted,
		"n_ctx reload":  bridge.ErrNCtxNeedsReload,
		"prompt length": bridge.ErrPromptTooLong,
	}
	for name, orig := range cases {
		got := b.diagnoseInferenceFailure(nil, "gemma", orig)
		if got != orig {
			t.Errorf("%s: ошибка подменена диагнозом узла: %v", name, got)
		}
	}
	time.Sleep(50 * time.Millisecond)
	if len(*unloaded) != 0 {
		t.Errorf("на классифицированных ошибках запущена авто-выгрузка: %v", *unloaded)
	}
}

func TestR224_AutoUnloadCooldown(t *testing.T) {
	b, _ := r224Backend(nil)
	now := time.Now()
	if !b.shouldAutoUnload("m", now) {
		t.Fatal("первая потеря сессии должна разрешать выгрузку")
	}
	if b.shouldAutoUnload("m", now.Add(time.Second)) {
		t.Error("вторая потеря подряд снова открыла выгрузку — будет гонка выгрузок")
	}
	if !b.shouldAutoUnload("other", now.Add(time.Second)) {
		t.Error("другая модель не должна блокироваться окном")
	}
	if !b.shouldAutoUnload("m", now.Add(rpcSessionUnloadCooldown+time.Second)) {
		t.Error("после окна охлаждения выгрузка должна быть снова разрешена")
	}

	// Отдельный бэкенд: у первого окно для "m" уже сдвинуто в будущее.
	b2, unloaded2 := r224Backend(nil)
	b2.unloadAfterSessionLoss(nil, "m")
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && len(*unloaded2) == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if len(*unloaded2) == 0 {
		t.Fatal("unloadAfterSessionLoss не вызвал выгрузку")
	}
	select {
	case <-time.After(time.Millisecond):
	}
}

// Регрессия живого случая: авто-выгрузка запускается в горутине, а следующий
// запрос тем временем перезагружает модель. Старый код выгружал инстанс ПО ИМЕНИ
// и освобождал уже новый (загрузка падала, модель пропадала из реестра).
func TestR224_ReplacedInstanceIsNotUnloaded(t *testing.T) {
	live := &modelInstance{}
	b := &Backend{models: map[string]*modelInstance{"m": live}}
	stale := &modelInstance{}

	if err := b.unloadModel("m", stale); err != nil {
		t.Fatalf("выгрузка с чужим инстансом: %v", err)
	}
	if b.models["m"] != live {
		t.Fatal("выгружен НОВЫЙ инстанс модели — именно этот дефект ловил exit code 2")
	}
	if err := b.unloadModel("m", live); err != nil {
		t.Fatalf("выгрузка своего инстанса: %v", err)
	}
	if _, ok := b.models["m"]; ok {
		t.Fatal("свой инстанс не выгружен")
	}
}
func TestR224_AutoUnloadCanBeDisabled(t *testing.T) {
	t.Setenv("CPPWORKER_RPC_SESSION_AUTO_UNLOAD", "0")
	b, unloaded := r224Backend(nil)
	b.unloadAfterSessionLoss(nil, "m")
	time.Sleep(100 * time.Millisecond)
	if len(*unloaded) != 0 {
		t.Fatalf("авто-выгрузка сработала при отключении: %v", *unloaded)
	}
	if !rpcSessionAutoUnloadDisabled() {
		t.Error("переменная отключения не распознана")
	}
	_ = os.Unsetenv("CPPWORKER_RPC_SESSION_AUTO_UNLOAD")
	if rpcSessionAutoUnloadDisabled() {
		t.Error("после снятия переменной авто-выгрузка должна быть включена")
	}
}
