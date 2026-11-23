package master

// 기존 경로 특성 테스트 — git 조회 경로(/deploys/git/...)를 더한 뒤에도 기존 경로가 같은 상태·같은 봉투로 답하는지 고정한다.
// 기준: requirement-spec.md §12.3 "기존 경로 무변경" · §12.7 "라우터". 지금(경로 추가 전) 동작을 그대로 적었다.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
)

func dataKeys(t *testing.T, recorder *httptest.ResponseRecorder) []string {
	t.Helper()
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("봉투가 아니다: %v %s", err, recorder.Body.String())
	}
	if string(envelope["success"]) != "true" {
		t.Fatalf("success:true 여야 한다: %s", recorder.Body.String())
	}
	var data map[string]json.RawMessage
	if err := json.Unmarshal(envelope["data"], &data); err != nil {
		t.Fatalf("data 가 객체가 아니다: %s", recorder.Body.String())
	}
	keys := make([]string, 0, len(data))
	for key := range data {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func TestExistingRoutesAnswerSameAfterGitRoutes(t *testing.T) {
	newFakeAgent(t)
	master := newTestMaster(t)
	if recorder := submit(master, "gh-keep-1"); recorder.Code != 202 || strings.Join(dataKeys(t, recorder), ",") != "request_id,service,state" {
		t.Fatalf("POST /deploy 의 답이 바뀌었다: %d %s", recorder.Code, recorder.Body.String())
	}
	waitFinished(t, master, "gh-keep-1")

	single := call(master, "GET", "/deploys/gh-keep-1", "")
	if single.Code != 200 || strings.Join(dataKeys(t, single), ",") != "accepted_at,cd_ms,ci_ms,commit_sha,finished_at,request_id,service,state" {
		t.Fatalf("GET /deploys/{requestID} 의 답이 바뀌었다: %d %s", single.Code, single.Body.String())
	}
	listing := call(master, "GET", "/deploys?service=llm&limit=5", "")
	if listing.Code != 200 || strings.Join(dataKeys(t, listing), ",") != "deployments" || !strings.Contains(listing.Body.String(), `"request_id":"gh-keep-1"`) {
		t.Fatalf("GET /deploys 의 답이 바뀌었다: %d %s", listing.Code, listing.Body.String())
	}
	status := call(master, "GET", "/status", "")
	if status.Code != 200 || strings.Join(dataKeys(t, status), ",") != "history,services" || !strings.Contains(status.Body.String(), `"services":["llm"]`) {
		t.Fatalf("GET /status 의 답이 바뀌었다: %d %s", status.Code, status.Body.String())
	}
	if missing := call(master, "GET", "/deploys/gh-none", ""); missing.Code != 404 || missing.Body.String() != `{"error":{"code":"not_found"},"success":false}`+"\n" {
		t.Fatalf("없는 건의 답이 바뀌었다: %d %q", missing.Code, missing.Body.String())
	}
	for _, path := range []string{"/deploys/gh-keep-1", "/deploys", "/status"} {
		recorder := httptest.NewRecorder()
		mux := http.NewServeMux()
		master.Register(mux)
		mux.ServeHTTP(recorder, httptest.NewRequest("GET", path, nil))
		if recorder.Code != 401 || recorder.Body.String() != `{"error":{"code":"unauthorized"},"success":false}`+"\n" {
			t.Fatalf("%s 무인증 답이 바뀌었다: %d %q", path, recorder.Code, recorder.Body.String())
		}
	}
}

// GET /deploys/git (한 조각)은 지금 {requestID}="git" 단건 조회다 — 새 경로가 이것을 가로채지 않는다.
func TestDeploysGitSingleSegmentIsStillDeploymentLookup(t *testing.T) {
	newFakeAgent(t)
	master := newTestMaster(t)
	if recorder := call(master, "GET", "/deploys/git", ""); recorder.Code != 404 || recorder.Body.String() != `{"error":{"code":"not_found"},"success":false}`+"\n" {
		t.Fatalf("GET /deploys/git 은 요청 ID 'git' 조회(없으면 404 not_found)여야 한다: %d %q", recorder.Code, recorder.Body.String())
	}
	if recorder := call(master, "POST", "/deploy", `{"service":"llm","commit_sha":"`+deploySha+`","request_id":"git"}`); recorder.Code != 202 {
		t.Fatalf("요청 ID 'git' 접수 실패: %d %s", recorder.Code, recorder.Body.String())
	}
	if deployment := waitFinished(t, master, "git"); deployment.RequestID != "git" || deployment.CommitSha != deploySha {
		t.Fatalf("GET /deploys/git 이 요청 ID 'git' 을 돌려주지 않았다: %+v", deployment)
	}
	if recorder := call(master, "POST", "/deploys/git", ""); recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /deploys/git 은 405 여야 한다: %d", recorder.Code)
	}
}
