package shared

// CutTail 은 진짜 표시만 표시로 본다 · limit 0 이하 — 기준: requirement-spec.md(2026-11-25 admin-incident-assist) §23 의 1 · 4.
//
// 정한 계약:
//   - 진짜 표시 = `[가림:<아는 종류>]`(token · hex · pem · authorization · secret_field · url_credential · private_ip · email)가 정확히 맞는 것.
//     자르는 자리가 진짜 표시의 가운데면 그 표시를 통째로 버린다(지금대로). 그 밖의 `[가림:`(공백 · 모르는 종류 · 닫는 `]` 없음 · 먼 `]`)은
//     보통 글이다 — 바이트로(글자의 경계로) 자른다. 가림이 없는 글은 옛 lastPart 와 바이트까지 같다(여러 바이트 글자의 가운데만 빼고).
//   - limit 이 0 이하면 빈 글(패닉 없음).

import (
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"
)

var realMask = regexp.MustCompile(`\[가림:(?:token|hex|pem|authorization|secret_field|url_credential|private_ip|email)\]`)

// byteCut — 옛 lastPart 에 글자 경계만 더한 자르기(가짜 표시는 보통 글).
func byteCut(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	start := len(text) - limit
	for start < len(text) && !utf8.RuneStart(text[start]) {
		start++
	}
	return text[start:]
}

func TestCutTailTreatsOpenBracketWithFarClosingAsPlainText(t *testing.T) {
	plain := strings.Repeat("error: connection reset by peer while pulling layer\n", 80) // 4000바이트 남짓
	captured := CaptureOutput("[가림: " + plain + "]")
	got := CutTail(captured.Text, 2000)
	if got != captured.Text[len(captured.Text)-2000:] {
		t.Fatalf("`[가림: ` 다음의 먼 `]` 때문에 끝부분이 비면 안 된다: 길이 %d", len(got))
	}
}

func TestCutTailOnPlainTextMatchesOldLastPart(t *testing.T) {
	text := strings.Repeat("x", 20) + "[가림:literal]" + strings.Repeat("y", 195)
	if got := CutTail(text, 200); got != text[len(text)-200:] {
		t.Fatalf("가림이 없는 글의 끝부분은 옛 lastPart 와 같다:\n got %q\nwant %q", got, text[len(text)-200:])
	}
	for _, fake := range []string{"[가림:unknown]", "[가림:token", "[가림:", "[가림:TOKEN]", "[가림: token]", "[가림:token ]"} {
		text := strings.Repeat("a", 30) + fake + strings.Repeat("b", 30)
		for limit := 1; limit <= len(text); limit++ {
			if got, want := CutTail(text, limit), byteCut(text, limit); got != want {
				t.Fatalf("가짜 표시 %q 는 보통 글(limit %d):\n got %q\nwant %q", fake, limit, got, want)
			}
		}
	}
}

func TestCutTailSkipsOnlyRealMasksInMixedText(t *testing.T) {
	text := "head " + mask("token") + " [가림:literal] mid [가림:secret_fieldX] " + mask("secret_field") + " [가림: x] " + mask("email") + " tail [가림:"
	spans := realMask.FindAllStringIndex(text, -1)
	if len(spans) != 3 {
		t.Fatalf("시험 전제: 진짜 표시 셋: %v", spans)
	}
	for limit := 1; limit <= len(text)+1; limit++ {
		got := CutTail(text, limit)
		start := len(text) - len(got)
		if !strings.HasSuffix(text, got) || len(got) > limit || !utf8.ValidString(got) {
			t.Fatalf("limit %d: 끝 limit 바이트 이하의 suffix · 올바른 UTF-8: %q", limit, got)
		}
		plainStart := len(text) - len(byteCut(text, limit))
		insideReal := false
		for _, span := range spans {
			if start > span[0] && start < span[1] {
				t.Fatalf("limit %d: 진짜 표시의 가운데에서 시작했다: %q", limit, got)
			}
			if plainStart > span[0] && plainStart < span[1] {
				insideReal = true
				if start != span[1] {
					t.Fatalf("limit %d: 진짜 표시의 가운데면 그 표시만 버린다: 시작 %d, 기대 %d", limit, start, span[1])
				}
			}
		}
		if !insideReal && start != plainStart {
			t.Fatalf("limit %d: 진짜 표시가 아닌 자리에서는 보통 글처럼 자른다(가짜 표시 때문에 더 버리지 않는다): 시작 %d, 기대 %d", limit, start, plainStart)
		}
	}
}

func TestCutTailWithZeroOrNegativeLimitIsEmpty(t *testing.T) {
	for _, limit := range []int{0, -1, -2000} {
		for _, text := range []string{"", "abc", mask("token") + "tail"} {
			func() {
				defer func() { // 패닉이 시험 프로세스를 죽여 다른 시험의 결과를 가리지 않게
					if recovered := recover(); recovered != nil {
						t.Errorf("limit %d 에서 패닉: %v", limit, recovered)
					}
				}()
				if got := CutTail(text, limit); got != "" {
					t.Errorf("limit %d 는 빈 글: %q", limit, got)
				}
			}()
		}
	}
}
