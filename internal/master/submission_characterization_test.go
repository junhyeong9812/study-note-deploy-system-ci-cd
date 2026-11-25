package master

// 접수 · 이력 조회 특성 테스트 — "누가 일으켰나"(requested_by)를 더하기 전의 동작을 그대로 적었다.
// GitHub 워크플로(.github/workflows/deploy.yml 의 submit 단계)가 보내는 본문은 printf 로 만든 이 꼴 하나다:
//   {"service":"<서비스>","commit_sha":"<40자 커밋>","request_id":"gh-<run_id>-<run_attempt>"}
// 이 본문의 접수 · 재접수 · 거절의 응답이 글자까지 그대로인지 고정한다.
// 이력 조회는 기존 칸의 이름 · 순서 · 값이 그대로인지 고정한다(맨 끝에 requested_by 하나가 더해지는 것만 허용한다).

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/junhyeong9812/study-note-deploy-system-ci-cd/internal/shared"
)

// 워크플로가 보내는 커밋 — GITHUB_SHA 는 40자다.
const workflowSha = "0123456789abcdef0123456789abcdef01234567"

func workflowBody(service, requestID string) string {
	return `{"service":"` + service + `","commit_sha":"` + workflowSha + `","request_id":"` + requestID + `"}`
}

func requireExactResponse(t *testing.T, recorder *httptest.ResponseRecorder, wantStatus int, wantBody string) {
	t.Helper()
	if recorder.Code != wantStatus || recorder.Body.String() != wantBody+"\n" {
		t.Fatalf("응답이 바뀌었다:\n  받은 것 %d %q\n  기대     %d %q", recorder.Code, recorder.Body.String(), wantStatus, wantBody+"\n")
	}
}

func TestWorkflowBodyIsAcceptedWithSameResponse(t *testing.T) {
	newFakeAgent(t)
	master := newTestMaster(t)
	requireExactResponse(t, call(master, "POST", "/deploy", workflowBody("llm", "gh-900-1")), http.StatusAccepted,
		`{"data":{"request_id":"gh-900-1","service":"llm","state":"accepted"},"success":true}`)
	if deployment := waitFinished(t, master, "gh-900-1"); deployment.State != StateSucceeded || deployment.CommitSha != workflowSha {
		t.Fatalf("워크플로 본문의 배포가 성공으로 끝나지 않았다: %+v", deployment)
	}
}

func TestWorkflowResubmissionAnswersSameDeploymentWithSameResponse(t *testing.T) {
	agent := newFakeAgent(t)
	master := newTestMaster(t)
	call(master, "POST", "/deploy", workflowBody("llm", "gh-901-1"))
	waitSettled(t, master, "gh-901-1")
	requireExactResponse(t, call(master, "POST", "/deploy", workflowBody("llm", "gh-901-1")), http.StatusAccepted,
		`{"data":{"duplicate":true,"request_id":"gh-901-1","service":"llm","state":"succeeded"},"success":true}`)
	if agent.called() != "/agent/ci,/agent/cd" {
		t.Fatalf("재접수가 배포를 한 번 더 돌렸다: %s", agent.called())
	}
}

func TestWorkflowSubmissionWhileRunningAnswersSameConflicts(t *testing.T) {
	agent := newFakeAgent(t)
	hold := make(chan struct{})
	agent.holds = map[string]chan struct{}{"/agent/ci": hold}
	released := false
	t.Cleanup(func() {
		if !released {
			close(hold)
		}
	})
	master := newTestMaster(t)
	call(master, "POST", "/deploy", workflowBody("llm", "gh-902-1"))
	waitState(t, master, "gh-902-1", StateIntegrating) // CI 가 붙잡혀 있다
	requireExactResponse(t, call(master, "POST", "/deploy", workflowBody("llm", "gh-902-1")), http.StatusAccepted,
		`{"data":{"duplicate":true,"request_id":"gh-902-1","service":"llm","state":"ci_running"},"success":true}`)
	requireExactResponse(t, call(master, "POST", "/deploy", workflowBody("llm", "gh-902-2")), http.StatusConflict,
		`{"error":{"code":"deploy_in_progress","detail":"llm"},"success":false}`)
	close(hold)
	released = true
	waitFinished(t, master, "gh-902-1")
	requireExactResponse(t, call(master, "POST", "/deploy",
		`{"service":"llm","commit_sha":"fff9999","request_id":"gh-902-1"}`), http.StatusConflict,
		`{"error":{"code":"request_id_conflict"},"success":false}`)
}

func TestWorkflowSubmissionRejectionsAnswerSame(t *testing.T) {
	master := newTestMaster(t)
	requireExactResponse(t, request(master, false, workflowBody("llm", "gh-903-1")), http.StatusUnauthorized,
		`{"error":{"code":"unauthorized"},"success":false}`)
	requireExactResponse(t, call(master, "POST", "/deploy", workflowBody("evil", "gh-903-2")), http.StatusUnprocessableEntity,
		`{"error":{"code":"unknown_service","detail":"evil"},"success":false}`)
	requireExactResponse(t, call(master, "POST", "/deploy", `{"service":"llm","commit_sha":"ABC; rm","request_id":"gh-903-3"}`),
		http.StatusUnprocessableEntity, `{"error":{"code":"invalid_sha"},"success":false}`)
	requireExactResponse(t, call(master, "POST", "/deploy", `{"service":"llm","request_id":"gh-903-4"}`),
		http.StatusUnprocessableEntity, `{"error":{"code":"invalid_request","detail":"need service·commit_sha"},"success":false}`)
	requireExactResponse(t, call(master, "POST", "/deploy", `not json`),
		http.StatusUnprocessableEntity, `{"error":{"code":"invalid_request","detail":"need service·commit_sha"},"success":false}`)
	for _, requestID := range []string{"gh-903-1", "gh-903-2", "gh-903-3", "gh-903-4"} {
		if recorder := call(master, "GET", "/deploys/"+requestID, ""); recorder.Code != http.StatusNotFound {
			t.Fatalf("거절된 접수 %s 가 이력에 남았다: %d %s", requestID, recorder.Code, recorder.Body.String())
		}
	}
}

// legacyDeploymentsSchema — requested_by 를 더하기 전(main 5314245)의 표. 이 꼴의 파일이 운영 서버에 있다.
const legacyDeploymentsSchema = `CREATE TABLE IF NOT EXISTS deployments (
		request_id   TEXT PRIMARY KEY,
		service      TEXT NOT NULL,
		commit_sha   TEXT NOT NULL,
		state        TEXT NOT NULL,
		failed_stage TEXT NOT NULL DEFAULT '',
		detail       TEXT NOT NULL DEFAULT '',
		accepted_at  TEXT NOT NULL,
		finished_at  TEXT NOT NULL DEFAULT '',
		ci_ms        INTEGER NOT NULL DEFAULT 0,
		cd_ms        INTEGER NOT NULL DEFAULT 0
	);
	CREATE INDEX IF NOT EXISTS deployments_service_accepted ON deployments(service, accepted_at);`

// writeLegacyDatabase — 지금 코드를 거치지 않고 SQL 로 옛 꼴의 이력 파일을 만든다(끝난 건만 — 기동 때 값이 바뀌지 않게).
func writeLegacyDatabase(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "deploys.db")
	// main 5314245 의 openStore 와 같은 연결 설정 — 운영의 옛 파일은 WAL 모드다
	database, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if _, err := database.Exec(legacyDeploymentsSchema); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`INSERT INTO deployments (request_id, service, commit_sha, state, failed_stage, detail, accepted_at, finished_at, ci_ms, cd_ms) VALUES
		('gh-800-1', 'llm', '1111111aaaaaaa', 'succeeded', '', '', '2026-11-01T10:00:00Z', '2026-11-01T10:07:00Z', 240000, 180000),
		('gh-800-2', 'llm', '2222222bbbbbbb', 'failed', 'ci_test', 'FAILED test_queue.py::test_order', '2026-11-02T10:00:00Z', '2026-11-02T10:03:00Z', 170000, 0)`); err != nil {
		t.Fatal(err)
	}
	return path
}

func startMasterOn(t *testing.T, path string) (*Master, error) {
	t.Helper()
	t.Setenv("DEPLOY_SECRET", "test-value")
	t.Setenv("AGENT_URL_LLM", "http://127.0.0.1:1")
	t.Setenv("DEPLOY_DB_PATH", path)
	return New(shared.NewLogger())
}

type jsonField struct {
	name  string
	value string // 그 칸의 JSON 원문
}

// orderedFields — JSON 객체의 칸을 나온 차례대로 돌려준다(map 으로 풀면 차례를 잃는다).
func orderedFields(t *testing.T, raw json.RawMessage) []jsonField {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if opening, err := decoder.Token(); err != nil || opening != json.Delim('{') {
		t.Fatalf("객체가 아니다: %s", raw)
	}
	fields := []jsonField{}
	for decoder.More() {
		nameToken, err := decoder.Token()
		if err != nil {
			t.Fatalf("칸 이름을 읽지 못했다: %v %s", err, raw)
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			t.Fatalf("칸 값을 읽지 못했다: %v %s", err, raw)
		}
		fields = append(fields, jsonField{name: nameToken.(string), value: string(value)})
	}
	return fields
}

// withoutTrailingRequestedBy — 맨 끝의 requested_by 하나만 떼어 낸다(그 밖의 자리에 있으면 남겨 비교가 실패하게 한다).
func withoutTrailingRequestedBy(fields []jsonField) []jsonField {
	if count := len(fields); count > 0 && fields[count-1].name == "requested_by" {
		return fields[:count-1]
	}
	return fields
}

func renderFields(fields []jsonField) string {
	parts := make([]string, 0, len(fields))
	for _, field := range fields {
		parts = append(parts, field.name+"="+field.value)
	}
	return strings.Join(parts, " ")
}

func envelopeData(t *testing.T, recorder *httptest.ResponseRecorder) json.RawMessage {
	t.Helper()
	if recorder.Code != http.StatusOK {
		t.Fatalf("조회 실패: %d %s", recorder.Code, recorder.Body.String())
	}
	var envelope struct {
		Success bool            `json:"success"`
		Data    json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil || !envelope.Success {
		t.Fatalf("봉투가 아니다: %v %s", err, recorder.Body.String())
	}
	return envelope.Data
}

func listedDeployments(t *testing.T, recorder *httptest.ResponseRecorder, listName string) []json.RawMessage {
	t.Helper()
	var data map[string]json.RawMessage
	if err := json.Unmarshal(envelopeData(t, recorder), &data); err != nil {
		t.Fatalf("data 가 객체가 아니다: %s", recorder.Body.String())
	}
	var deployments []json.RawMessage
	if err := json.Unmarshal(data[listName], &deployments); err != nil {
		t.Fatalf("%s 가 배열이 아니다: %s", listName, recorder.Body.String())
	}
	return deployments
}

const (
	legacySucceededFields = `request_id="gh-800-1" service="llm" commit_sha="1111111aaaaaaa" state="succeeded" accepted_at="2026-11-01T10:00:00Z" finished_at="2026-11-01T10:07:00Z" ci_ms=240000 cd_ms=180000`
	legacyFailedFields    = `request_id="gh-800-2" service="llm" commit_sha="2222222bbbbbbb" state="failed" failed_stage="ci_test" detail="FAILED test_queue.py::test_order" accepted_at="2026-11-02T10:00:00Z" finished_at="2026-11-02T10:03:00Z" ci_ms=170000 cd_ms=0`
)

func TestLegacyHistoryRowsAnswerWithSameFieldsInSameOrder(t *testing.T) {
	master, err := startMasterOn(t, writeLegacyDatabase(t))
	if err != nil {
		t.Fatalf("옛 이력 파일로 뜨지 못했다: %v", err)
	}
	for _, listing := range []struct{ path, listName string }{{"/deploys?service=llm", "deployments"}, {"/deploys", "deployments"}, {"/status", "history"}} {
		deployments := listedDeployments(t, call(master, "GET", listing.path, ""), listing.listName)
		if len(deployments) != 2 {
			t.Fatalf("%s 의 건수가 다르다: %d", listing.path, len(deployments))
		}
		for index, want := range []string{legacyFailedFields, legacySucceededFields} { // 최신순
			if got := renderFields(withoutTrailingRequestedBy(orderedFields(t, deployments[index]))); got != want {
				t.Fatalf("%s 의 %d 번째 줄의 기존 칸이 바뀌었다:\n  받은 것 %s\n  기대     %s", listing.path, index, got, want)
			}
		}
	}
	for requestID, want := range map[string]string{"gh-800-1": legacySucceededFields, "gh-800-2": legacyFailedFields} {
		if got := renderFields(withoutTrailingRequestedBy(orderedFields(t, envelopeData(t, call(master, "GET", "/deploys/"+requestID, ""))))); got != want {
			t.Fatalf("GET /deploys/%s 의 기존 칸이 바뀌었다:\n  받은 것 %s\n  기대     %s", requestID, got, want)
		}
	}
}
