package master

// 특성 테스트 — 배포 실패의 전체 출력을 더하기 전, 실패 한 건이 이력 · 알림 · 로그에 남기는 것을 그대로 적었다.
// 기준: requirement-spec.md(2026-11-25 admin-incident-assist) §15 "출력을 남기지 못해도 배포의 결과 · 이력 · 알림은 그대로다".
//   - 이력의 detail = agent 가 준 detail 의 끝 2000바이트 · 알림 = 제목 "배포 실패 <서비스> — 단계 <단계>", 몸 "커밋 <sha>\n<detail 의 끝 800바이트>"
//   - 로그 = "deploy failed <서비스> sha=<sha> stage=<단계> <detail 의 끝 200바이트>"
//   - agent 의 실패 봉투에 모르는 칸(error.output)이 더해져도 단계 · detail 은 그대로다.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/junhyeong9812/study-note-deploy-system-ci-cd/internal/shared"
)

// scriptedAgent — /agent/ci · /agent/cd 의 응답 본문을 경로마다 그대로 정해 둔다(정하지 않은 경로는 성공).
func scriptedAgent(t *testing.T, responses map[string]string) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		io.Copy(io.Discard, request.Body)
		if body, scripted := responses[request.URL.Path]; scripted {
			writer.Header().Set("Content-Type", "application/json; charset=utf-8")
			writer.WriteHeader(http.StatusInternalServerError)
			io.WriteString(writer, body)
			return
		}
		shared.WriteOK(writer, map[string]any{"took_ms": 1})
	}))
	t.Cleanup(server.Close)
	t.Setenv("AGENT_URL_LLM", server.URL)
}

// captureMasterLog — 그동안 표준 출력에 찍힌 로그 줄.
func captureMasterLog(t *testing.T, run func()) string {
	t.Helper()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	original := os.Stdout
	os.Stdout = writer
	captured := make(chan string)
	go func() {
		content, _ := io.ReadAll(reader)
		captured <- string(content)
	}()
	func() {
		defer func() { os.Stdout = original }()
		run()
	}()
	writer.Close()
	return <-captured
}

func longDetail() string {
	var builder strings.Builder
	for number := 0; builder.Len() < 3000; number++ {
		builder.WriteString("collected ")
		builder.WriteString(strings.Repeat("x", number%7))
		builder.WriteString("\n")
	}
	return builder.String() + "FAILED test_queue.py::test_order"
}

func failureBody(code, detail string) string {
	return `{"success":false,"error":{"code":"` + code + `","detail":` + quoteJSON(detail) + `}}`
}

func quoteJSON(value string) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

func TestFailedDeploymentKeepsDetailAlertAndLogAsBefore(t *testing.T) {
	alerts := newAlertRecorder(t, http.StatusAccepted)
	detail := longDetail()
	scriptedAgent(t, map[string]string{"/agent/ci": failureBody("ci_test", detail)})
	master := newTestMaster(t)
	var deployment Deployment
	logged := captureMasterLog(t, func() {
		submit(master, "gh-c1-1")
		deployment = waitSettled(t, master, "gh-c1-1")
	})
	if deployment.State != StateFailed || deployment.FailedStage != "ci_test" || deployment.Detail != detail[len(detail)-2000:] {
		t.Fatalf("이력은 단계 ci_test · detail 의 끝 2000바이트: stage=%s detail 길이 %d", deployment.FailedStage, len(deployment.Detail))
	}
	events := alerts.waitEventsOfKind("deploy_failed", 1, 2*time.Second)
	if len(events) != 1 || events[0]["title"] != "배포 실패 llm — 단계 ci_test" || events[0]["detail"] != "커밋 "+deploySha+"\n"+detail[len(detail)-800:] ||
		events[0]["key"] != "deploy-failed:llm:"+deploySha || events[0]["severity"] != "warning" {
		t.Fatalf("배포 실패 알림이 바뀌었다: %v", events)
	}
	if !strings.Contains(logged, "gh-c1-1:") || !strings.Contains(logged, "deploy failed llm sha="+deploySha+" stage=ci_test "+detail[len(detail)-200:]) {
		t.Fatalf("배포 실패 로그 줄이 바뀌었다:\n%s", logged)
	}
}

func TestAgentFailureWithUnknownOutputFieldKeepsStageAndDetail(t *testing.T) {
	body := `{"success":false,"error":{"code":"ci_build","detail":"exit status 2","output":{"text":"whole build output","truncated":false,"omitted_bytes":0,"redactions":{}}}}`
	scriptedAgent(t, map[string]string{"/agent/ci": body})
	master := newTestMaster(t)
	submit(master, "gh-c2-1")
	if deployment := waitSettled(t, master, "gh-c2-1"); deployment.FailedStage != "ci_build" || deployment.Detail != "exit status 2" {
		t.Fatalf("봉투에 칸이 더해져도 단계 · detail 은 그대로: %+v", deployment)
	}
}

func TestFailedDeploymentLookupFieldsStayTheSame(t *testing.T) {
	scriptedAgent(t, map[string]string{"/agent/cd": failureBody("cd_health", "health 미도달: status 503")})
	master := newTestMaster(t)
	submit(master, "gh-c3-1")
	waitSettled(t, master, "gh-c3-1")
	single := call(master, "GET", "/deploys/gh-c3-1", "")
	keys := dataKeys(t, single)
	sort.Strings(keys)
	if strings.Join(keys, ",") != "accepted_at,cd_ms,ci_ms,commit_sha,detail,failed_stage,finished_at,request_id,requested_by,service,state" {
		t.Fatalf("실패한 배포의 단건 조회 칸이 바뀌었다: %s", single.Body.String())
	}
}
