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
	mutex      sync.Mutex
	calls      []string
	failPath   string
	failCode   string
	failStatus int                      // 0이면 500
	busyTimes  int                      // 처음 이만큼은 409 agent_busy
	rawBody    string                   // 비어 있지 않으면 200에 이 본문을 그대로 돌려준다
	holds      map[string]chan struct{} // 경로별 — 닫힐 때까지 그 단계에 머문다
	server     *httptest.Server
}

func newFakeAgent(t *testing.T) *fakeAgent {
	agent := &fakeAgent{}
	agent.server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		agent.mutex.Lock()
		agent.calls = append(agent.calls, request.URL.Path)
		failPath, failCode, failStatus, rawBody := agent.failPath, agent.failCode, agent.failStatus, agent.rawBody
		hold := agent.holds[request.URL.Path]
		busy := agent.busyTimes > 0
		if busy {
			agent.busyTimes--
		}
		agent.mutex.Unlock()
		if busy {
			shared.WriteFail(writer, http.StatusConflict, "agent_busy", "")
			return
		}
		if hold != nil {
			<-hold
		}
		if rawBody != "" {
			writer.Write([]byte(rawBody))
			return
		}
		if request.URL.Path == failPath {
			if failStatus == 0 {
				failStatus = http.StatusInternalServerError
			}
			shared.WriteFail(writer, failStatus, failCode, "FAILED test_queue.py::test_order")
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
	hold := make(chan struct{})
	agent.holds = map[string]chan struct{}{"/agent/ci": hold}
	master := newTestMaster(t)
	submit(master, "gh-6-1")
	if recorder := submit(master, "gh-6-1"); recorder.Code != 202 || !strings.Contains(recorder.Body.String(), `"duplicate":true`) {
		t.Fatalf("진행 중 재접수는 409가 아니라 기존 건이어야 한다: %d %s", recorder.Code, recorder.Body.String())
	}
	if recorder := submit(master, "gh-6-2"); recorder.Code != 409 || !strings.Contains(recorder.Body.String(), "deploy_in_progress") {
		t.Fatalf("다른 요청은 진행 중 409여야 한다: %d %s", recorder.Code, recorder.Body.String())
	}
	close(hold)
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
	mutex       sync.Mutex
	failInsert  bool
	failOnState string
}

func (store *failingStore) setFailOnState(state string) {
	store.mutex.Lock()
	defer store.mutex.Unlock()
	store.failOnState = state
}

func (store *failingStore) Insert(deployment Deployment) error {
	if store.failInsert {
		return errors.New("disk full")
	}
	return store.deploymentStore.Insert(deployment)
}

func (store *failingStore) Update(deployment Deployment) error {
	store.mutex.Lock()
	failOnState := store.failOnState
	store.mutex.Unlock()
	if deployment.State == failOnState {
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

func waitState(t *testing.T, master *Master, requestID, state string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if lookup(t, master, requestID).State == state {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("%s 상태가 되지 않았다: %+v", state, lookup(t, master, requestID))
}

func TestStatePassesThroughEveryStage(t *testing.T) {
	agent := newFakeAgent(t)
	holdIntegration, holdDelivery := make(chan struct{}), make(chan struct{})
	agent.holds = map[string]chan struct{}{"/agent/ci": holdIntegration, "/agent/cd": holdDelivery}
	released := false
	t.Cleanup(func() { // 중간에 실패해도 붙잡힌 요청을 풀어 준다 — 안 그러면 가짜 서버 종료가 멈춘다
		if !released {
			agent.mutex.Lock()
			agent.holds = nil
			agent.mutex.Unlock()
			for _, hold := range []chan struct{}{holdIntegration, holdDelivery} {
				select {
				case <-hold:
				default:
					close(hold)
				}
			}
		}
	})
	master := newTestMaster(t)
	recorder := submit(master, "gh-11-1")
	if !strings.Contains(recorder.Body.String(), `"state":"accepted"`) {
		t.Fatalf("접수 응답의 상태는 accepted여야 한다: %s", recorder.Body.String())
	}
	waitState(t, master, "gh-11-1", StateIntegrating)
	time.Sleep(50 * time.Millisecond) // CI가 붙잡혀 있는 동안에는 CD로 넘어가지 않는다
	if calls := agent.called(); strings.Contains(calls, "/agent/cd") || lookup(t, master, "gh-11-1").State != StateIntegrating {
		t.Fatalf("CI가 끝나기 전에 CD로 넘어갔다: %s", calls)
	}
	close(holdIntegration)
	deadline := time.Now().Add(5 * time.Second) // CD 요청이 실제로 agent에 붙잡힌 뒤에 상태를 본다
	for agent.called() != "/agent/ci,/agent/cd" && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if state := lookup(t, master, "gh-11-1").State; agent.called() != "/agent/ci,/agent/cd" || state != StateDelivering {
		t.Fatalf("CD가 진행 중일 때 상태는 cd_running이어야 한다: %s calls=%s", state, agent.called())
	}
	close(holdDelivery)
	released = true
	if deployment := waitFinished(t, master, "gh-11-1"); deployment.State != StateSucceeded {
		t.Fatalf("성공으로 끝나야 한다: %+v", deployment)
	}
}

func TestSuccessThatCannotBeRecordedIsReportedAsFailure(t *testing.T) {
	newFakeAgent(t)
	master := newTestMaster(t)
	store := &failingStore{deploymentStore: master.store, failOnState: StateSucceeded}
	master.store = store
	submit(master, "gh-12-1")
	deployment := waitFinished(t, master, "gh-12-1")
	if deployment.State != StateFailed || deployment.FailedStage != StageStore || !strings.Contains(deployment.Detail, deploySha) {
		t.Fatalf("기록하지 못한 성공은 store_failed로 드러나야 한다: %+v", deployment)
	}
}

func TestFinishedResultIsServedFromMemoryUntilStoreRecovers(t *testing.T) {
	agent := newFakeAgent(t)
	agent.failPath, agent.failCode = "/agent/ci", "ci_test"
	master := newTestMaster(t)
	store := &failingStore{deploymentStore: master.store, failOnState: StateFailed}
	master.store = store
	submit(master, "gh-13-1")
	deployment := waitFinished(t, master, "gh-13-1") // DB에는 ci_running으로 남아 있지만 조회는 실제 결과를 준다
	if deployment.FailedStage != "ci_test" {
		t.Fatalf("메모리에 보관한 결과가 아니다: %+v", deployment)
	}
	if recorded, _, _ := store.deploymentStore.Find("gh-13-1"); recorded.State != StateIntegrating {
		t.Fatalf("테스트 전제 오류 — DB에 이미 기록됨: %+v", recorded)
	}
	// 복구 전에도 이력·재접수가 단건 조회와 같은 결과를 보여야 한다
	if body := call(master, "GET", "/deploys?service=llm", "").Body.String(); !strings.Contains(body, `"failed_stage":"ci_test"`) {
		t.Fatalf("이력이 DB의 진행 중 상태를 그대로 보여 준다: %s", body)
	}
	if body := submit(master, "gh-13-1").Body.String(); !strings.Contains(body, `"state":"failed"`) {
		t.Fatalf("재접수가 DB의 진행 중 상태를 돌려준다: %s", body)
	}
	store.setFailOnState("") // DB 복구
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if recorded, _, _ := store.deploymentStore.Find("gh-13-1"); recorded.State == StateFailed {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("DB가 복구됐는데 결과를 다시 기록하지 않았다")
}

func TestBusyAgentIsRetriedInsteadOfFailing(t *testing.T) {
	agent := newFakeAgent(t)
	agent.busyTimes = 2 // master 재시작 직후: agent가 앞선 작업을 아직 끝내지 못했다
	master := newTestMaster(t)
	submit(master, "gh-14-1")
	if deployment := waitFinished(t, master, "gh-14-1"); deployment.State != StateSucceeded {
		t.Fatalf("agent가 비면 진행돼야 한다: %+v", deployment)
	}
	if agent.called() != "/agent/ci,/agent/ci,/agent/ci,/agent/cd" {
		t.Fatalf("재시도 순서가 다르다: %s", agent.called())
	}
}

func TestAgentRefusalIsRecordedAsRejected(t *testing.T) {
	agent := newFakeAgent(t)
	agent.failPath, agent.failCode, agent.failStatus = "/agent/ci", "service_not_configured", 422
	master := newTestMaster(t)
	submit(master, "gh-15-1")
	deployment := waitFinished(t, master, "gh-15-1")
	if deployment.FailedStage != StageAgentRejected || !strings.Contains(deployment.Detail, "service_not_configured") {
		t.Fatalf("agent의 거절은 agent_rejected + 사유여야 한다: %+v", deployment)
	}
}

func TestOkStatusWithoutSuccessEnvelopeIsNotSuccess(t *testing.T) {
	agent := newFakeAgent(t)
	agent.rawBody = "<html>proxy</html>"
	master := newTestMaster(t)
	submit(master, "gh-16-1")
	deployment := waitFinished(t, master, "gh-16-1")
	if deployment.State != StateFailed || agent.called() != "/agent/ci" {
		t.Fatalf("봉투 없는 200을 성공으로 쳤다: %+v calls=%s", deployment, agent.called())
	}
}

func TestBusyRetriesAreSharedAcrossBothStages(t *testing.T) {
	agent := newFakeAgent(t)
	agent.busyTimes = busyRetryLimit + 5 // 끝내 비지 않는다
	master := newTestMaster(t)
	submit(master, "gh-17-1")
	deployment := waitFinished(t, master, "gh-17-1")
	if deployment.FailedStage != StageAgentRejected || !strings.Contains(deployment.Detail, "agent_busy") {
		t.Fatalf("대기 상한을 넘으면 agent_rejected(agent_busy)여야 한다: %+v", deployment)
	}
	if calls := strings.Count(agent.called(), "/agent/"); calls != busyRetryLimit+1 {
		t.Fatalf("재호출 횟수가 상한과 다르다: %d", calls)
	}
}
