package balancer

// one_inference_r493_test.go — R493-12q (этап 5 «два мозга»): паритет R86 на
// /api/chat.
//
// ЖИВОЙ ФАКТ (план §1): инструмент генерации изображений объявлялся только на
// handler-пути (`/v1/chat/completions` и `/ollama/api/chat`). «Голый» `/api/chat`
// идёт generic-путём и инструмента не объявлял вовсе — один кластер вёл себя
// по-разному в зависимости от URL у клиента.
//
// Здесь проверяется РЕШЕНИЕ (объявлять ли и каким телом) и подключение:
//  - выключенный флаг не трогает тело (историческое поведение);
//  - другая поверхность generic-пути инструмента не получает;
//  - выключенный инструмент / нет image-бэкенда — тело прежнее;
//  - клиент без tools[] в режиме auto — не объявляем (R141);
//  - при объявлении тело получает ровно те имена, что и на handler-пути
//    (паритет по ФАКТУ, а не по обещанию), и заголовок X-Image-Tool;
//  - цикл вызывается только когда инструмент объявлен.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ollama-loadbalancer/pkg/types"
)

// ollamaChatBodyR493 — тело /api/chat с одним клиентским инструментом (в режиме
// auto инструмент объявляется только тем, кто прислал свои tools[]).
const ollamaChatBodyR493 = `{"model":"qwen-test","messages":[{"role":"user","content":"нарисуй кота"}],"stream":false,` +
	`"tools":[{"type":"function","function":{"name":"client_tool","description":"client","parameters":{"type":"object","properties":{}}}}]}`

// ollamaChatBodyNoToolsR493 — то же без tools[] (режим auto молчит).
const ollamaChatBodyNoToolsR493 = `{"model":"qwen-test","messages":[{"role":"user","content":"нарисуй кота"}],"stream":false}`

func TestR493q_GenericImageToolGate(t *testing.T) {
	cases := []struct {
		name        string
		flag        string
		env         map[string]string
		surface     string
		body        string
		wantChanged bool
		wantReason  string
	}{
		{
			name: "флаг выключен — тело прежнее (историческое поведение)",
			flag: "0", surface: "/api/chat", body: ollamaChatBodyR493,
			wantChanged: false, wantReason: oneInferenceSkipDisabledR493,
		},
		{
			name: "другая поверхность — инструмента нет",
			flag: "1", surface: "/api/generate", body: ollamaChatBodyR493,
			wantChanged: false, wantReason: oneInferenceSkipSurfaceR493,
		},
		{
			name: "инструмент выключен оператором",
			flag: "1", surface: "/api/chat", body: ollamaChatBodyR493,
			env:         map[string]string{"LB_IMAGE_TOOL": "0"},
			wantChanged: false, wantReason: imageToolSkipDisabled,
		},
		{
			// Живой факт с локального стенда: при LB_IMAGE_TOOL_INJECT=off и
			// клиентских tools[] причина обязана называть ВЫКЛЮЧАТЕЛЬ, а не
			// «клиент не прислал tools[]».
			name: "инжект выключен, клиент прислал tools[]",
			flag: "1", surface: "/api/chat", body: ollamaChatBodyR493,
			env:         map[string]string{"LB_IMAGE_TOOL_INJECT": "off"},
			wantChanged: false, wantReason: imageToolSkipInjectOff,
		},
		{
			name: "клиент не прислал tools[] в режиме auto",
			flag: "1", surface: "/api/chat", body: ollamaChatBodyNoToolsR493,
			env: map[string]string{"LB_IMAGE_TOOL_INJECT": "auto"},
			// Причина — та же, что у handler-пути (imageToolNoInjectReason), иначе
			// лог врёт про «клиент не прислал tools[]» там, где выключен инжект.
			wantChanged: false, wantReason: imageToolSkipNoClientTools,
		},
		{
			name: "флаг включён, image-бэкенд есть, клиент прислал tools[] — объявляем",
			flag: "1", surface: "/api/chat", body: ollamaChatBodyR493,
			wantChanged: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("LB_ONE_INFERENCE", tc.flag)
			t.Setenv("LB_IMAGE_TOOL_INJECT", "")
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			p, _ := newImgResProxy(t, types.ImageResourceSettings{})

			out, added, tgt, reason := p.chooseGenericImageToolR493(
				context.Background(), tc.surface, "qwen-test", []byte(tc.body))

			if !tc.wantChanged {
				if string(out) != tc.body {
					t.Fatalf("тело изменилось там, где инструмент объявлять нельзя: %s", out)
				}
				if tgt != nil || len(added) > 0 {
					t.Fatalf("объявлен инструмент %v при причине %q", added, reason)
				}
				if tc.wantReason != "" && reason != tc.wantReason {
					t.Fatalf("причина = %q, ожидалась %q", reason, tc.wantReason)
				}
				return
			}
			if tgt == nil || len(added) == 0 {
				t.Fatalf("инструмент обязан быть объявлен, причина: %q", reason)
			}
			if reason != "" {
				t.Fatalf("при успешном объявлении причина обязана быть пустой: %q", reason)
			}
			// Тело — валидный JSON, и объявленные имена реально в нём есть.
			var doc map[string]interface{}
			if err := json.Unmarshal(out, &doc); err != nil {
				t.Fatalf("тело после объявления не разбирается: %v", err)
			}
			for _, name := range added {
				if !strings.Contains(string(out), `"`+name+`"`) {
					t.Fatalf("объявленное имя %q отсутствует в теле", name)
				}
			}
		})
	}
}

// TestR493q_ParityWithHandlerPath — паритет по ФАКТУ: generic-путь и handler-путь
// объявляют ОДНИ И ТЕ ЖЕ имена для одного тела.
func TestR493q_ParityWithHandlerPath(t *testing.T) {
	t.Setenv("LB_ONE_INFERENCE", "1")
	p, _ := newImgResProxy(t, types.ImageResourceSettings{})
	body := []byte(ollamaChatBodyR493)

	// Как это делает handler-путь (/ollama/api/chat): imageToolDecision + инжектор.
	tgt, skip := p.imageToolDecision(context.Background())
	if tgt == nil {
		t.Fatalf("фикстура не даёт image-цель: %s", skip)
	}
	_, handlerAdded := injectImageToolOllama(body, tgt)

	_, genericAdded, genericTgt, reason := p.chooseGenericImageToolR493(
		context.Background(), "/api/chat", "qwen-test", body)
	if genericTgt == nil {
		t.Fatalf("generic-путь инструмент не объявил: %s", reason)
	}
	if strings.Join(genericAdded, ",") != strings.Join(handlerAdded, ",") {
		t.Fatalf("пути объявили РАЗНЫЕ инструменты: generic %v, handler %v — "+
			"это ровно тот класс расхождения, который этап и убирает",
			genericAdded, handlerAdded)
	}
}

// TestR493q_LoopOnlyWhenDeclared — цикл вызывается только при объявленном
// инструменте: иначе на каждом /api/chat делался бы лишний нестриминговый ход.
func TestR493q_LoopOnlyWhenDeclared(t *testing.T) {
	t.Setenv("LB_ONE_INFERENCE", "0")
	p, _ := newImgResProxy(t, types.ImageResourceSettings{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/chat", strings.NewReader(ollamaChatBodyR493))

	handled, out := p.genericImageToolLoopR493(rec, req, "llm-1", "qwen-test", []byte(ollamaChatBodyR493))
	if handled {
		t.Fatal("при выключенном флаге цикл не должен забирать ответ")
	}
	if out != nil {
		t.Fatalf("при выключенном флаге тело не подменяется, получено %s", out)
	}
	if got := rec.Header().Get("X-Image-Tool"); got != "" {
		t.Fatalf("при выключенном флаге заголовок X-Image-Tool не ставится, получено %q", got)
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("при выключенном флаге в ответ ничего не пишется, получено %q", rec.Body.String())
	}
}

// TestR493q_HeaderOnlyWhenToolIsReal — заголовок X-Image-Tool не обещает
// инструмент, которого в этом запросе не будет: без исполнителя (роутера нет)
// объявление откатывается вместе с заголовком.
func TestR493q_HeaderOnlyWhenToolIsReal(t *testing.T) {
	t.Setenv("LB_ONE_INFERENCE", "1")
	p, _ := newImgResProxy(t, types.ImageResourceSettings{})
	p.llamaCppRouter = nil
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/chat", strings.NewReader(ollamaChatBodyR493))

	handled, out := p.genericImageToolLoopR493(rec, req, "llm-1", "qwen-test", []byte(ollamaChatBodyR493))
	if handled || out != nil {
		t.Fatalf("без исполнителя инструмент объявлять нельзя: handled=%t body=%s", handled, out)
	}
	if got := rec.Header().Get("X-Image-Tool"); got != "" {
		t.Fatalf("заголовок X-Image-Tool обещает инструмент, которого не будет: %q", got)
	}
}

// TestR493q_DeclaredToolSurvivesLoopFailure — если инструмент объявлен, а первый
// шаг цикла не удался, обычный путь обслуживает ТО ЖЕ тело (как handler-путь):
// иначе объявленный инструмент исчез бы, а заголовок остался.
func TestR493q_DeclaredToolSurvivesLoopFailure(t *testing.T) {
	t.Setenv("LB_ONE_INFERENCE", "1")
	p, _ := newImgResProxy(t, types.ImageResourceSettings{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/chat", strings.NewReader(ollamaChatBodyR493))

	// Закрытый порт ДЕТЕРМИНИРОВАННО: порт 18092 в фикстуре может оказаться живым
	// cppworker стенда, и тогда тест зависел бы от машины (класс флака R383).
	if err := p.MutateBackend("llm-1", func(b *types.Backend) {
		b.Host = "127.0.0.1"
		b.CppWorkerPort = 1
	}); err != nil {
		t.Fatalf("MutateBackend: %v", err)
	}
	handled, out := p.genericImageToolLoopR493(rec, req, "llm-1", "qwen-test", []byte(ollamaChatBodyR493))
	if handled {
		t.Fatal("цикл не мог ответить: сеть недоступна")
	}
	if len(out) == 0 {
		t.Fatal("объявленный инструмент обязан остаться в теле для обычного пути")
	}
	if !strings.Contains(string(out), "generate_image") {
		t.Fatalf("в теле нет объявленного инструмента: %s", out)
	}
	if got := rec.Header().Get("X-Image-Tool"); got == "" {
		t.Fatal("заголовок X-Image-Tool обязан называть объявленный инструмент")
	}
}

// TestR493q_FlagModes — один разбор флага: 0/1 и мусор.
func TestR493q_FlagModes(t *testing.T) {
	cases := map[string]bool{"": false, "0": false, "1": true, "2": false, "мусор": false}
	for raw, want := range cases {
		t.Run("значение="+raw, func(t *testing.T) {
			t.Setenv("LB_ONE_INFERENCE", raw)
			if got := oneInferenceEnabledR493(); got != want {
				t.Fatalf("флаг %q дал %t, ожидалось %t", raw, got, want)
			}
		})
	}
}
