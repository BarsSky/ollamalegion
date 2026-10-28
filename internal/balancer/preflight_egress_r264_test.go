// preflight_egress_r264_test.go — R264 (2026-10-28): пути self-proxy диалога
// preflight'а обязаны закрывать выход фильтра размышлений так же, как основной путь.
//
// ЖИВОЙ ПРОВАЛ (стенд r261). На основном (hijack) пути объяснение пустого ответа
// (R261) работало, а на self-proxy пути — нет: там применялся egress-фильтр, но не
// вызывался finalize. В результате стрим, целиком ушедший в reasoning_content,
// отдавался клиенту как молчаливая пустота — ровно та жалоба, из-за которой R261 и
// делался. Плюс на этом же пути мог теряться удержанный хвост маркера.
//
// Тесты фиксируют порядок клиентского контракта: объяснение → finish_reason →
// [DONE], и ровно по одному разу.
package balancer

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// collectSelfProxyLines — прогнать последовательность строк через
// selfProxyEgressLine так же, как это делает копирование self-proxy.
func collectSelfProxyLines(lines [][]byte) string {
	egress := &reasoningEgressState{}
	var out bytes.Buffer
	write := func(fr []byte) error {
		out.Write(fr)
		return nil
	}
	for _, line := range lines {
		if err := selfProxyEgressLine(line, egress, write); err != nil {
			panic(err)
		}
	}
	// Хвост и придержанный finish — как в реальном коде после цикла копирования.
	var last []byte
	_ = writeEgressFinalizeFrames(egress, &last, write)
	return out.String()
}

// TestR264_SelfProxyEmitsEmptyAnswerNotice — стрим только из размышлений получает
// ОДИН кадр-объяснение, и он идёт строго ПЕРЕД finish_reason.
func TestR264_SelfProxyEmitsEmptyAnswerNotice(t *testing.T) {
	reasoning := []byte(`data: {"choices":[{"index":0,"delta":{"reasoning_content":"думаю над задачей"},"finish_reason":null}]}`)
	reasoning2 := []byte(`data: {"choices":[{"index":0,"delta":{"reasoning_content":" и ещё немного"},"finish_reason":null}]}`)
	finish := []byte(`data: {"choices":[{"index":0,"delta":{},"finish_reason":"length"}]}`)
	done := []byte("data: [DONE]")

	got := collectSelfProxyLines([][]byte{reasoning, reasoning2, finish, done})

	noticeAt := strings.Index(got, "только размышления")
	if noticeAt < 0 {
		t.Fatalf("клиент не получил объяснения пустого ответа — ровно тот живой дефект:\n%s", got)
	}
	if n := strings.Count(got, "только размышления"); n != 1 {
		t.Errorf("объяснение пришло %d раз, want ровно 1", n)
	}
	finishAt := strings.Index(got, `"finish_reason":"length"`)
	if finishAt < 0 {
		t.Fatalf("finish_reason потерян:\n%s", got)
	}
	if noticeAt > finishAt {
		t.Errorf("объяснение пришло ПОСЛЕ finish_reason (строгие клиенты не читают контент после него):\n%s", got)
	}
	doneAt := strings.LastIndex(got, "data: [DONE]")
	if doneAt < finishAt {
		t.Errorf("[DONE] должен быть последним событием потока:\n%s", got)
	}
}

// TestR264_SelfProxyNormalAnswerUnchanged — когда видимый текст есть, объяснения
// быть не должно (иначе «лечим» нормальные ответы).
func TestR264_SelfProxyNormalAnswerUnchanged(t *testing.T) {
	content := []byte(`data: {"choices":[{"index":0,"delta":{"content":"391"},"finish_reason":null}]}`)
	finish := []byte(`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`)
	done := []byte("data: [DONE]")

	got := collectSelfProxyLines([][]byte{content, finish, done})

	if strings.Contains(got, "только размышления") {
		t.Errorf("объяснение появилось на нормальном ответе:\n%s", got)
	}
	if !strings.Contains(got, `"content":"391"`) {
		t.Errorf("текст ответа потерян:\n%s", got)
	}
	if !strings.Contains(got, `"finish_reason":"stop"`) {
		t.Errorf("finish_reason потерян:\n%s", got)
	}
}

// TestR264_SelfProxyHeldTailIsFlushed — хвост, удержанный как возможный префикс
// маркера размышлений, обязан доехать до клиента в конце стрима.
func TestR264_SelfProxyHeldTailIsFlushed(t *testing.T) {
	// Ответ кончается на "<" — фильтр держит это как возможное начало маркера.
	content := []byte(`data: {"choices":[{"index":0,"delta":{"content":"a <"},"finish_reason":null}]}`)
	finish := []byte(`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`)
	done := []byte("data: [DONE]")

	got := collectSelfProxyLines([][]byte{content, finish, done})

	// Собираем видимый текст так, как его склеит клиент по delta.content
	// (json.Marshal экранирует "<" в \u003c, поэтому сверяем не сырые байты).
	var visible strings.Builder
	for _, line := range strings.Split(got, "data: ") {
		line = strings.TrimSpace(strings.SplitN(line, "\n", 2)[0])
		if line == "" || line == "[DONE]" {
			continue
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(line), &chunk); err != nil {
			continue
		}
		if len(chunk.Choices) > 0 {
			visible.WriteString(chunk.Choices[0].Delta.Content)
		}
	}
	if got := visible.String(); got != "a <" {
		t.Errorf("удержанный хвост потерян: собрано %q, want %q (ответ обрывается на ровном месте)", got, "a <")
	}
}

// TestR264_SelfProxyChunkedTerminatorContract — порядок «кадры → [DONE] → конец»
// сохраняется и без upstream [DONE]: ensure*SSEDone пишет ровно один.
func TestR264_SelfProxyDoneOnlyOnce(t *testing.T) {
	reasoning := []byte(`data: {"choices":[{"index":0,"delta":{"reasoning_content":"x"},"finish_reason":null}]}`)
	finish := []byte(`data: {"choices":[{"index":0,"delta":{},"finish_reason":"length"}]}`)

	// Без [DONE] от воркера: синтетический маркер добавляет ensure*SSEDone,
	// поэтому здесь проверяем только то, что объяснение уже отдано до него.
	got := collectSelfProxyLines([][]byte{reasoning, finish})
	if !strings.Contains(got, "только размышления") {
		t.Fatalf("объяснение не пришло в потоке без [DONE]:\n%s", got)
	}
	if strings.Count(got, "data: [DONE]") != 0 {
		t.Errorf("сам selfProxyEgressLine не должен писать [DONE] (это делает ensure*):\n%s", got)
	}
}
