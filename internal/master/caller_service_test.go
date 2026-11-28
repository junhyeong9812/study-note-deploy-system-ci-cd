package master

// 마스터가 우리 서비스를 부르는 호출에 자기 규약 이름을 X-Caller-Service: ci-master 로 싣는다(⑬ T4b · 통신 규약 2.1 · 2.2 · 7.1 의 T4 줄).
//   - 우리 서비스로 가는 호출 = 에이전트의 배포 단계(/agent/ci · /agent/cd — 작업 중 409 뒤의 다시 부르기 포함) · 에이전트의 git 조회 · 알림 서버의 사건.
//   - 머리글은 정확히 하나 · 값은 소문자 ci-master(코드의 상수). 마스터로 떴다는 것이 값을 정한다 — SERVER_NAME · MODE · HOSTNAME 같은 설정에서 읽지 않는다.
//   - 들어온 요청의 X-Caller-Service(admin · front · 이상한 값)를 옮겨 싣지 않는다.
//   - 그 밖의 머리글(비밀값 · 요청 ID · 실패 출력 청함 · Content-Type) · 본문 · 다시 부르는 수는 그대로.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/junhyeong9812/study-note-deploy-system-ci-cd/internal/shared"
)

const callerServiceHeader = "X-Caller-Service"

var callerTrackingIDPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

// callerReceived — 가짜가 받은 요청 하나(경로 · 머리글 · 본문).
type callerReceived struct {
	path    string
	headers http.Header
	body    string
}

// callerRecordingAgent — /agent/ci · /agent/cd · /agent/git/* 를 받아 머리글을 그대로 쌓는 가짜 에이전트. 처음 busyTimes 번의 배포 단계는 409 agent_busy.
type callerRecordingAgent struct {
	mutex     sync.Mutex
	busyTimes int
	received  []callerReceived
}

func newCallerRecordingAgent(t *testing.T, busyTimes int) *callerRecordingAgent {
	t.Helper()
	agent := &callerRecordingAgent{busyTimes: busyTimes}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		agent.mutex.Lock()
		agent.received = append(agent.received, callerReceived{path: request.URL.Path, headers: request.Header.Clone(), body: string(body)})
		busy := strings.HasPrefix(request.URL.Path, "/agent/c") && agent.busyTimes > 0
		if busy {
			agent.busyTimes--
		}
		agent.mutex.Unlock()
		switch {
		case busy:
			shared.WriteFail(writer, http.StatusConflict, "agent_busy", "")
		case request.URL.Path == "/agent/git/commits":
			writer.Header().Set("Content-Type", "application/json; charset=utf-8")
			io.WriteString(writer, `{"success":true,"data":{"commits":[],"truncated":false,"range_complete":true,"relation":"ancestor"}}`)
		case request.URL.Path == "/agent/git/commit":
			writer.Header().Set("Content-Type", "application/json; charset=utf-8")
			io.WriteString(writer, `{"success":true,"data":{"sha":"`+relayToSha+`","files":[],"files_truncated":false}}`)
		default:
			shared.WriteOK(writer, map[string]any{"took_ms": 1})
		}
	}))
	t.Cleanup(server.Close)
	t.Setenv("AGENT_URL_LLM", server.URL)
	return agent
}

func (agent *callerRecordingAgent) calls() []callerReceived {
	agent.mutex.Lock()
	defer agent.mutex.Unlock()
	return append([]callerReceived(nil), agent.received...)
}

// callerRecordingAlerts — 알림 서버 대역. 받은 사건의 종류와 머리글을 쌓는다.
type callerRecordingAlerts struct {
	mutex    sync.Mutex
	received []callerReceived
}

func newCallerRecordingAlerts(t *testing.T) *callerRecordingAlerts {
	t.Helper()
	alerts := &callerRecordingAlerts{}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		alerts.mutex.Lock()
		alerts.received = append(alerts.received, callerReceived{path: request.URL.Path, headers: request.Header.Clone(), body: string(body)})
		alerts.mutex.Unlock()
		writer.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(server.Close)
	t.Setenv("ALERT_EVENTS_URL", server.URL)
	t.Setenv("ALERT_SECRET", "test-value")
	return alerts
}

// waitKinds — 그 종류의 사건이 atLeast 개 올 때까지 기다린다(보내기는 따로 돈다).
func (alerts *callerRecordingAlerts) waitKinds(t *testing.T, kind string, atLeast int) []callerReceived {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		alerts.mutex.Lock()
		matched := []callerReceived{}
		for _, received := range alerts.received {
			var event map[string]string
			json.Unmarshal([]byte(received.body), &event)
			if event["kind"] == kind {
				matched = append(matched, received)
			}
		}
		alerts.mutex.Unlock()
		if len(matched) >= atLeast || time.Now().After(deadline) {
			return matched
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// misleadingSettings — 값을 설정에서 읽으면 드러나게, 이름 비슷한 설정을 모두 다른 값으로 둔다.
func misleadingSettings(t *testing.T) {
	t.Setenv("SERVER_NAME", "front")
	t.Setenv("MODE", "agent")
	t.Setenv("HOSTNAME", "ci-agent")
	t.Setenv("CALLER_SERVICE", "admin")
	t.Setenv("SERVICE_NAME", "backend")
}

func requireCallerMaster(t *testing.T, where string, headers http.Header) {
	t.Helper()
	if values := headers.Values(callerServiceHeader); len(values) != 1 || values[0] != "ci-master" {
		t.Fatalf("%s: X-Caller-Service 는 정확히 하나 ci-master 여야 한다: %q", where, values)
	}
}

// 배포의 진행: /agent/ci 를 작업 중(409)으로 두 번 다시 부른 뒤 /agent/cd 까지 — 모든 호출에 ci-master 하나. 들어온 접수의 머리글은 옮기지 않는다.
func TestDeployStageCallsCarryMasterCallerOnEveryAttempt(t *testing.T) {
	for _, incoming := range [][]string{{"admin"}, {"front"}, {"Weird Value"}, {"ci-agent"}, {"admin", "front"}, nil} {
		misleadingSettings(t)
		agent := newCallerRecordingAgent(t, 2)
		master := newTestMaster(t)
		mux := http.NewServeMux()
		master.Register(mux)
		request := httptest.NewRequest("POST", "/deploy", strings.NewReader(`{"service":"llm","commit_sha":"`+deploySha+`","request_id":"gh-caller-1"}`))
		request.RemoteAddr = "10.0.0.1:40000"
		request.Header.Set(shared.SecretHeader, "test-value")
		for _, value := range incoming {
			request.Header.Add(callerServiceHeader, value)
		}
		recorder := httptest.NewRecorder()
		shared.TrackRequests(master.logger, mux).ServeHTTP(recorder, request)
		if recorder.Code != 202 {
			t.Fatalf("%q: 접수 %d %s", incoming, recorder.Code, recorder.Body.String())
		}
		if deployment := waitSettled(t, master, "gh-caller-1"); deployment.State != StateSucceeded {
			t.Fatalf("%q: 배포가 성공해야 한다: %+v", incoming, deployment)
		}
		calls := agent.calls()
		wantPaths := []string{"/agent/ci", "/agent/ci", "/agent/ci", "/agent/cd"}
		if len(calls) != len(wantPaths) {
			t.Fatalf("%q: 다시 부르는 수가 바뀌었다 — 호출 %d번 (기대 %d)", incoming, len(calls), len(wantPaths))
		}
		for index, received := range calls {
			where := incoming2label(incoming) + " " + itoa(index) + "번째 " + received.path
			if received.path != wantPaths[index] {
				t.Fatalf("%s: 경로가 %s 여야 한다", where, wantPaths[index])
			}
			requireCallerMaster(t, where, received.headers)
			if received.headers.Get(shared.SecretHeader) != "test-value" || received.headers.Get(shared.FailureOutputHeader) != shared.FailureOutputVersion ||
				received.headers.Get("Content-Type") != "application/json" || !callerTrackingIDPattern.MatchString(received.headers.Get(shared.RequestIDHeader)) {
				t.Fatalf("%s: 그 밖의 머리글이 바뀌었다: %v", where, received.headers)
			}
			var body map[string]any
			if json.Unmarshal([]byte(received.body), &body) != nil || len(body) != 3 || body["service"] != "llm" || body["commit_sha"] != deploySha || body["request_id"] != "gh-caller-1" {
				t.Fatalf("%s: 본문이 바뀌었다: %s", where, received.body)
			}
		}
	}
}

func incoming2label(incoming []string) string {
	if incoming == nil {
		return "(들어온 머리글 없음)"
	}
	return "(들어온 " + strings.Join(incoming, "+") + ")"
}

// git 조회의 중계: 두 경로 · 같은 조회의 되풀이 모두 ci-master 하나. 들어온 X-Caller-Service 는 옮기지 않는다.
func TestGitRelayCallsCarryMasterCaller(t *testing.T) {
	misleadingSettings(t)
	agent := newCallerRecordingAgent(t, 0)
	master := newTestMaster(t)
	mux := http.NewServeMux()
	master.Register(mux)
	handler := shared.TrackRequests(master.logger, mux)
	targets := []string{
		relayCommitsTarget("llm", relayFromSha, relayToSha), relayCommitsTarget("llm", relayFromSha, relayToSha),
		relayCommitTarget("llm", relayToSha), relayCommitTarget("llm", relayToSha),
	}
	for index, target := range targets {
		request := httptest.NewRequest("GET", target, nil)
		request.RemoteAddr = "10.0.0.1:40000"
		request.Header.Set(shared.SecretHeader, "test-value")
		request.Header.Set(callerServiceHeader, []string{"admin", "front", "ci-agent", "ADMIN"}[index])
		request.Header.Set(shared.RequestIDHeader, "trace-admin-0001")
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		if recorder.Code != 200 {
			t.Fatalf("%s: 중계 %d %s", target, recorder.Code, recorder.Body.String())
		}
	}
	calls := agent.calls()
	if len(calls) != len(targets) {
		t.Fatalf("에이전트 호출 %d번 (기대 %d)", len(calls), len(targets))
	}
	for index, received := range calls {
		where := itoa(index) + "번째 " + received.path
		requireCallerMaster(t, where, received.headers)
		if received.headers.Get(shared.SecretHeader) != "test-value" || received.headers.Get(shared.RequestIDHeader) != "trace-admin-0001" || received.body != "" {
			t.Fatalf("%s: 그 밖의 머리글 · 본문이 바뀌었다: %v %q", where, received.headers, received.body)
		}
	}
}

// 알림 서버로 보내는 사건(사람이 일으킨 접수 · 배포 실패)도 우리 서비스로 가는 호출이다 — ci-master 하나.
func TestMasterAlertsCarryMasterCaller(t *testing.T) {
	misleadingSettings(t)
	alerts := newCallerRecordingAlerts(t)
	t.Setenv("AGENT_URL_LLM", "http://127.0.0.1:1") // 닫힌 포트 — 접수 뒤 agent_unreachable 로 실패한다
	master := newTestMaster(t)
	mux := http.NewServeMux()
	master.Register(mux)
	request := httptest.NewRequest("POST", "/deploy", strings.NewReader(`{"service":"llm","commit_sha":"`+deploySha+`","request_id":"gh-caller-2","requested_by":"admin:owner"}`))
	request.Header.Set(shared.SecretHeader, "test-value")
	request.Header.Set(callerServiceHeader, "admin")
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, request)
	if recorder.Code != 202 {
		t.Fatalf("접수 %d %s", recorder.Code, recorder.Body.String())
	}
	waitSettled(t, master, "gh-caller-2")
	for _, kind := range []string{"deploy_requested", "deploy_failed"} {
		received := alerts.waitKinds(t, kind, 1)
		if len(received) != 1 {
			t.Fatalf("%s 사건이 하나 와야 한다: %d", kind, len(received))
		}
		requireCallerMaster(t, kind, received[0].headers)
		if received[0].headers.Get("X-Alert-Secret") != "test-value" || received[0].headers.Get("Content-Type") != "application/json" ||
			!callerTrackingIDPattern.MatchString(received[0].headers.Get(shared.RequestIDHeader)) {
			t.Fatalf("%s: 그 밖의 머리글이 바뀌었다: %v", kind, received[0].headers)
		}
	}
	// 마스터가 직접 보내는 알림(boot 의 중단 알림과 같은 길)도 같다
	master.alerts.Send("boot", "deploy_failed", "warning", "deploy-interrupted", "t", "d")
	if received := alerts.waitKinds(t, "deploy_failed", 2); len(received) != 2 {
		t.Fatalf("직접 보낸 사건이 오지 않았다: %d", len(received))
	} else {
		requireCallerMaster(t, "직접 보낸 사건", received[1].headers)
	}
}
