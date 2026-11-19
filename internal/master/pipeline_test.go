package master

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/junhyeong9812/study-note-deploy-system-ci-cd/internal/shared"
)

const deploySha = "abc1234def"

// fakeAgent — /agent/ci·/agent/cd 호출을 기록하고, 정해 둔 단계를 실패시키거나 붙잡아 둔다.
type fakeAgent struct {
	mutex    sync.Mutex
	calls    []string
	failPath string
	failCode string
	hold     chan struct{} // nil이 아니면 CI가 닫힐 때까지 기다린다
	server   *httptest.Server
}

func newFakeAgent(t *testing.T) *fakeAgent {
	agent := &fakeAgent{}
	agent.server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		agent.mutex.Lock()
		agent.calls = append(agent.calls, request.URL.Path)
		failPath, failCode, hold := agent.failPath, agent.failCode, agent.hold
		agent.mutex.Unlock()
		if hold != nil && request.URL.Path == "/agent/ci" {
			<-hold
		}
		if request.URL.Path == failPath {
			shared.WriteFail(writer, http.StatusInternalServerError, failCode, "FAILED test_queue.py::test_order")
			return
		}
		shared.WriteOK(writer, map[string]any{"took_ms": 1})
	}))
	t.Cleanup(agent.server.Close)
	t.Setenv("AGENT_URL_LLM", agent.server.URL)
	return agent
}

func (agent *fakeAgent) called() string {
	agent.mutex.Lock()
	defer agent.mutex.Unlock()
	return strings.Join(agent.calls, ",")
}

func call(master *Master, method, path, body string) *httptest.ResponseRecorder {
	mux := http.NewServeMux()
	master.Register(mux)
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set(shared.SecretHeader, "test-value")
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, req)
	return recorder
}

func submit(master *Master, requestID string) *httptest.ResponseRecorder {
	return call(master, "POST", "/deploy", `{"service":"llm","commit_sha":"`+deploySha+`","request_id":"`+requestID+`"}`)
}

func lookup(t *testing.T, master *Master, requestID string) Deployment {
	t.Helper()
	recorder := call(master, "GET", "/deploys/"+requestID, "")
	if recorder.Code != 200 {
		t.Fatalf("조회 실패: %d %s", recorder.Code, recorder.Body.String())
	}
	var envelope struct {
		Data Deployment `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	return envelope.Data
}

func waitFinished(t *testing.T, master *Master, requestID string) Deployment {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if deployment := lookup(t, master, requestID); deployment.finished() {
			return deployment
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("배포가 끝나지 않았다: %+v", lookup(t, master, requestID))
	return Deployment{}
}

func TestPipelineSucceedsAfterIntegrationThenDelivery(t *testing.T) {
	agent := newFakeAgent(t)
	master := newTestMaster(t)
	if recorder := submit(master, "gh-1-1"); recorder.Code != 202 {
		t.Fatalf("접수 실패: %d %s", recorder.Code, recorder.Body.String())
	}
	deployment := waitFinished(t, master, "gh-1-1")
	if deployment.State != StateSucceeded || deployment.CommitSha != deploySha || deployment.FinishedAt == "" {
		t.Fatalf("성공으로 끝나야 한다: %+v", deployment)
	}
	if agent.called() != "/agent/ci,/agent/cd" {
		t.Fatalf("CI 다음 CD 순서가 아니다: %s", agent.called())
	}
}

func TestIntegrationFailureNeverCallsDelivery(t *testing.T) {
	agent := newFakeAgent(t)
	agent.failPath, agent.failCode = "/agent/ci", "ci_test"
	master := newTestMaster(t)
	submit(master, "gh-2-1")
	deployment := waitFinished(t, master, "gh-2-1")
	if deployment.State != StateFailed || deployment.FailedStage != "ci_test" {
		t.Fatalf("ci_test 실패여야 한다: %+v", deployment)
	}
	if !strings.Contains(deployment.Detail, "test_order") {
		t.Fatalf("실패 출력이 이력에 없다: %q", deployment.Detail)
	}
	if agent.called() != "/agent/ci" {
		t.Fatalf("CI가 실패했는데 CD가 호출됐다: %s", agent.called())
	}
}

func TestDeliveryFailureIsRecordedWithStage(t *testing.T) {
	agent := newFakeAgent(t)
	agent.failPath, agent.failCode = "/agent/cd", "cd_health"
	master := newTestMaster(t)
	submit(master, "gh-3-1")
	if deployment := waitFinished(t, master, "gh-3-1"); deployment.State != StateFailed || deployment.FailedStage != "cd_health" {
		t.Fatalf("cd_health 실패여야 한다: %+v", deployment)
	}
}

func TestUnreachableAgentFails(t *testing.T) {
	master := newTestMaster(t) // AGENT_URL_LLM = 닫힌 포트
	submit(master, "gh-4-1")
	if deployment := waitFinished(t, master, "gh-4-1"); deployment.FailedStage != StageAgentUnreachable {
		t.Fatalf("agent_unreachable이어야 한다: %+v", deployment)
	}
}

func TestResubmittingSameRequestDoesNotDeployTwice(t *testing.T) {
	agent := newFakeAgent(t)
	master := newTestMaster(t)
	submit(master, "gh-5-1")
	waitFinished(t, master, "gh-5-1")
	recorder := submit(master, "gh-5-1") // 202 응답이 유실돼 워크플로가 다시 보낸 경우
	if recorder.Code != 202 || !strings.Contains(recorder.Body.String(), `"duplicate":true`) ||
		!strings.Contains(recorder.Body.String(), `"state":"succeeded"`) {
		t.Fatalf("재접수는 기존 건의 상태를 돌려줘야 한다: %d %s", recorder.Code, recorder.Body.String())
	}
	time.Sleep(50 * time.Millisecond)
	if agent.called() != "/agent/ci,/agent/cd" {
		t.Fatalf("재접수가 배포를 한 번 더 돌렸다: %s", agent.called())
	}
}

func TestResubmittingWhileRunningReturnsSameDeployment(t *testing.T) {
	agent := newFakeAgent(t)
	agent.hold = make(chan struct{})
	master := newTestMaster(t)
	submit(master, "gh-6-1")
	if recorder := submit(master, "gh-6-1"); recorder.Code != 202 || !strings.Contains(recorder.Body.String(), `"duplicate":true`) {
		t.Fatalf("진행 중 재접수는 409가 아니라 기존 건이어야 한다: %d %s", recorder.Code, recorder.Body.String())
	}
	if recorder := submit(master, "gh-6-2"); recorder.Code != 409 || !strings.Contains(recorder.Body.String(), "deploy_in_progress") {
		t.Fatalf("다른 요청은 진행 중 409여야 한다: %d %s", recorder.Code, recorder.Body.String())
	}
	close(agent.hold)
	waitFinished(t, master, "gh-6-1")
	if recorder := call(master, "GET", "/deploys/gh-6-2", ""); recorder.Code != 404 {
		t.Fatalf("거절된 요청이 이력에 남았다: %d", recorder.Code)
	}
}

func TestSameRequestIDWithDifferentCommitIsRejected(t *testing.T) {
	newFakeAgent(t)
	master := newTestMaster(t)
	submit(master, "gh-7-1")
	waitFinished(t, master, "gh-7-1")
	recorder := call(master, "POST", "/deploy", `{"service":"llm","commit_sha":"fff9999","request_id":"gh-7-1"}`)
	if recorder.Code != 409 || !strings.Contains(recorder.Body.String(), "request_id_conflict") {
		t.Fatalf("같은 번호·다른 커밋은 거절해야 한다: %d %s", recorder.Code, recorder.Body.String())
	}
}

func TestRestartClosesUnfinishedDeploymentsAndKeepsHistory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "deploys.db")
	store, err := openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	store.Insert(Deployment{RequestID: "gh-8-1", Service: "llm", CommitSha: deploySha, State: StateSucceeded, AcceptedAt: "2026-11-19T10:00:00Z"})
	store.Insert(Deployment{RequestID: "gh-8-2", Service: "llm", CommitSha: deploySha, State: StateDelivering, AcceptedAt: "2026-11-19T11:00:00Z"})
	store.database.Close()

	t.Setenv("DEPLOY_SECRET", "test-value")
	t.Setenv("AGENT_URL_LLM", "http://127.0.0.1:1")
	t.Setenv("DEPLOY_DB_PATH", path)
	master, err := New(shared.NewLogger()) // 재시작
	if err != nil {
		t.Fatal(err)
	}
	if interrupted := lookup(t, master, "gh-8-2"); interrupted.State != StateFailed || interrupted.FailedStage != StageInterrupted {
		t.Fatalf("진행 중이던 건은 interrupted로 닫혀야 한다: %+v", interrupted)
	}
	if kept := lookup(t, master, "gh-8-1"); kept.State != StateSucceeded {
		t.Fatalf("끝난 건이 바뀌었다: %+v", kept)
	}
	recorder := call(master, "GET", "/deploys?service=llm&limit=1", "")
	if !strings.Contains(recorder.Body.String(), "gh-8-2") || strings.Contains(recorder.Body.String(), "gh-8-1") {
		t.Fatalf("이력은 최신순·limit 적용이어야 한다: %s", recorder.Body.String())
	}
}

// failingStore — 정해 둔 상태를 기록하려 할 때 실패한다.
type failingStore struct {
	deploymentStore
	failInsert  bool
	failOnState string
}

func (store *failingStore) Insert(deployment Deployment) error {
	if store.failInsert {
		return errors.New("disk full")
	}
	return store.deploymentStore.Insert(deployment)
}

func (store *failingStore) Update(deployment Deployment) error {
	if deployment.State == store.failOnState {
		return errors.New("disk full")
	}
	return store.deploymentStore.Update(deployment)
}

func TestStoreWriteFailureRejectsSubmission(t *testing.T) {
	agent := newFakeAgent(t)
	master := newTestMaster(t)
	master.store = &failingStore{deploymentStore: master.store, failInsert: true}
	if recorder := submit(master, "gh-9-1"); recorder.Code != 500 || !strings.Contains(recorder.Body.String(), StageStore) {
		t.Fatalf("기록 못 하는 배포를 접수했다: %d %s", recorder.Code, recorder.Body.String())
	}
	time.Sleep(50 * time.Millisecond)
	if agent.called() != "" {
		t.Fatalf("기록 없이 agent를 호출했다: %s", agent.called())
	}
	master.store = master.store.(*failingStore).deploymentStore
	if recorder := submit(master, "gh-9-2"); recorder.Code != 202 {
		t.Fatalf("실패한 접수가 진행 중 표시를 남겼다: %d %s", recorder.Code, recorder.Body.String())
	}
}

func TestStoreFailureBeforeDeliveryStopsPipeline(t *testing.T) {
	agent := newFakeAgent(t)
	master := newTestMaster(t)
	master.store = &failingStore{deploymentStore: master.store, failOnState: StateDelivering}
	submit(master, "gh-10-1")
	deployment := waitFinished(t, master, "gh-10-1")
	if deployment.State != StateFailed || deployment.FailedStage != StageStore {
		t.Fatalf("store_failed로 끝나야 한다: %+v", deployment)
	}
	if agent.called() != "/agent/ci" {
		t.Fatalf("CD 시작을 기록하지 못했는데 CD를 호출했다: %s", agent.called())
	}
}

func TestLookupRequiresSecretAndReportsMissing(t *testing.T) {
	master := newTestMaster(t)
	mux := http.NewServeMux()
	master.Register(mux)
	for _, path := range []string{"/deploys/gh-x", "/deploys"} {
		recorder := httptest.NewRecorder()
		mux.ServeHTTP(recorder, httptest.NewRequest("GET", path, nil))
		if recorder.Code != 401 {
			t.Fatalf("%s 무인증인데 %d", path, recorder.Code)
		}
	}
	if recorder := call(master, "GET", "/deploys/gh-none", ""); recorder.Code != 404 {
		t.Fatalf("없는 건은 404여야 한다: %d", recorder.Code)
	}
}
