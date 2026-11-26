package agent

// 가림의 패닉은 까닭을 남긴다(agent) — 기준: requirement-spec.md(2026-11-25 admin-incident-assist) §23 의 3.
//
// 정한 계약:
//   - 가림이 패닉하면 로그에 한 건 — `redaction panicked: <패닉 값의 형(%T)> stack=<스택>`. 요청 ID 와 함께(로그 규약의 앞머리),
//     한 줄(줄바꿈 없이 — 스택의 줄은 이어 붙인다)이고, 스택에는 `파일.go:줄` 이 적어도 하나 든다.
//   - 패닉 값의 글 · 입력의 글은 싣지 않는다(패닉 값에 비밀값이 들어 있어도).
//   - agent 의 한 실패에 한 건. 기존 고정 글(detail · 로그의 끝) · 응답은 그대로.

import (
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/junhyeong9812/study-note-deploy-system-ci-cd/internal/shared"
)

// panicLogEntries — "redaction panicked:" 로그 건들(각 건은 다음 시각 머리까지).
func panicLogEntries(logged string) []string {
	entries := []string{}
	for offset := 0; ; {
		start := strings.Index(logged[offset:], "redaction panicked:")
		if start < 0 {
			return entries
		}
		start += offset
		lineStart := strings.LastIndex(logged[:start], "\n") + 1
		entry := logged[lineStart:]
		if next := nextLogEntry.FindStringIndex(entry); next != nil {
			entry = entry[:next[0]]
		}
		entries = append(entries, strings.TrimSuffix(entry, "\n"))
		offset = start + 1
	}
}

func TestRedactionPanicLogsTypeAndStackWithoutText(t *testing.T) {
	for _, testCase := range []struct {
		value    any
		typeName string
	}{
		{"secret Zq81LeakPanicValue", "string"},
		{errors.New("token=Zq81LeakPanicError"), "*errors.errorString"},
	} {
		tailText, _ := secretTail()
		agent := newScriptedAgent(t, failingTest(tailText))
		panicValue := testCase.value
		agent.redact = func(string) shared.CapturedOutput { panic(panicValue) }
		var recorder *httptest.ResponseRecorder
		logged := captureStandardOutput(t, func() { recorder = postAsking(agent, "/agent/ci", "v1") })
		entries := panicLogEntries(logged)
		if len(entries) != 1 {
			t.Fatalf("%s: 패닉의 까닭은 한 건: %d\n%s", testCase.typeName, len(entries), logged)
		}
		entry := entries[0]
		if !strings.Contains(entry, "gh-1:") || !strings.Contains(entry, "redaction panicked: "+testCase.typeName+" stack=") {
			t.Fatalf("로그의 꼴이 다르다(요청 ID · 형 · stack=): %q", entry)
		}
		if strings.Contains(entry, "\n") || !strings.Contains(entry[strings.Index(entry, "stack="):], ".go:") {
			t.Fatalf("스택은 한 줄로, 파일.go:줄 을 담아: %q", entry)
		}
		if strings.Contains(logged, "Zq81") {
			t.Fatalf("패닉 값 · 입력의 글이 로그에 실렸다:\n%s", logged)
		}
		if envelope := decodeStageEnvelope(t, recorder.Body.Bytes()); envelope.Error.Detail != shared.DetailRedactionFailed || envelope.Error.Code != "ci_test" {
			t.Fatalf("응답은 그대로(고정 글): %s", recorder.Body.String())
		}
	}
}
