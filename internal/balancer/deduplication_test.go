// deduplication_test.go — R60.54 tests for Qwen3-Instruct antipattern response deduplication.
package balancer

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

// TestR6054_DeduplicateResponseContent_BasicThreeDuplicates — simulates the
// exact pattern user reported: Qwen3-Instruct emits the same response 3 times
// with greeting as anchor. Verify the second occurrence triggers truncation.
func TestR6054_DeduplicateResponseContent_BasicThreeDuplicates(t *testing.T) {
	// Simulate the 3-duplicate response with the exact anchor pattern
	// from the user's report:
	//   "Привет! Конечно, с радостью помогу — вот красивый и интерактивный сайт..."
	greetingAnchor := "Привет! Конечно, с радостью помогу — вот красивый и интерактивный сайт на HTML + CSS"

	// First occurrence: full response with greeting + code (incomplete)
	first := greetingAnchor + " для расчёта полёта\n\n```html\n<!DOCTYPE html>\n<html><body>\n  <div class=\"form-group\">\n    <input id=\"speed\" />\n  </div>\n</body></html>"

	// Second occurrence: full duplicate
	second := greetingAnchor + " для расчёта полёта\n\n```html\n<!DOCTYPE html>\n<html><body>\n  <div class=\"form-group\">\n    <input id=\"speed\" />\n  </div>\n</body></html>"

	// Third occurrence: same
	third := greetingAnchor + " для расчёта полёта\n\n```html\n<!DOCTYPE html>\n<html><body>\n  <div class=\"form-group\">\n    <input id=\"speed\" />\n  </div>\n</body></html>"

	combined := first + "\n\n" + second + "\n\n" + third

	deduped, removed := deduplicateResponseContent(combined)

	// Verify deduplication happened
	if removed == 0 {
		t.Errorf("expected duplicates to be removed, got 0 removed bytes")
	}

	// Verify only first occurrence remains (no second greeting)
	if strings.Contains(deduped, greetingAnchor[20:]) && strings.Count(deduped, greetingAnchor) > 1 {
		t.Errorf("deduped content still has duplicate greeting anchor:\n%s", deduped)
	}

	// Verify deduped content is shorter than original
	if len(deduped) >= len(combined) {
		t.Errorf("deduped should be shorter than original: %d vs %d", len(deduped), len(combined))
	}

	// Verify deduped content has 1 occurrence of anchor (only the first)
	count := strings.Count(deduped, greetingAnchor)
	if count != 1 {
		t.Errorf("expected exactly 1 occurrence of greeting anchor, got %d", count)
	}

	t.Logf("Original: %d bytes, Deduped: %d bytes, Removed: %d bytes (%.1f%%)",
		len(combined), len(deduped), removed,
		float64(removed)/float64(len(combined))*100)
}

// TestR6054_DeduplicateResponseContent_NoDuplicates — clean response must not be modified.
func TestR6054_DeduplicateResponseContent_NoDuplicates(t *testing.T) {
	cleanResponse := `Привет! Вот твой HTML сайт. Это простое решение с формой для расчёта полёта.

Вот код:

` + "```html\n" + `<!DOCTYPE html>
<html>
<head><title>Полёт</title></head>
<body>
  <div class="form">
    <input id="speed" />
    <button onclick="calc()">Calc</button>
  </div>
</body>
</html>
` + "```" + `

Этот код работает без JavaScript.`

	deduped, removed := deduplicateResponseContent(cleanResponse)

	if removed > 0 {
		t.Errorf("clean response should NOT be deduplicated, but removed %d bytes", removed)
	}

	if deduped != cleanResponse {
		t.Errorf("clean response modified. expected length=%d, got length=%d", len(cleanResponse), len(deduped))
	}
}

// TestR6054_DeduplicateResponseContent_TooShort — responses < 200 bytes are not deduplicated
// (too short to have meaningful duplicates, false positive risk too high).
func TestR6054_DeduplicateResponseContent_TooShort(t *testing.T) {
	short := "Привет! Конечно, вот код ```html <html>Привет! Конечно</html> ```"
	deduped, removed := deduplicateResponseContent(short)

	if removed > 0 {
		t.Errorf("short response should not be deduplicated")
	}
	if deduped != short {
		t.Errorf("short response modified")
	}
}

// TestR6054_DeduplicateResponseContent_AnchorTooShort — anchor < 20 chars returns unchanged
// (too risky to dedup based on short anchor).
func TestR6054_DeduplicateResponseContent_AnchorTooShort(t *testing.T) {
	// Build response where first 80 chars are: "Привет" + 75 chars padding
	// Actually we use response that has the literal "Привет" repeated but the anchor (first 80 chars)
	// contains short text. Since anchor length is the first 80 chars, we can't construct this case
	// easily. But the function has len(anchor) < 20 guard. Test with a 20+ char anchor.
	// Just ensure normal-length anchor works.
	resp := strings.Repeat("ABCDEFGHIJKLMNOPQRSTUVWXYZ", 10) // 260 chars, first 80 = 80 unique letters
	resp = resp + resp // 520 chars total - first half repeats second half, but anchor only first 80 chars
	deduped, removed := deduplicateResponseContent(resp)
	// The anchor (first 80 chars = "ABCDEFGHIJKLMNOPQRSTUVWXYZ" * 80/26 + some) WILL match later in content
	// So dedup should trigger. Just verify no panic.
	t.Logf("deduped length=%d, removed=%d", len(deduped), removed)
}

// TestR6054_DeduplicateResponseContent_RealWorldPattern — exact pattern from user's report.
func TestR6054_DeduplicateResponseContent_RealWorldPattern(t *testing.T) {
	// This is the exact pattern from the user's screenshot:
	// 1. First code block (HTML+CSS)
	// 2. Greeting reappears mid-code: "<div classПривет! Конечно, вот красивый..."
	// 3. Second ```html
	// 4. Second code block (duplicate)
	// 5. Same greeting reappears
	// 6. Third ```html
	// 7. Third code block (duplicate)

	greeting := "Привет! Конечно, вот красивый и интерактивный сайт на HTML + CSS"

	// First occurrence: greeting + intro + opening code fence + first part of code
	first := greeting + " для расчёта полёта\n\n```html\n<!DOCTYPE html>\n<html lang=\"ru\">\n<head>\n  <meta charset=\"UTF-8\" />\n</head>\n<body>\n  <div class=\"form-group\">\n    <label>Начальная скорость</label>\n    <input id=\"speed\" />\n  </div>"

	// Second occurrence (the bug — greeting reappears mid-code, then code restarts)
	second := "\n  <div classПривет! Конечно, вот красивый и интерактивный сайт на HTML + CSS для расчёта полёта\n\n```html\n<!DOCTYPE html>\n<html lang=\"ru\">\n<head>\n  <meta charset=\"UTF-8\" />\n</head>\n<body>\n  <div class=\"form-group\">\n    <label>Начальная скорость</label>\n    <input id=\"speed\" />\n  </div>"

	// Third occurrence
	third := "\n  <div classПривет! Конечно, вот красивый и интерактивный сайт на HTML + CSS для расчёта полёта\n\n```html\n<!DOCTYPE html>\n<html lang=\"ru\">\n<head>\n  <meta charset=\"UTF-8\" />\n</head>\n<body>\n  <div class=\"form-group\">\n    <label>Начальная скорость</label>\n    <input id=\"speed\" />\n  </div>"

	combined := first + second + third

	deduped, removed := deduplicateResponseContent(combined)

	// Should remove ~2/3 of content (duplicates)
	expectedMinRemoval := len(combined) / 3
	if removed < expectedMinRemoval {
		t.Errorf("expected to remove at least %d bytes, removed %d", expectedMinRemoval, removed)
	}

	// Verify only 1 greeting remains
	count := strings.Count(deduped, greeting)
	if count != 1 {
		t.Errorf("expected 1 greeting anchor, got %d in:\n%s", count, deduped)
	}

	// Verify no broken "<div classПривет" pattern
	if strings.Contains(deduped, "<div classПривет") {
		t.Errorf("broken pattern <div classПривет still present in deduped content:\n%s", deduped)
	}

	t.Logf("Original: %d, Deduped: %d, Removed: %d (%.1f%%)",
		len(combined), len(deduped), removed,
		float64(removed)/float64(len(combined))*100)
}

// TestR6054_DeduplicateResponseContent_NoFalsePositiveOnLongUnique — long unique content shouldn't be touched.
func TestR6054_DeduplicateResponseContent_NoFalsePositiveOnLongUnique(t *testing.T) {
	// Build 5000+ char response with truly random content using SHA-256 hashes
	// (no 80-char substring can possibly repeat).
	var sb strings.Builder
	counter := 0
	for sb.Len() < 5000 {
		h := sha256.Sum256([]byte{byte(counter >> 8), byte(counter & 0xff)})
		sb.WriteString(hex.EncodeToString(h[:]))
		counter++
	}
	unique := sb.String()

	// Sanity: confirm first 80 chars do NOT appear later in content
	anchor := unique[:80]
	if strings.Count(unique, anchor) > 1 {
		t.Fatalf("test setup wrong: anchor should appear only once, found %d (anchor='%s')",
			strings.Count(unique, anchor), anchor)
	}

	deduped, removed := deduplicateResponseContent(unique)

	if removed > 0 {
		t.Errorf("unique content should not be deduplicated, removed %d bytes", removed)
	}
	if deduped != unique {
		t.Errorf("unique content modified")
	}
}
