package master

// 배포 대상 claude(Claude 실행 자리) — 기준: requirement-spec.md(2026-11-25 claude-fallback) §7 · §10 끝 "배포의 선행".
//
// 정한 계약:
//   - master 의 서비스 허용 목록에 claude 가 든다 — AGENT_URL_CLAUDE 가 있으면 접수(GitHub 워크플로의 본문 · 사람이 고른 재배포)를 받고
//     그 agent 를 부른다. 이름은 정확히 claude — Claude · CLAUDE · claude-runner · claudes 는 422 unknown_service(지금의 규칙).
//   - 서비스 → agent 의 대응은 설정(AGENT_URL_CLAUDE)이다 — 코드에 주소가 없다. 실행 자리는 backend 와 같은 서버의 agent 로 간다
//     (.env.example 에 같은 자리 표시 — service_documents_test.go).
//   - 조회(GET /status 의 services · history, GET /deploys?service=claude, GET /deploys/{id})에 다른 서비스와 같은 꼴로 나온다.
//   - agent 가 cd_health 로 실패하면(토큰이 없어 503) failed — 성공으로 새지 않는다. 이력 · 알림 · 로그의 끝부분은 가린다(12-2b).

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/junhyeong9812/study-note-deploy-system-ci-cd/internal/shared"
)

// claudeAgent — 받은 단계 요청의 서비스를 기록하고, 정해 두면 그 경로를 실패시킨다.
type claudeAgent struct {
	mutex    sync.Mutex
	services []string
	failPath string
	failBody string
}

func newClaudeAgentServer(t *testing.T) *claudeAgent {
	agent := &claudeAgent{}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var payload struct {
			Service string `json:"service"`
		}
		body, _ := io.ReadAll(request.Body)
		json.Unmarshal(body, &payload)
		agent.mutex.Lock()
		agent.services = append(agent.services, payload.Service+request.URL.Path)
		failPath, failBody := agent.failPath, agent.failBody
		agent.mutex.Unlock()
		if request.URL.Path == failPath {
			writer.WriteHeader(http.StatusInternalServerError)
			io.WriteString(writer, failBody)
			return
		}
		shared.WriteOK(writer, map[string]any{"took_ms": 1})
	}))
	t.Cleanup(server.Close)
	t.Setenv("AGENT_URL_CLAUDE", server.URL)
	t.Setenv("AGENT_URL_LLM", server.URL)
	return agent
}

func (agent *claudeAgent) called() string {
	agent.mutex.Lock()
	defer agent.mutex.Unlock()
	return strings.Join(agent.services, ",")
}

func submitClaude(master *Master, requestID, extra string) *httptest.ResponseRecorder {
	return call(master, "POST", "/deploy", `{"service":"claude","commit_sha":"`+workflowSha+`","request_id":"`+requestID+`"`+extra+`}`)
}

func TestClaudeSubmissionIsAcceptedAndDeployed(t *testing.T) {
	agent := newClaudeAgentServer(t)
	master := newTestMaster(t)
	requireExactResponse(t, submitClaude(master, "gh-c1-1", ""), http.StatusAccepted,
		`{"data":{"request_id":"gh-c1-1","service":"claude","state":"accepted"},"success":true}`) // 실행 자리의 deploy.yml 이 보내는 본문
	if deployment := waitSettled(t, master, "gh-c1-1"); deployment.State != StateSucceeded || deployment.Service != "claude" || deployment.CommitSha != workflowSha {
		t.Fatalf("claude 의 배포가 성공으로 끝나야 한다: %+v", deployment)
	}
	if agent.called() != "claude/agent/ci,claude/agent/cd" {
		t.Fatalf("claude 의 agent 에 CI 다음 CD 를 불러야 한다: %s", agent.called())
	}
	if recorder := submitClaude(master, "gh-c1-2", `,"requested_by":"admin:operator"`); recorder.Code != http.StatusAccepted { // 판을 골라 다시 배포
		t.Fatalf("사람이 고른 재배포도 받는다: %d %s", recorder.Code, recorder.Body.String())
	}
	waitSettled(t, master, "gh-c1-2")
}

func TestClaudeLookalikesAreRejected(t *testing.T) {
	newClaudeAgentServer(t)
	master := newTestMaster(t)
	for _, name := range []string{"Claude", "CLAUDE", "claude-runner", "claudes", "claude ", " claude", "evil"} {
		recorder := call(master, "POST", "/deploy", `{"service":"`+name+`","commit_sha":"`+workflowSha+`","request_id":"gh-c2-1"}`)
		if recorder.Code != 422 || !strings.Contains(recorder.Body.String(), "unknown_service") {
			t.Fatalf("%q 는 허용 목록 밖이다: %d %s", name, recorder.Code, recorder.Body.String())
		}
	}
}

func TestClaudeIsRejectedWithoutAgentURL(t *testing.T) {
	t.Setenv("AGENT_URL_CLAUDE", "")
	master := newTestMaster(t)
	if recorder := submitClaude(master, "gh-c3-1", ""); recorder.Code != 422 || !strings.Contains(recorder.Body.String(), "unknown_service") {
		t.Fatalf("agent 주소가 설정되지 않은 서비스는 지금처럼 거절한다: %d %s", recorder.Code, recorder.Body.String())
	}
}

func sortedKeys(raw map[string]json.RawMessage) string {
	keys := make([]string, 0, len(raw))
	for key := range raw {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return strings.Join(keys, ",")
}

func TestClaudeAppearsInLookupsLikeOtherServices(t *testing.T) {
	newClaudeAgentServer(t)
	master := newTestMaster(t)
	call(master, "POST", "/deploy", `{"service":"llm","commit_sha":"`+workflowSha+`","request_id":"gh-c4-llm"}`)
	waitSettled(t, master, "gh-c4-llm")
	submitClaude(master, "gh-c4-claude", "")
	waitSettled(t, master, "gh-c4-claude")

	var status struct {
		Data struct {
			Services []string                     `json:"services"`
			History  []map[string]json.RawMessage `json:"history"`
		} `json:"data"`
	}
	json.Unmarshal(call(master, "GET", "/status", "").Body.Bytes(), &status)
	sort.Strings(status.Data.Services)
	if strings.Join(status.Data.Services, ",") != "claude,llm" {
		t.Fatalf("상태의 서비스 목록에 claude 가 다른 서비스와 함께 나온다: %v", status.Data.Services)
	}
	if len(status.Data.History) != 2 || sortedKeys(status.Data.History[0]) != sortedKeys(status.Data.History[1]) ||
		string(status.Data.History[0]["service"]) != `"claude"` {
		t.Fatalf("상태의 이력에서 claude 의 줄이 다른 서비스와 같은 꼴이다: %v", status.Data.History)
	}
	listing := call(master, "GET", "/deploys?service=claude", "")
	if listing.Code != 200 || !strings.Contains(listing.Body.String(), `"request_id":"gh-c4-claude"`) || strings.Contains(listing.Body.String(), "gh-c4-llm") {
		t.Fatalf("서비스로 고른 이력에 claude 만 나온다: %s", listing.Body.String())
	}
	if single := call(master, "GET", "/deploys/gh-c4-claude", ""); single.Code != 200 || !strings.Contains(single.Body.String(), `"service":"claude"`) {
		t.Fatalf("단건 조회: %d %s", single.Code, single.Body.String())
	}
}

func TestClaudeUnhealthyDeploymentFailsWithMaskedTail(t *testing.T) {
	alerts := newAlertRecorder(t, http.StatusAccepted)
	agent := newClaudeAgentServer(t)
	token := "sk-" + "ant-oat01-" + "Zq81" + strings.Repeat("aB3", 25)
	agent.failPath = "/agent/cd"
	agent.failBody = failureBody("cd_health", "env CLAUDE_CODE_OAUTH_TOKEN="+token+"\nhealth 미도달: status 503")
	master := newTestMaster(t)
	var deployment Deployment
	logged := captureMasterLog(t, func() {
		submitClaude(master, "gh-c5-1", "")
		deployment = waitSettled(t, master, "gh-c5-1")
	})
	if deployment.State != StateFailed || deployment.FailedStage != "cd_health" || !strings.HasSuffix(deployment.Detail, "health 미도달: status 503") {
		t.Fatalf("헬스 503 은 실패로 끝난다(성공으로 새지 않는다): %+v", deployment)
	}
	events := alerts.waitEventsOfKind("deploy_failed", 1, 2*time.Second)
	if len(events) != 1 || events[0]["title"] != "배포 실패 claude — 단계 cd_health" || events[0]["key"] != "deploy-failed:claude:"+workflowSha {
		t.Fatalf("claude 의 배포 실패 알림이 다른 서비스와 같은 꼴이다: %v", events)
	}
	for where, text := range map[string]string{"이력": deployment.Detail, "알림": events[0]["detail"], "로그": logged} {
		if strings.Contains(text, "Zq81") || strings.Contains(text, "aB3aB3aB3") {
			t.Fatalf("%s 에 구독 토큰이 남았다:\n%s", where, text)
		}
	}
	if !strings.Contains(deployment.Detail, "CLAUDE_CODE_OAUTH_TOKEN=[가림:") {
		t.Fatalf("토큰 줄은 이름을 남기고 값을 가린다: %q", deployment.Detail)
	}
}
