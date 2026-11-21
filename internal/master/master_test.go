package master

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/junhyeong9812/study-note-deploy-system-ci-cd/internal/shared"
)

func newTestMaster(t *testing.T) *Master {
	t.Setenv("DEPLOY_SECRET", "test-value")
	if os.Getenv("AGENT_URL_LLM") == "" {
		t.Setenv("AGENT_URL_LLM", "http://127.0.0.1:1") // 닫힌 포트 — 접수만 보는 테스트용
	}
	t.Setenv("DEPLOY_DB_PATH", filepath.Join(t.TempDir(), "deploys.db"))
	master, err := New(shared.NewLogger())
	if err != nil {
		t.Fatal(err)
	}
	master.busyRetryInterval, master.storeRetryInterval = 5*time.Millisecond, 20*time.Millisecond
	return master
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
	recorder := request(newTestMaster(t), true, `{"service":"evil","commit_sha":"abc1234"}`)
	if recorder.Code != 422 || !strings.Contains(recorder.Body.String(), "unknown_service") {
		t.Fatalf("allowlist 밖 서비스가 통과: %d %s", recorder.Code, recorder.Body.String())
	}
}

func TestDeployRejectsMissingSha(t *testing.T) {
	if code := request(newTestMaster(t), true, `{"service":"llm"}`).Code; code != 422 {
		t.Fatalf("sha 없는 요청이 통과: %d", code)
	}
}

func TestDeployRejectsNonHexShaBeforeAccept(t *testing.T) {
	recorder := request(newTestMaster(t), true, `{"service":"llm","commit_sha":"abc; rm -rf /"}`)
	if recorder.Code != 422 || !strings.Contains(recorder.Body.String(), "invalid_sha") {
		t.Fatalf("비hex sha가 202로 접수됨: %d %s", recorder.Code, recorder.Body.String())
	}
}

func TestDeployAcceptsKnownService(t *testing.T) {
	recorder := request(newTestMaster(t), true, `{"service":"llm","commit_sha":"abc1234def"}`)
	if recorder.Code != 202 || !strings.Contains(recorder.Body.String(), `"success":true`) {
		t.Fatalf("정상 요청 거절: %d %s", recorder.Code, recorder.Body.String())
	}
}

func TestDeployAcceptsObserveServiceWhenConfigured(t *testing.T) {
	t.Setenv("AGENT_URL_OBSERVE", "http://127.0.0.1:1") // 닫힌 포트 — 접수만 본다
	master := newTestMaster(t)
	recorder := request(master, true, `{"service":"observe","commit_sha":"abc1234def"}`)
	if recorder.Code != 202 || !strings.Contains(recorder.Body.String(), `"success":true`) {
		t.Fatalf("배포 대상 observe 가 거절됨: %d %s", recorder.Code, recorder.Body.String())
	}
	recorder = request(master, true, `{"service":"observer","commit_sha":"abc1234def"}`)
	if recorder.Code != 422 || !strings.Contains(recorder.Body.String(), "unknown_service") {
		t.Fatalf("목록 밖 서비스가 통과: %d %s", recorder.Code, recorder.Body.String())
	}
}
