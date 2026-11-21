package agent

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/junhyeong9812/study-note-deploy-system-ci-cd/internal/shared"
)

func newTestAgent(t *testing.T) *Agent {
	t.Setenv("DEPLOY_SECRET", "test-value")
	t.Setenv("DEPLOY_DIR_LLM", t.TempDir())
	t.Setenv("DEPLOY_SVC_LLM", "wrapper")
	return New(shared.NewLogger())
}

func request(agent *Agent, body string) *httptest.ResponseRecorder {
	mux := http.NewServeMux()
	agent.Register(mux)
	req := httptest.NewRequest("POST", "/agent/ci", strings.NewReader(body))
	req.Header.Set(shared.SecretHeader, "test-value")
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, req)
	return recorder
}

func TestAgentRejectsUnknownService(t *testing.T) {
	recorder := request(newTestAgent(t), `{"service":"other","commit_sha":"abc1234"}`)
	if recorder.Code != 422 {
		t.Fatalf("디렉토리 allowlist 밖 서비스 통과: %d", recorder.Code)
	}
}

func TestAgentRejectsNonHexSha(t *testing.T) {
	recorder := request(newTestAgent(t), `{"service":"llm","commit_sha":"abc; rm -rf /"}`)
	if recorder.Code != 422 || !strings.Contains(recorder.Body.String(), "invalid_sha") {
		t.Fatalf("비hex sha 통과: %d %s", recorder.Code, recorder.Body.String())
	}
}

func TestAgentLoadsObserveDeploySettings(t *testing.T) {
	directory := t.TempDir()
	t.Setenv("DEPLOY_DIR_OBSERVE", directory)
	t.Setenv("DEPLOY_SVC_OBSERVE", "observe")
	t.Setenv("DEPLOY_MODE_OBSERVE", "bluegreen")
	t.Setenv("DEPLOY_HEALTH_OBSERVE", "http://127.0.0.1:17000/health")
	t.Setenv("DEPLOY_HEALTH_OBSERVE_BLUE", "http://127.0.0.1:17001/health")
	t.Setenv("DEPLOY_HEALTH_OBSERVE_GREEN", "http://127.0.0.1:17002/health")
	agent := newTestAgent(t)
	if agent.directories["observe"] != directory || agent.composeServices["observe"] != "observe" || agent.modes["observe"] != "bluegreen" ||
		agent.healthURLs["observe"] != "http://127.0.0.1:17000/health" ||
		agent.slotHealthURLs["observe"][slotBlue] != "http://127.0.0.1:17001/health" ||
		agent.slotHealthURLs["observe"][slotGreen] != "http://127.0.0.1:17002/health" {
		t.Fatalf("observe 배포 설정을 읽지 못했다: dir=%q svc=%q mode=%q health=%q slots=%v",
			agent.directories["observe"], agent.composeServices["observe"], agent.modes["observe"],
			agent.healthURLs["observe"], agent.slotHealthURLs["observe"])
	}
	if agent.slotDrainURLs["observe"][slotBlue] != "" || agent.slotDrainURLs["observe"][slotGreen] != "" {
		t.Fatalf("설정하지 않은 정리 알림 경로가 생겼다: %v", agent.slotDrainURLs["observe"])
	}
	if recorder := request(agent, `{"service":"observer","commit_sha":"abc1234"}`); recorder.Code != 422 ||
		!strings.Contains(recorder.Body.String(), "unknown_service") {
		t.Fatalf("목록 밖 서비스가 통과: %d %s", recorder.Code, recorder.Body.String())
	}
}
