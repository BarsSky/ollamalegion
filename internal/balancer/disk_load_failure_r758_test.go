package balancer

// disk_load_failure_r758_test.go — R758 (2026-11-25): текст уведомления о нехватке
// места на диске и трекер повторных запросов.
//
// Первая половина — про ЖИВУЮ ЖАЛОБУ «мало места на диске, а видно только в
// колокольчике»: сообщение обязано называть РЕСУРС (диск), ЧИСЛА (если пришли) и
// действие, а не «не хватило памяти».
//
// Вторая — про «модель отвечает одно и то же несколько раз»: трекер считает
// повторы ТОГО ЖЕ тела запроса и объясняет их в логе. Он не должен ни блокировать
// запрос, ни считать повторами разные вопросы одной сессии.

import (
	"strings"
	"testing"

	"ollama-loadbalancer/pkg/types"
)

// TestR758_DiskFailureMessageNamesDiskAndNumbers — сообщение про диск.
func TestR758_DiskFailureMessageNamesDiskAndNumbers(t *testing.T) {
	msg := loadFailureEventMessage(&types.LoadFailureInfo{
		Model:  "Inkling-2-Mini-UD-Q4_K_M",
		Reason: "insufficient_disk",
		Error:  "write /app/models/x.gguf: no space left on device",
		Diagnostics: map[string]interface{}{
			"disk_free_mb": float64(2635),
			"disk_need_mb": float64(12000),
		},
	})
	if !strings.Contains(msg, "диск") {
		t.Fatalf("в сообщении не назван диск: %q — оператор пойдёт уменьшать n_ctx", msg)
	}
	if !strings.Contains(msg, "2635") || !strings.Contains(msg, "12000") {
		t.Errorf("в сообщении нет чисел (свободно/требуется): %q", msg)
	}
	if strings.Contains(msg, "памяти") {
		t.Errorf("сообщение про нехватку ПАМЯТИ при отказе по ДИСКУ: %q", msg)
	}
}

// TestR758_DiskFailureMessageWithoutNumbers — чисел нет: не выдумываем нули.
func TestR758_DiskFailureMessageWithoutNumbers(t *testing.T) {
	msg := loadFailureEventMessage(&types.LoadFailureInfo{
		Model:  "m",
		Reason: "insufficient_disk",
	})
	if strings.Contains(msg, "0 МБ") {
		t.Errorf("в сообщении появились выдуманные нули: %q", msg)
	}
	if !strings.Contains(msg, "места на диске") {
		t.Errorf("причина не названа: %q", msg)
	}
}

// TestR758_RepeatTrackerCountsSameBody — тот же ход = повтор; другой вопрос нет.
func TestR758_RepeatTrackerCountsSameBody(t *testing.T) {
	p := &Proxy{config: &types.LoadBalancerConfig{}}
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"исправь файл"}]}`)

	if n, rep := p.noteRequestRepeatR758("sess-1", "m", "b1", "/api/chat", body, 200); n != 1 || rep {
		t.Fatalf("первый запрос: count=%d repeat=%v, want 1/false", n, rep)
	}
	if n, rep := p.noteRequestRepeatR758("sess-1", "m", "b1", "/api/chat", body, 502); n != 2 || !rep {
		t.Fatalf("второй тот же запрос: count=%d repeat=%v, want 2/true — именно это "+
			"оператор видит как «модель ответила то же самое»", n, rep)
	}
	if n, rep := p.noteRequestRepeatR758("sess-1", "m", "b1", "/api/chat", body, 200); n != 3 || !rep {
		t.Fatalf("третий: count=%d repeat=%v, want 3/true", n, rep)
	}

	// Другой вопрос той же сессии — НЕ повтор.
	other := []byte(`{"model":"m","messages":[{"role":"user","content":"другой вопрос"}]}`)
	if n, rep := p.noteRequestRepeatR758("sess-1", "m", "b1", "/api/chat", other, 200); n != 1 || rep {
		t.Fatalf("другой вопрос посчитан повтором: count=%d repeat=%v", n, rep)
	}
	// Другая сессия с тем же телом — тоже не повтор.
	if n, rep := p.noteRequestRepeatR758("sess-2", "m", "b1", "/api/chat", body, 200); n != 1 || rep {
		t.Fatalf("другая сессия посчитана повтором: count=%d repeat=%v", n, rep)
	}
}

// TestR758_RepeatTrackerNilSafe — без прокси/тела трекер молчит и не паникует.
func TestR758_RepeatTrackerNilSafe(t *testing.T) {
	var p *Proxy
	if n, rep := p.noteRequestRepeatR758("s", "m", "b", "/api/chat", []byte("x"), 0); n != 1 || rep {
		t.Fatalf("nil-прокси: count=%d repeat=%v, want 1/false", n, rep)
	}
	real := &Proxy{config: &types.LoadBalancerConfig{}}
	if n, rep := real.noteRequestRepeatR758("s", "m", "b", "/api/chat", nil, 0); n != 1 || rep {
		t.Fatalf("пустое тело: count=%d repeat=%v, want 1/false", n, rep)
	}
}
