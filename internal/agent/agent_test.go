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
	return New(shared.NewLogger())
}

func request(agent *Agent, body string) *httptest.ResponseRecorder {
	mux := http.NewServeMux()
	agent.Register(mux)
	req := httptest.NewRequest("POST", "/agent/deploy", strings.NewReader(body))
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
