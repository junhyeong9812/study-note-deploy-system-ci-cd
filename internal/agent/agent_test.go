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
	recorder := request(newTestAgent(t), `{"service":"other","image_tag":"abc"}`)
	if recorder.Code != 422 {
		t.Fatalf("디렉토리 allowlist 밖 서비스 통과: %d", recorder.Code)
	}
}

func TestAgentRejectsShellInjectionTag(t *testing.T) {
	recorder := request(newTestAgent(t), `{"service":"llm","image_tag":"abc; rm -rf /"}`)
	if recorder.Code != 422 || !strings.Contains(recorder.Body.String(), "invalid_tag") {
		t.Fatalf("주입성 태그 통과: %d %s", recorder.Code, recorder.Body.String())
	}
}
