package balancer

import (
	"encoding/json"
	"testing"
)

// R249: auto-stream → non-stream сборка должна чинить аргументы так же, как
// стриминговый путь. Живой дефект: non-stream клиент получал
// {"content":"{\"content\": ...}"} без обязательного path.
func TestR249_RepairAssembledToolCalls(t *testing.T) {
	clientReq := []byte(`{"model":"m","tools":[{"type":"function","function":{"name":"write","parameters":{"type":"object","properties":{"path":{"type":"string"},"content":{"type":"string"}},"required":["path","content"]}}}]}`)
	assembled := []byte(`{"choices":[{"index":0,"finish_reason":"tool_calls","message":{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"write","arguments":"{\"content\":\"{\\\"content\\\": \\\"int main(){return 0;}\\\";\\\\n\\\", \\\"path\\\": \\\"C:/work/hello.cpp\\\"}\"}"}}]}}]}`)

	out := repairAssembledToolCalls(assembled, clientReq)
	var resp map[string]interface{}
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatalf("результат не JSON: %v (%s)", err, out)
	}
	msg := resp["choices"].([]interface{})[0].(map[string]interface{})["message"].(map[string]interface{})
	call := msg["tool_calls"].([]interface{})[0].(map[string]interface{})
	rawArgs := call["function"].(map[string]interface{})["arguments"].(string)

	var args map[string]interface{}
	if err := json.Unmarshal([]byte(rawArgs), &args); err != nil {
		t.Fatalf("аргументы после ремонта не JSON: %v (%s)", err, rawArgs)
	}
	if args["path"] != "C:/work/hello.cpp" {
		t.Fatalf("path = %v (args=%s)", args["path"], rawArgs)
	}
	if c, _ := args["content"].(string); c == "" {
		t.Fatalf("content пуст (args=%s)", rawArgs)
	}
}

// R249: без схем или на нормальных аргументах ответ не меняется.
func TestR249_RepairAssembledKeepsUntouched(t *testing.T) {
	assembled := []byte(`{"choices":[{"message":{"tool_calls":[{"function":{"name":"write","arguments":"{\"path\":\".\",\"content\":\"x\"}"}}]}}]}`)
	if got := repairAssembledToolCalls(assembled, []byte(`{"model":"m"}`)); string(got) != string(assembled) {
		t.Fatalf("ответ без схем изменён: %s", got)
	}
}
