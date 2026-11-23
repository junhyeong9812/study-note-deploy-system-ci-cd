package agent

// 기존 경로 특성 테스트 — git 조회 경로(/agent/git/...)를 더한 뒤에도 /agent/ci · /agent/cd 가 같은 상태·같은 봉투로 답하는지 고정한다.
// 기준: requirement-spec.md §12.3 "기존 경로 무변경". 지금(경로 추가 전) 동작을 그대로 적었다.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/junhyeong9812/study-note-deploy-system-ci-cd/internal/shared"
)

func stageDataKeys(t *testing.T, recorder *httptest.ResponseRecorder) []string {
	t.Helper()
	var envelope struct {
		Success bool                       `json:"success"`
		Data    map[string]json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil || !envelope.Success {
		t.Fatalf("성공 봉투가 아니다: %v %s", err, recorder.Body.String())
	}
	keys := make([]string, 0, len(envelope.Data))
	for key := range envelope.Data {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func TestExistingStageRoutesAnswerSameAfterGitRoutes(t *testing.T) {
	for _, path := range []string{"/agent/ci", "/agent/cd"} {
		agent := newPipelineAgent(t, &recordedCommands{})
		if recorder := post(agent, path); recorder.Code != 200 || strings.Join(stageDataKeys(t, recorder), ",") != "output,took_ms" {
			t.Fatalf("%s 성공 답이 바뀌었다: %d %s", path, recorder.Code, recorder.Body.String())
		}

		agent.busy.Lock()
		if recorder := post(agent, path); recorder.Code != 409 || recorder.Body.String() != `{"error":{"code":"agent_busy"},"success":false}`+"\n" {
			t.Fatalf("%s 작업 중 답이 바뀌었다: %d %q", path, recorder.Code, recorder.Body.String())
		}
		agent.busy.Unlock()

		mux := http.NewServeMux()
		agent.Register(mux)
		unauthorized := httptest.NewRecorder()
		mux.ServeHTTP(unauthorized, httptest.NewRequest("POST", path, strings.NewReader(`{}`)))
		if unauthorized.Code != 401 || unauthorized.Body.String() != `{"error":{"code":"unauthorized"},"success":false}`+"\n" {
			t.Fatalf("%s 무인증 답이 바뀌었다: %d %q", path, unauthorized.Code, unauthorized.Body.String())
		}
		getRecorder := httptest.NewRecorder()
		getRequest := httptest.NewRequest("GET", path, nil)
		getRequest.Header.Set(shared.SecretHeader, "test-value")
		mux.ServeHTTP(getRecorder, getRequest)
		if getRecorder.Code != http.StatusMethodNotAllowed {
			t.Fatalf("GET %s 는 405 여야 한다: %d", path, getRecorder.Code)
		}
	}
}
