package master

// 12-2b 리뷰 루프 1 의 반영(master) — 기준: requirement-spec.md(2026-11-25 admin-incident-assist) §23 의 1 · 2 · 3.
//
// 정한 계약:
//   - 가짜 표시(`[가림: …` 와 먼 `]`)가 든 detail 도 이력 · 알림 · 로그의 끝부분이 비지 않는다.
//   - agent 가 보낸 detail 이 정확히 shared.DetailRedactionFailed 이고 output 이 없으면 실패 출력을 남기지 않는다(조회 404 output_not_recorded ·
//     폴더에 파일 없음). 이력 · 알림 · 로그에는 그 고정 글이 실린다. 앞에 다른 글이 있으면 보통 detail 이다(남긴다).
//   - 가림이 패닉하면 가림을 부른 때마다 로그 한 건 `redaction panicked: <형> stack=<한 줄 스택>`(요청 ID 와 함께 · 패닉 값 · 입력의 글 없음):
//     agent 단계의 실패는 두 건(실패 출력을 남길 때 · 끝부분을 뗄 때), master 단계의 실패(agent_unreachable 등)는 한 건.

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/junhyeong9812/study-note-deploy-system-ci-cd/internal/shared"
)

func TestFakeMaskInDetailDoesNotEmptyTheTails(t *testing.T) {
	alerts := newAlertRecorder(t, http.StatusAccepted)
	plain := strings.Repeat("error: connection reset by peer while pulling layer\n", 60)
	scriptedAgent(t, map[string]string{"/agent/ci": failureBody("ci_test", "[가림: "+plain+"]")}) // 옛 agent 의 가리지 않은 detail
	master := newTestMaster(t)
	result := runFailure(t, master, alerts, "gh-r1-1")
	prefix := "deploy failed llm sha=" + deploySha + " stage=ci_test "
	if len(result.deployment.Detail) < 1900 || len(result.alert["detail"]) < 700 || len(result.logEntry)-len(prefix) < 150 {
		t.Fatalf("가짜 표시 때문에 끝부분이 비었다: 이력 %d · 알림 %d · 로그 %d", len(result.deployment.Detail), len(result.alert["detail"]), len(result.logEntry)-len(prefix))
	}
	if !strings.HasSuffix(result.deployment.Detail, "while pulling layer\n]") {
		t.Fatalf("이력의 끝이 다르다: %q", result.deployment.Detail[len(result.deployment.Detail)-40:])
	}
}

func TestAgentRedactionFailureIsNotKeptAsOutput(t *testing.T) {
	alerts := newAlertRecorder(t, http.StatusAccepted)
	agent := newOutputAgent(t)
	master, directory, _ := newOutputMaster(t)
	agent.script("llm", outputScript{path: "/agent/ci", code: "ci_test", detail: shared.DetailRedactionFailed}) // 가림이 실패한 agent — output 없음
	var deployment Deployment
	logged := captureMasterLog(t, func() {
		submitService(master, "llm", "gh-r2-1")
		deployment = waitSettled(t, master, "gh-r2-1")
	})
	requireFailure(t, lookupOutput(master, "gh-r2-1"), http.StatusNotFound, "output_not_recorded")
	if _, err := os.Stat(filepath.Join(directory, "gh-r2-1.json")); !os.IsNotExist(err) {
		t.Fatalf("고정 글을 실패 출력으로 남겼다: %v", err)
	}
	if deployment.Detail != shared.DetailRedactionFailed || !strings.HasSuffix(failedLogEntry(t, logged), shared.DetailRedactionFailed) {
		t.Fatalf("이력 · 로그에는 고정 글이 그대로 실린다: %q", deployment.Detail)
	}
	events := alerts.waitEventsOfKind("deploy_failed", 1, 2*time.Second)
	if len(events) != 1 || events[0]["detail"] != "커밋 "+deploySha+"\n"+shared.DetailRedactionFailed {
		t.Fatalf("알림에는 고정 글이 그대로 실린다: %v", events)
	}
	for requestID, detail := range map[string]string{
		"gh-r2-2": "exit status 1",                                // 보통의 detail — 지금처럼 남긴다
		"gh-r2-3": "step output\n" + shared.DetailRedactionFailed, // 끝이 우연히 그 글 — 정확히 같지 않으면 남긴다
	} {
		agent.script("llm", outputScript{path: "/agent/ci", code: "ci_test", detail: detail})
		submitService(master, "llm", requestID)
		waitSettled(t, master, requestID)
		if record := decodeOutputRecord(t, lookupOutput(master, requestID)); record.Output != detail {
			t.Fatalf("%s: 보통 detail 은 출력으로 남긴다: %q", requestID, record.Output)
		}
	}
}

// masterPanicEntries — "redaction panicked:" 로그 건들.
func masterPanicEntries(logged string) []string {
	entries := []string{}
	for offset := 0; ; {
		start := strings.Index(logged[offset:], "redaction panicked:")
		if start < 0 {
			return entries
		}
		start += offset
		lineStart := strings.LastIndex(logged[:start], "\n") + 1
		entry := logged[lineStart:]
		if next := nextMasterLogEntry.FindStringIndex(entry); next != nil {
			entry = entry[:next[0]]
		}
		entries = append(entries, strings.TrimSuffix(entry, "\n"))
		offset = start + 1
	}
}

func requirePanicEntries(t *testing.T, logged, requestID, typeName string, count int) {
	t.Helper()
	entries := masterPanicEntries(logged)
	if len(entries) != count {
		t.Fatalf("패닉의 까닭은 %d건이어야 한다: %d\n%s", count, len(entries), logged)
	}
	for _, entry := range entries {
		if !strings.Contains(entry, requestID+":") || !strings.Contains(entry, "redaction panicked: "+typeName+" stack=") ||
			strings.Contains(entry, "\n") || !strings.Contains(entry[strings.Index(entry, "stack="):], ".go:") {
			t.Fatalf("로그의 꼴이 다르다(요청 ID · 형 · 한 줄 스택): %q", entry)
		}
	}
	if strings.Contains(logged, "Zq81") {
		t.Fatalf("패닉 값 · 입력의 글이 로그에 실렸다:\n%s", logged)
	}
}

func TestMasterRedactionPanicLogsTypeAndStackWithoutText(t *testing.T) {
	newAlertRecorder(t, http.StatusAccepted)
	agent := newOutputAgent(t)
	master, _, _ := newOutputMaster(t)
	master.redact = func(string) shared.CapturedOutput { panic(errors.New("token=Zq81LeakPanicValue")) }
	agent.script("llm", outputScript{path: "/agent/ci", code: "ci_test", detail: "password=Zq81LeakDetail", output: rawOutput("password=Zq81LeakOutput")})
	var deployment Deployment
	logged := captureMasterLog(t, func() {
		submitService(master, "llm", "gh-r3-1")
		deployment = waitSettled(t, master, "gh-r3-1")
	})
	requirePanicEntries(t, logged, "gh-r3-1", "*errors.errorString", 2) // 실패 출력 · 끝부분 — 가림을 부른 때마다
	if deployment.FailedStage != "ci_test" || deployment.Detail != shared.DetailRedactionFailed {
		t.Fatalf("결과 · 고정 글은 그대로: %+v", deployment)
	}
}

func TestMasterStagePanicLogsOnce(t *testing.T) {
	newAlertRecorder(t, http.StatusAccepted)
	t.Setenv("AGENT_URL_LLM", "http://127.0.0.1:1")
	master := newTestMaster(t)
	master.redact = func(string) shared.CapturedOutput { panic("Zq81LeakPanicString") }
	var deployment Deployment
	logged := captureMasterLog(t, func() {
		submit(master, "gh-r4-1")
		deployment = waitSettled(t, master, "gh-r4-1")
	})
	requirePanicEntries(t, logged, "gh-r4-1", "string", 1)
	if deployment.FailedStage != StageAgentUnreachable || deployment.Detail != shared.DetailRedactionFailed {
		t.Fatalf("master 단계의 실패도 결과 · 고정 글은 그대로: %+v", deployment)
	}
}
