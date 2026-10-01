// streamprobe — потоковый клиент для стенда: показывает, ЧЕРЕЗ СКОЛЬКО
// миллисекунд приходит каждый NDJSON-чанк, и проверяет ответ на дублирование
// хвоста (баг antiprompt в bridge.c).
//
// Запуск: go run ./debug/streamprobe <url> <body.json>
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
	"unicode/utf8"
)

func main() {
	if len(os.Args) < 3 {
		fmt.Println("usage: streamprobe <url> <body.json>")
		os.Exit(2)
	}
	url, bodyPath := os.Args[1], os.Args[2]
	body, err := os.ReadFile(bodyPath)
	if err != nil {
		panic(err)
	}
	start := time.Now()
	resp, err := http.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		panic(err)
	}
	defer resp.Body.Close()
	fmt.Printf("HTTP %d, content-type=%s\n", resp.StatusCode, resp.Header.Get("Content-Type"))

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0), 4<<20)

	var content strings.Builder
	var chunks, contentChunks, keepalives int
	var firstContentMs int64 = -1
	var final map[string]interface{}

	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		ms := time.Since(start).Milliseconds()
		var obj map[string]interface{}
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			fmt.Printf("[%6d ms] НЕ JSON: %s\n", ms, truncate(line, 120))
			continue
		}
		chunks++
		if ka, _ := obj["keepalive"].(bool); ka {
			keepalives++
			fmt.Printf("[%6d ms] keepalive\n", ms)
			continue
		}
		msg, _ := obj["message"].(map[string]interface{})
		c := ""
		if msg != nil {
			c, _ = msg["content"].(string)
		}
		done, _ := obj["done"].(bool)
		if done {
			final = obj
			reason, _ := obj["done_reason"].(string)
			fmt.Printf("[%6d ms] DONE reason=%s eval_count=%v\n", ms, reason, obj["eval_count"])
			continue
		}
		if c != "" {
			contentChunks++
			if firstContentMs < 0 {
				firstContentMs = ms
			}
			content.WriteString(c)
			fmt.Printf("[%6d ms] content %d байт: %q\n", ms, len(c), truncate(c, 60))
		} else {
			fmt.Printf("[%6d ms] пустой чанк (done=false)\n", ms)
		}
	}
	if err := sc.Err(); err != nil {
		fmt.Printf("ошибка чтения стрима: %v\n", err)
	}

	full := content.String()
	fmt.Printf("\nИТОГО: чанков=%d (контентных=%d, keepalive=%d) первый контент=%d ms всего=%d ms\n",
		chunks, contentChunks, keepalives, firstContentMs, time.Since(start).Milliseconds())
	fmt.Printf("склеенный контент: %d байт\n", len(full))
	if final != nil {
		if msg, ok := final["message"].(map[string]interface{}); ok {
			if tc, ok := msg["tool_calls"]; ok && tc != nil {
				b, _ := json.Marshal(tc)
				fmt.Printf("tool_calls: %s\n", truncate(string(b), 300))
			}
			if c, _ := msg["content"].(string); c != "" {
				fmt.Printf("контент в финальном чанке: %d байт\n", len(c))
			}
		}
		if e, _ := final["error"].(string); e != "" {
			fmt.Printf("ERROR: %s\n", e)
		}
	}
	if full == "" {
		fmt.Println("контента не было")
		return
	}
	// Проверка дублирования: ищем самый длинный хвост (>=40 байт), который
	// встречается в тексте ещё раз — признак повторной отправки буфера.
	if dup := duplicatedTail(full); dup >= 40 {
		fmt.Printf("!!! ДУБЛИРОВАНИЕ ХВОСТА: последние %d байт встречаются в тексте дважды\n", dup)
	} else {
		fmt.Println("дублирования хвоста не обнаружено")
	}
	// Маркеры tool call в контенте — утечка.
	for _, m := range []string{"<tool_call>", "<tool_call|>", "<|python_tag|>", "[TOOL_CALLS]"} {
		if strings.Contains(full, m) {
			fmt.Printf("!!! УТЕЧКА МАРКЕРА в content: %s\n", m)
		}
	}
	// Битый UTF-8: признак того, что окно удержания/дельта разрезали руну.
	if !utf8.ValidString(full) {
		fmt.Printf("!!! БИТЫЙ UTF-8 в ответе (%d символов U+FFFD)\n", strings.Count(full, "\uFFFD"))
	} else {
		fmt.Println("UTF-8 в ответе корректен")
	}
}

func duplicatedTail(s string) int {
	max := len(s) / 2
	if max > 1024 {
		max = 1024
	}
	for n := max; n >= 40; n-- {
		tail := s[len(s)-n:]
		if strings.Count(s, tail) > 1 {
			return n
		}
	}
	return 0
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "..."
}
