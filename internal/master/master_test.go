package master

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/junhyeong9812/study-note-deploy-system-ci-cd/internal/shared"
)

func newTestMaster(t *testing.T) *Master {
	t.Setenv("DEPLOY_SECRET", "test-value")
	t.Setenv("AGENT_URL_LLM", "http://agent-llm:15001")
	return New(shared.NewLogger())
}

func request(master *Master, withSecret bool, body string) *httptest.ResponseRecorder {
	mux := http.NewServeMux()
	master.Register(mux)
	req := httptest.NewRequest("POST", "/deploy", strings.NewReader(body))
	if withSecret {
		req.Header.Set(shared.SecretHeader, "test-value")
	}
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, req)
	return recorder
}

func TestDeployRejectsWithoutSecret(t *testing.T) {
	if code := request(newTestMaster(t), false, `{}`).Code; code != 401 {
		t.Fatalf("무인증인데 %d", code)
	}
}

func TestDeployRejectsUnknownService(t *testing.T) {
	recorder := request(newTestMaster(t), true, `{"service":"evil","image_tag":"abc"}`)
	if recorder.Code != 422 || !strings.Contains(recorder.Body.String(), "unknown_service") {
		t.Fatalf("allowlist 밖 서비스가 통과: %d %s", recorder.Code, recorder.Body.String())
	}
}

func TestDeployRejectsMissingTag(t *testing.T) {
	if code := request(newTestMaster(t), true, `{"service":"llm"}`).Code; code != 422 {
		t.Fatalf("tag 없는 요청이 통과: %d", code)
	}
}

func TestDeployAcceptsKnownService(t *testing.T) {
	recorder := request(newTestMaster(t), true, `{"service":"llm","image_tag":"sha-abc"}`)
	if recorder.Code != 202 || !strings.Contains(recorder.Body.String(), `"success":true`) {
		t.Fatalf("정상 요청 거절: %d %s", recorder.Code, recorder.Body.String())
	}
}
