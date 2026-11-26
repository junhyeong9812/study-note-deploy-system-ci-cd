package shared

// 실패 글의 끝부분 — 기준: requirement-spec.md(2026-11-25 admin-incident-assist) §22.
//
// 정한 계약(CutTail):
//   - 가린 글(CaptureOutput 의 Text)에서 끝 limit 바이트 이하를 뗀다. limit 이하면 그대로.
//   - 자르는 자리가 가림 표시(`[가림:<종류>]`)의 가운데면 그 표시를 통째로 버린다(표시 뒤에서 시작 — 반쪽 표시가 없고, limit 을 넘지 않는다).
//   - 자르는 자리가 글자(UTF-8)의 가운데면 다음 글자의 처음에서 시작한다. 옛 lastPart 는 바이트로 잘라 깨진 UTF-8 을 남길 수 있었다.
//   - 결과는 늘 원래 글의 끝(suffix)이다.

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestCutTailKeepsShortTextAndCutsPlainTextByBytes(t *testing.T) {
	if got := CutTail("short text", 2000); got != "short text" {
		t.Fatalf("limit 이하는 그대로: %q", got)
	}
	plain := strings.Repeat("collected ok\n", 300)
	if got := CutTail(plain, 2000); got != plain[len(plain)-2000:] {
		t.Fatalf("가림 · 여러 바이트 글자가 없는 글은 끝 limit 바이트 그대로(옛 꼴과 같다): 길이 %d", len(got))
	}
}

func TestCutTailNeverStartsInsideAMask(t *testing.T) {
	text := strings.Repeat("a", 30) + mask("secret_field") + strings.Repeat("b", 7) + mask("token") + strings.Repeat("c", 5)
	for limit := 1; limit <= len(text)+2; limit++ {
		got := CutTail(text, limit)
		if len(got) > limit || !strings.HasSuffix(text, got) {
			t.Fatalf("limit %d: 끝 limit 바이트 이하의 suffix 여야 한다: %q", limit, got)
		}
		stripped := anyMask.ReplaceAllString(got, "")
		if strings.ContainsAny(stripped, "[]:") || strings.Contains(stripped, "가림") || strings.Contains(stripped, "secret_field") || strings.Contains(stripped, "token") {
			t.Fatalf("limit %d: 표시의 반쪽이 남았다: %q", limit, got)
		}
		if limit <= len(text) && len(got) < limit-len(mask("secret_field")) {
			t.Fatalf("limit %d: 표시 하나보다 많이 버렸다: %d바이트", limit, len(got))
		}
	}
}

func TestCutTailStartsOnACharacterBoundary(t *testing.T) {
	text := strings.Repeat("가나다🙂a", 400)
	for limit := 1; limit <= 60; limit++ {
		got := CutTail(text, limit)
		if !utf8.ValidString(got) || len(got) > limit || len(got) < limit-3 || !strings.HasSuffix(text, got) {
			t.Fatalf("limit %d: 글자의 경계에서 시작해야 한다(옛 바이트 자르기는 %q 처럼 깨졌다): %q", limit, text[len(text)-limit:], got)
		}
	}
}
