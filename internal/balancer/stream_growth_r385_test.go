package balancer

// stream_growth_r385_test.go — R385 (2026-11-01): стриминговый запрос с БОЛЬШИМ
// окном поднимает модель, если это возможно.
//
// Прежнее правило (R301) «стриминг окно не растит» отменено оператором: запрос
// с num_ctx=131072 при загруженных 32768 обязан приводить к перезагрузке модели,
// если она это поддерживает и нет занятости/физических ограничений. Здесь
// проверяются все четыре исхода: рост, fallback-«обслужить», отказ и запрет роста
// по занятости.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ollama-loadbalancer/pkg/types"
)

// TestR385_StreamingGrowsInsteadOfServing — клиент попросил окно больше
// загруженного, промпт крошечный: раньше его молча обслуживали с урезанным
// окном, теперь — рост.
func TestR385_StreamingGrowsInsteadOfServing(t *testing.T) {
	p := newGrowthGuardProxyR299()

	v := p.growthVerdictR301("bk-1", "m", []byte(r301StreamBody), "/api/chat", true)
	if !v.Grow {
		t.Fatalf("ожидался рост окна (needed=%d loaded=%d), получено %+v", v.Needed, v.Loaded, v)
	}
	if v.Served || v.Defer {
		t.Fatalf("рост несовместим с Served/Defer: %+v", v)
	}
	if v.Needed <= v.Loaded {
		t.Fatalf("нужное окно %d не больше загруженного %d — вердикт не про рост", v.Needed, v.Loaded)
	}
}

// TestR385_StreamingGrowsForOversizedPrompt — промпт не влезает в загруженное
// окно: раньше это был отказ «поднимите окно в карточке модели», теперь рост.
func TestR385_StreamingGrowsForOversizedPrompt(t *testing.T) {
	p := newGrowthGuardProxyR299()

	v := p.growthVerdictR301("bk-1", "m", []byte(r301LongStreamBody()), "/api/chat", true)
	if !v.Grow {
		t.Fatalf("ожидался рост окна под большой промпт, получено %+v", v)
	}
}

// TestR385_PreflightStartsReloadAndAsksRetry — транспорт: отказать, но с
// причиной «рост начат» и числами, а не «нельзя».
func TestR385_PreflightStartsReloadAndAsksRetry(t *testing.T) {
	p := newGrowthGuardProxyR299()

	body, needsProxy, msg, status := p.preflightNCtxReloadIfNeeded(
		httptest.NewRequest(http.MethodPost, "/api/chat", nil).Context(),
		"bk-1", "m", []byte(r301StreamBody), "/api/chat")
	if needsProxy {
		t.Fatal("рост окна требует перезагрузки: запрос нельзя пропускать дальше как есть")
	}
	if status != http.StatusServiceUnavailable {
		t.Fatalf("HTTP %d, ожидался 503 (перезагрузка идёт)", status)
	}
	if !strings.Contains(msg, "started reloading") {
		t.Fatalf("в причине нет факта начала перезагрузки: %q", msg)
	}
	if !strings.Contains(msg, "16384") || !strings.Contains(msg, "8192") {
		t.Fatalf("в причине нет чисел нужно/загружено: %q", msg)
	}
	if string(body) != r301StreamBody {
		t.Fatal("тело изменилось: при росте окна его нельзя подменять на меньшее")
	}
}

// TestR385_NoGrowthWhileStreamActive — идёт стрим: рост НЕ запускаем (иначе
// перезагрузка рвёт живой ответ, R271). Поведение прежнее: обслужить, если
// промпт влезает.
func TestR385_NoGrowthWhileStreamActive(t *testing.T) {
	p := newGrowthGuardProxyR299()
	release := beginClientStreamR271(p, "bk-1", "m")
	defer release()

	v := p.growthVerdictR301("bk-1", "m", []byte(r301StreamBody), "/api/chat", true)
	if v.Grow {
		t.Fatalf("при активном стриме рост запрещён: %+v", v)
	}
	if !v.Served {
		t.Fatalf("ожидалось «обслужить с загруженным окном», получено %+v", v)
	}
}

// TestR385_GrowthCheckReasons — почему рост может быть запрещён.
func TestR385_GrowthCheckReasons(t *testing.T) {
	t.Run("выключатель", func(t *testing.T) {
		t.Setenv("LB_NCTX_GROW_FOR_STREAM", "0")
		p := newGrowthGuardProxyR299()
		if ok, why := p.streamGrowthCheckR385("bk-1", "m", 16384); ok {
			t.Fatal("при LB_NCTX_GROW_FOR_STREAM=0 рост обязан быть запрещён")
		} else if !strings.Contains(why, "выключен") {
			t.Fatalf("причина = %q", why)
		}
	})

	t.Run("primary: окно фиксировано администратором", func(t *testing.T) {
		p := newGrowthGuardProxyR299()
		p.config.LlamaCppModelProfiles = map[string]types.LlamaCppModelProfile{"m": {Primary: true}}
		if ok, why := p.streamGrowthCheckR385("bk-1", "m", 16384); ok {
			t.Fatal("primary-модель клиентским запросом не перезагружается")
		} else if !strings.Contains(why, "primary") {
			t.Fatalf("причина = %q", why)
		}
	})

	t.Run("потолок окна ниже нужного", func(t *testing.T) {
		p := newGrowthGuardProxyR299()
		p.config.LlamaCppModelProfiles = map[string]types.LlamaCppModelProfile{"m": {ContextLengthMax: 8192}}
		if ok, why := p.streamGrowthCheckR385("bk-1", "m", 65536); ok {
			t.Fatal("нужно 65536 при потолке 8192 — рост обязан быть запрещён")
		} else if why == "" {
			t.Fatal("причина отказа не названа")
		}
	})

	t.Run("рост разрешён", func(t *testing.T) {
		p := newGrowthGuardProxyR299()
		if ok, why := p.streamGrowthCheckR385("bk-1", "m", 16384); !ok {
			t.Fatalf("рост должен быть разрешён, причина отказа: %q", why)
		}
	})
}
