package shared

// 실패 글의 끝부분 — 이력 · 알림 · 로그 · agent 의 실패 봉투에 싣는 끝부분은 가린 글에서 뗀다(§22).
// 계약: failure_detail_test.go 머리의 주석.

import (
	"fmt"
	"runtime/debug"
	"strings"
	"unicode/utf8"
)

// DetailRedactionFailed — 가림이 실패(패닉)했을 때 끝부분 대신 싣는 글.
const DetailRedactionFailed = "[가림 실패 — 끝부분을 싣지 않았다]"

// CutTail — 가린 글의 끝 limit 바이트 이하를 뗀다(limit 이 0 이하면 빈 글). 진짜 표시나 글자의 가운데에서 자르지 않는다:
// 자르는 자리가 진짜 표시(isExactMask — `[가림:<아는 종류>]`) 안이면 그 표시를 통째로 버리고(표시 뒤에서), 글자의 가운데면 다음 글자의 처음에서
// 시작한다. 그 밖의 `[가림:`(입력에 든 가짜)은 보통 글이다.
func CutTail(masked string, limit int) string {
	if limit <= 0 {
		return ""
	}
	if len(masked) <= limit {
		return masked
	}
	start := len(masked) - limit
	window := masked[:min(len(masked), start+len(maskPrefix)-1)] // 자리가 표시의 머리("[가림:") 안에 떨어진 것도 찾는다
	if open := strings.LastIndex(window, maskPrefix); open >= 0 && open < start {
		if closing := strings.IndexByte(masked[open:], ']'); closing >= 0 && start <= open+closing && isExactMask([]byte(masked[open:open+closing+1])) {
			start = open + closing + 1
		}
	}
	for start < len(masked) && !utf8.RuneStart(masked[start]) {
		start++
	}
	return masked[start:]
}

// RedactSafely — redact 를 부르되 패닉을 받아 낸다(redacted=false). 가림이 깨져도 배포의 응답 · 기록은 이어져야 한다 —
// 부르는 쪽은 끝부분 대신 DetailRedactionFailed 를 싣고, 가리지 않은 글은 어디에도 싣지 않는다.
// panicReason 은 부르는 쪽이 요청 ID 와 함께 로그에 싣는 까닭 `<패닉 값의 형> stack=<한 줄 스택>` — 패닉 값의 글은 싣지 않는다(입력이 섞일 수 있다).
func RedactSafely(redact func(raw string) CapturedOutput, raw string) (captured CapturedOutput, redacted bool, panicReason string) {
	defer func() {
		if recovered := recover(); recovered != nil {
			stack := strings.Join(strings.Fields(string(debug.Stack())), " ")
			captured, redacted, panicReason = CapturedOutput{}, false, fmt.Sprintf("%T stack=%s", recovered, stack)
		}
	}()
	return redact(raw), true, ""
}
