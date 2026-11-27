package agent

// 에이전트가 우리 서비스를 부르는 호출에 자기 규약 이름을 X-Caller-Service: ci-agent 로 싣는다(⑬ T4b · 통신 규약 2.1 · 2.2 · 7.1 의 T4 줄).
//   - 우리 서비스로 가는 호출 = 배포한 서비스의 헬스 확인(waitHealthy — 폴링의 되풀이 포함) · 자리 확인(probeSlot — servingSlot 의 다시 보기 포함) ·
//     정리 시작 알림(notifyDrain) · 알림 서버의 사건. 서비스의 nginx 를 거치든 컨테이너 포트를 바로 부르든 규약 2.1 의 서비스이면 싣는다.
//   - 머리글은 정확히 하나 · 값은 소문자 ci-agent(코드의 상수). 에이전트로 떴다는 것이 값을 정한다 — SERVER_NAME(ci-agent-<서버>) · MODE · HOSTNAME 에서 읽지 않는다.
//   - 들어온 요청의 X-Caller-Service(마스터가 보낸 ci-master 등)를 옮겨 싣지 않는다.
//   - 싣지 않는다: 바깥(git 명령 — GitHub · 도커 API 의 유닉스 소켓)과 규약 2.1 의 목록에 없는 Claude 실행 자리(서비스 claude)의 헬스 확인.
//   - 그 밖의 머리글(요청 ID · Content-Type) · 판정 · 다시 보는 수는 그대로.

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/junhyeong9812/study-note-deploy-system-ci-cd/internal/shared"
)

const callerServiceHeader = "X-Caller-Service"

// callerRecorder — 받은 요청의 머리글을 차례대로 쌓는 대역. answers 가 남아 있으면 그 차례의 상태 · 자리 표식으로 답한다(없으면 200 · blue).
type callerRecorder struct {
	mutex    sync.Mutex
	received []http.Header
	answers  []callerAnswer
	server   *httptest.Server
}

type callerAnswer struct {
	status int
	slot   string
}

func newCallerRecorder(t *testing.T, answers ...callerAnswer) *callerRecorder {
	t.Helper()
	recorder := &callerRecorder{answers: answers}
	recorder.server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		io.Copy(io.Discard, request.Body)
		recorder.mutex.Lock()
		recorder.received = append(recorder.received, request.Header.Clone())
		answer := callerAnswer{status: http.StatusOK, slot: slotBlue}
		if len(recorder.answers) > 0 {
			answer, recorder.answers = recorder.answers[0], recorder.answers[1:]
		}
		recorder.mutex.Unlock()
		if answer.slot != "" {
			writer.Header().Set(slotHeader, answer.slot)
		}
		writer.WriteHeader(answer.status)
	}))
	t.Cleanup(recorder.server.Close)
	return recorder
}

func (recorder *callerRecorder) headers() []http.Header {
	recorder.mutex.Lock()
	defer recorder.mutex.Unlock()
	return append([]http.Header(nil), recorder.received...)
}

func requireCallerAgent(t *testing.T, where string, headers http.Header) {
	t.Helper()
	if values := headers.Values(callerServiceHeader); len(values) != 1 || values[0] != "ci-agent" {
		t.Fatalf("%s: X-Caller-Service 는 정확히 하나 ci-agent 여야 한다: %q", where, values)
	}
}

// misleadingAgentSettings — 값을 설정에서 읽으면 드러나게, 이름 비슷한 설정을 모두 다른 값으로 둔다.
func misleadingAgentSettings(t *testing.T) {
	t.Setenv("SERVER_NAME", "ci-agent-home")
	t.Setenv("MODE", "master")
	t.Setenv("HOSTNAME", "ci-master")
	t.Setenv("CALLER_SERVICE", "front")
	t.Setenv("SERVICE_NAME", "backend")
}

// 헬스 확인(되풀이 포함) · 자리 확인 · 정리 알림 — 호출마다 ci-agent 하나, 요청 ID 는 그대로 새 32자.
func TestDeployProbesCarryAgentCaller(t *testing.T) {
	misleadingAgentSettings(t)
	// 헬스: 503 한 번 뒤 200 — 폴링의 두 번째 호출에도 실린다(사이에 3초를 쉰다)
	health := newCallerRecorder(t, callerAnswer{status: http.StatusServiceUnavailable}, callerAnswer{status: http.StatusOK})
	if err := waitHealthy(health.server.URL+"/health", 10*time.Second); err != nil {
		t.Fatalf("헬스: %v", err)
	}
	probes := newCallerRecorder(t)
	if slot, err := probeSlot(probes.server.URL + "/health"); err != nil || slot != slotBlue {
		t.Fatalf("자리 확인: %q %v", slot, err)
	}
	if err := notifyDrain(probes.server.URL + "/drain"); err != nil {
		t.Fatalf("정리 알림: %v", err)
	}
	received := append(health.headers(), probes.headers()...)
	if len(received) != 4 {
		t.Fatalf("호출 %d번 (기대 4 — 헬스 둘 · 자리 확인 · 정리 알림)", len(received))
	}
	for index, headers := range received {
		where := []string{"헬스 1번째", "헬스 2번째(되풀이)", "자리 확인", "정리 알림"}[index]
		requireCallerAgent(t, where, headers)
		if values := headers.Values(shared.RequestIDHeader); len(values) != 1 || len(values[0]) != 32 {
			t.Fatalf("%s: 요청 ID 가 바뀌었다: %q", where, values)
		}
	}
	if probes.headers()[1].Get("Content-Type") != "application/json" {
		t.Fatalf("정리 알림의 Content-Type 이 바뀌었다: %v", probes.headers()[1])
	}
}

// 자리 표식이 없으면 servingSlot 이 다시 본다(세 번까지) — 다시 보는 호출에도 실린다.
func TestServingSlotRetriesCarryAgentCaller(t *testing.T) {
	misleadingAgentSettings(t)
	agent := newPipelineAgent(t, &recordedCommands{})
	agent.probeSlot = probeSlot // 기본 이음매 — 실제 호출
	agent.probeInterval = time.Millisecond
	public := newCallerRecorder(t, callerAnswer{status: http.StatusOK}, callerAnswer{status: http.StatusBadGateway}, callerAnswer{status: http.StatusOK, slot: slotGreen})
	if slot, err := agent.servingSlot(public.server.URL + "/health"); err != nil || slot != slotGreen {
		t.Fatalf("세 번째에 green: %q %v", slot, err)
	}
	received := public.headers()
	if len(received) != 3 {
		t.Fatalf("다시 보는 수가 바뀌었다: %d (기대 3)", len(received))
	}
	for index, headers := range received {
		requireCallerAgent(t, "자리 확인 "+string(rune('1'+index))+"번째", headers)
	}
}

// newDefaultSeamAgent — 헬스 확인 이음매를 바꾸지 않은 에이전트(명령만 기록) — llm 과 claude 의 헬스를 각자의 대역으로 둔다.
func newDefaultSeamAgent(t *testing.T, llmHealthURL, claudeHealthURL string) (*Agent, *recordedCommands) {
	t.Helper()
	t.Setenv("DEPLOY_SECRET", "test-value")
	t.Setenv("DEPLOY_DIR_LLM", "/srv/llm")
	t.Setenv("DEPLOY_SVC_LLM", "wrapper")
	t.Setenv("DEPLOY_HEALTH_LLM", llmHealthURL)
	t.Setenv("DEPLOY_DIR_CLAUDE", "/srv/claude")
	t.Setenv("DEPLOY_SVC_CLAUDE", "claude-runner")
	t.Setenv("DEPLOY_HEALTH_CLAUDE", claudeHealthURL)
	recorded := &recordedCommands{runningImage: "wrapper:" + requestSha}
	agent := New(shared.NewLogger())
	agent.run = recorded.run
	return agent, recorded
}

func stageRequest(service, incomingCaller string) *http.Request {
	request := httptest.NewRequest("POST", "/agent/cd", strings.NewReader(`{"service":"`+service+`","commit_sha":"`+requestSha+`","request_id":"gh-caller-9"}`))
	request.RemoteAddr = "127.0.0.1:40000"
	request.Header.Set(shared.SecretHeader, "test-value")
	request.Header.Set(shared.RequestIDHeader, "trace-master-0001")
	if incomingCaller != "" {
		request.Header.Set(callerServiceHeader, incomingCaller)
	}
	return request
}

// 마스터가 보낸 배포(머리글 ci-master)를 받아 서비스의 헬스를 볼 때 — 에이전트는 자기 이름 ci-agent 를 싣는다.
// Claude 실행 자리(claude)는 규약 2.1 의 서비스가 아니고 앞에 nginx 가 없다 — 그 헬스에는 싣지 않는다(요청 ID 는 그대로 싣는다).
func TestDeliveryHealthCarriesAgentCallerExceptForClaudeRunner(t *testing.T) {
	misleadingAgentSettings(t)
	llmHealth, claudeHealth := newCallerRecorder(t), newCallerRecorder(t)
	agent, _ := newDefaultSeamAgent(t, llmHealth.server.URL+"/health", claudeHealth.server.URL+"/health")
	mux := http.NewServeMux()
	agent.Register(mux)
	handler := shared.TrackRequests(agent.logger, mux)
	for _, sent := range []struct{ service, incoming string }{{"llm", "ci-master"}, {"llm", "front"}, {"llm", ""}, {"claude", "ci-master"}} {
		recorder := httptest.NewRecorder()
		func() {
			defer failOnPanic(t)
			handler.ServeHTTP(recorder, stageRequest(sent.service, sent.incoming))
		}()
		if recorder.Code != 200 {
			t.Fatalf("%s (들어온 %q): CD 성공이어야 한다: %d %s", sent.service, sent.incoming, recorder.Code, recorder.Body.String())
		}
	}
	llmReceived := llmHealth.headers()
	if len(llmReceived) != 3 {
		t.Fatalf("llm 헬스 호출 %d번 (기대 3)", len(llmReceived))
	}
	for index, headers := range llmReceived {
		requireCallerAgent(t, "llm 헬스 "+string(rune('1'+index))+"번째", headers)
	}
	claudeReceived := claudeHealth.headers()
	if len(claudeReceived) != 1 {
		t.Fatalf("claude 헬스 호출 %d번 (기대 1)", len(claudeReceived))
	}
	if values := claudeReceived[0].Values(callerServiceHeader); len(values) != 0 {
		t.Fatalf("규약 밖의 Claude 실행 자리에 X-Caller-Service 를 실었다: %q", values)
	}
	if values := claudeReceived[0].Values(shared.RequestIDHeader); len(values) != 1 || len(values[0]) != 32 {
		t.Fatalf("Claude 실행 자리의 헬스에도 요청 ID 는 그대로 실린다: %q", values)
	}
}

// 에이전트의 알림(두 자리가 남았을 수 있는 상황 등)도 우리 서비스로 가는 호출이다 — ci-agent 하나.
func TestAgentAlertsCarryAgentCaller(t *testing.T) {
	misleadingAgentSettings(t)
	received := make(chan http.Header, 4)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		io.Copy(io.Discard, request.Body)
		received <- request.Header.Clone()
		writer.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()
	t.Setenv("ALERT_EVENTS_URL", server.URL)
	t.Setenv("ALERT_SECRET", "test-value")
	agent := newPipelineAgent(t, &recordedCommands{})
	for index := 0; index < 2; index++ {
		agent.alerts.Send("cd", "slots_left", "critical", "slots-left:llm", "t", "d")
		select {
		case headers := <-received:
			requireCallerAgent(t, "알림 "+string(rune('1'+index))+"번째", headers)
			if headers.Get("X-Alert-Secret") != "test-value" || len(headers.Get(shared.RequestIDHeader)) != 32 {
				t.Fatalf("알림의 그 밖의 머리글이 바뀌었다: %v", headers)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("사건이 도착하지 않았다")
		}
	}
}

// 바깥에는 싣지 않는다: git 명령(GitHub 로 가는 fetch)의 인자 · 환경, 그리고 실제 유닉스 소켓으로 잇는 도커 API.
func TestCallerServiceIsNotSentOutside(t *testing.T) {
	misleadingAgentSettings(t)
	recorded := &recordedCommands{runningImage: "llm-wrapper:" + runningSha}
	agent := newPipelineAgent(t, recorded)
	mux := http.NewServeMux()
	agent.Register(mux)
	request := httptest.NewRequest("POST", "/agent/ci", strings.NewReader(`{"service":"llm","commit_sha":"`+requestSha+`","request_id":"gh-1"}`))
	request.RemoteAddr = "127.0.0.1:40000"
	request.Header.Set(shared.SecretHeader, "test-value")
	request.Header.Set(callerServiceHeader, "ci-master")
	recorder := httptest.NewRecorder()
	shared.TrackRequests(agent.logger, mux).ServeHTTP(recorder, request)
	if recorder.Code != 200 || !recorded.has("fetch") {
		t.Fatalf("CI 의 전제: %d fetch=%v", recorder.Code, recorded.has("fetch"))
	}
	for index, line := range recorded.lines {
		lowered := strings.ToLower(line + " " + strings.Join(recorded.environments[index], " "))
		if strings.Contains(lowered, "caller") || strings.Contains(lowered, "ci-agent") || strings.Contains(lowered, "extraheader") {
			t.Fatalf("명령 %q 에 호출자 이름이 실렸다(환경 %v)", line, recorded.environments[index])
		}
	}

	socketPath := filepath.Join(t.TempDir(), "docker.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	dockerHeaders := make(chan http.Header, 1)
	dockerServer := &http.Server{Handler: http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		dockerHeaders <- request.Header.Clone()
		writer.Header().Set("Content-Type", "application/json")
		io.WriteString(writer, `[]`)
	})}
	go dockerServer.Serve(listener)
	defer dockerServer.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var listed []dockerListedContainer
	if err := newDockerAPI(socketPath).decode(ctx, http.MethodGet, "/containers/json", nil, &listed); err != nil {
		t.Fatalf("도커 대역: %v", err)
	}
	select {
	case headers := <-dockerHeaders:
		if values := headers.Values(callerServiceHeader); len(values) != 0 {
			t.Fatalf("도커 API 에 X-Caller-Service 를 실었다: %q", values)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("도커 대역에 요청이 오지 않았다")
	}
}
