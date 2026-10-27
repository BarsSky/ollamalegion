package main

import (
	"encoding/base64"
	"strings"
	"testing"
)

// R248/B3: base64-payload'ы картинок из messages[].images.
func TestR248_DecodeVisionImages(t *testing.T) {
	payload := base64.StdEncoding.EncodeToString([]byte("\x89PNG\r\n\x1a\nfake"))
	for _, in := range []string{
		payload,                            // голый base64
		"data:image/png;base64," + payload, // data-URL
		" " + payload + "\n",               // пробелы/переводы строк
	} {
		out, err := decodeVisionImages([]string{in})
		if err != nil {
			t.Fatalf("%q: %v", in, err)
		}
		if len(out) != 1 || len(out[0]) == 0 {
			t.Fatalf("%q: пустой результат", in)
		}
	}
}

// R248/B3: непригодные payload'ы дают различимые ошибки (клиент должен видеть
// image_decode_failed / image_unsupported_type, а не 500).
func TestR248_DecodeVisionImagesErrors(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"пусто", "", "image_decode_failed"},
		{"не base64", "%%%not-base64%%%", "image_decode_failed"},
		{"data-URL без base64", "data:image/svg+xml,<svg/>", "image_unsupported_type"},
		{"data-URL без запятой", "data:image/png;base64", "image_decode_failed"},
	}
	for _, tc := range cases {
		_, err := decodeVisionImages([]string{tc.in})
		if err == nil {
			t.Fatalf("%s: ожидалась ошибка", tc.name)
		}
		if !strings.HasPrefix(err.Error(), tc.want) {
			t.Fatalf("%s: ошибка %q, ожидался префикс %q", tc.name, err.Error(), tc.want)
		}
	}
}

// R248/B3: лимит количества картинок — too_many_images.
func TestR248_DecodeVisionImagesTooMany(t *testing.T) {
	payload := base64.StdEncoding.EncodeToString([]byte("x"))
	payloads := make([]string, visionMaxImages+1)
	for i := range payloads {
		payloads[i] = payload
	}
	if _, err := decodeVisionImages(payloads); err == nil || !strings.HasPrefix(err.Error(), "too_many_images") {
		t.Fatalf("ожидалась too_many_images, получено %v", err)
	}
}

// R248/B3: маркер проектора вставляется в текст сообщения по одному на
// картинку, в порядке сообщений; сообщения без картинок не меняются.
func TestR248_InjectMediaMarkers(t *testing.T) {
	msgs := []chatMessage{
		{Role: "user", Content: "что на картинке?", Images: []string{"a", "b"}},
		{Role: "assistant", Content: "вижу"},
	}
	out := injectMediaMarkers(msgs, "<__media__>")
	if out[0].Content != "<__media__>\n<__media__>\nчто на картинке?" {
		t.Fatalf("content = %q", out[0].Content)
	}
	if out[1].Content != "вижу" {
		t.Fatalf("сообщение без картинок изменено: %q", out[1].Content)
	}
	if msgs[0].Content != "что на картинке?" {
		t.Fatalf("исходный срез мутирован: %q", msgs[0].Content)
	}
	if out := injectMediaMarkers(msgs, ""); out[0].Content != "что на картинке?" {
		t.Fatalf("без маркера текст не должен меняться: %q", out[0].Content)
	}
}

// R248/B3: пустой images в сообщении → нет картинок.
func TestR248_ChatImagesEmpty(t *testing.T) {
	if got := chatImages([]chatMessage{{Role: "user", Content: "hi"}}); len(got) != 0 {
		t.Fatalf("ожидалось 0 payload'ов, получено %d", len(got))
	}
}
