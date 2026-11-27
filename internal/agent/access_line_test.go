package agent

// 에이전트가 받은 요청마다 쓰는 접속 줄(통신 규약 3.4 · 3.6 · ⑬ T4c)과, 그 줄을 읽는 쪽(이 패키지의 classifyTrafficLine — T5)의 맞물림.
//   - 쓴 줄은 읽는 쪽이 읽는다(unparsed 0 — loopback 이 든 15칸 그대로) · 갈래가 3.6 대로 갈린다:
//     GET /health 를 루프백에서(compose 의 헬스 확인 — 호출자 머리글 없음) · alert 가 사설 대역에서 → health,
//     같은 경로를 다른 호출자(front)가 → request, /health/ → request, POST /agent/ci · /agent/cd → stream.
//   - 에이전트의 모든 받는 길에 정확히 한 줄, service = ci-agent, 마스터가 실은 X-Caller-Service: ci-master 가 caller 로 적힌다.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/junhyeong9812/study-note-deploy-system-ci-cd/internal/shared"
)

// agentAccessTexts — 표준 출력에서 {"v": 로 시작하는 줄의 글.
func agentAccessTexts(captured string) []string {
	texts := []string{}
	for _, text := range strings.Split(captured, "\n") {
		if strings.HasPrefix(text, `{"v":`) {
			texts = append(texts, text)
		}
	}
	return texts
}

// servedAgentLine — main 의 에이전트처럼(Register + GET /health) 감싸 한 번 부르고 그 요청의 접속 줄을 돌려준다.
func servedAgentLine(t *testing.T, agent *Agent, method, target, body, remoteAddress string, headers map[string]string) (*httptest.ResponseRecorder, string) {
	t.Helper()
	mux := http.NewServeMux()
	agent.Register(mux)
	mux.HandleFunc("GET /health", AgentHealthHandler("agent", nil))
	request := httptest.NewRequest(method, target, strings.NewReader(body))
	request.RemoteAddr = remoteAddress
	request.Header.Set(shared.SecretHeader, "test-value")
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	recorder := httptest.NewRecorder()
	captured := captureStandardOutput(t, func() {
		defer failOnPanic(t)
		shared.TrackRequests(agent.logger, shared.WriteAccessLines("ci-agent", mux)).ServeHTTP(recorder, request)
	})
	texts := agentAccessTexts(captured)
	if len(texts) != 1 {
		t.Fatalf("%s %s: 접속 줄은 정확히 하나: %d\n%s", method, target, len(texts), captured)
	}
	return recorder, texts[0]
}

func TestWrittenAccessLinesAreReadAndClassifiedByTheTrafficReader(t *testing.T) {
	agent := newPipelineAgent(t, &recordedCommands{})
	agent.gitProgram = "/nonexistent/git-for-access-test"
	stageBody := `{"service":"llm","commit_sha":"` + requestSha + `","request_id":"gh-1"}`
	longPath := "/" + strings.Repeat("p", 1100)
	cases := []struct {
		label, method, target, body, remoteAddress, caller string
		wantCaller, wantClass                              string
		status                                             int64
		truncated                                          bool
	}{
		{"compose 의 헬스 확인(루프백 · 머리글 없음)", "GET", "/health", "", "127.0.0.1:40000", "", "outside", "health", 200, false},
		{"알림 서버의 감시", "GET", "/health", "", "10.0.0.1:40000", "alert", "alert", "health", 200, false},
		{"알림 서버의 감시(HEAD)", "HEAD", "/health", "", "10.0.0.1:40000", "alert", "alert", "health", 200, false},
		{"다른 호출자의 /health", "GET", "/health", "", "10.0.0.1:40000", "front", "front", "request", 200, false},
		{"바깥에서 온 /health", "GET", "/health", "", "203.0.113.7:40000", "alert", "outside", "request", 200, false},
		{"끝에 / 가 붙은 경로", "GET", "/health/", "", "127.0.0.1:40000", "", "outside", "request", 404, false},
		{"배포 단계(마스터가 부름)", "POST", "/agent/ci", stageBody, "127.0.0.1:40000", "ci-master", "ci-master", "stream", 200, false},
		{"배포 단계 cd", "POST", "/agent/cd", stageBody, "10.0.0.1:40000", "ci-master", "ci-master", "stream", 200, false},
		{"git 조회", "GET", "/agent/git/commit?service=llm&sha=" + requestSha, "", "10.0.0.1:40000", "ci-master", "ci-master", "request", 500, false},
		{"긴 경로", "GET", longPath + "?q=x", "", "10.0.0.1:40000", "admin", "admin", "request", 404, true},
	}
	for _, testCase := range cases {
		headers := map[string]string{}
		if testCase.caller != "" {
			headers["X-Caller-Service"] = testCase.caller
		}
		recorder, text := servedAgentLine(t, agent, testCase.method, testCase.target, testCase.body, testCase.remoteAddress, headers)
		if int64(recorder.Code) != testCase.status {
			t.Fatalf("%s: 상태 %d (기대 %d)", testCase.label, recorder.Code, testCase.status)
		}
		kind, record := classifyTrafficLine([]byte(text))
		if kind != trafficUnitLine || record == nil {
			t.Fatalf("%s: 읽는 쪽이 쓴 줄을 읽지 못했다(kind %d): %s", testCase.label, kind, text)
		}
		if record.service != "ci-agent" || record.caller != testCase.wantCaller || record.method != testCase.method || record.status != testCase.status ||
			record.requestID != recorder.Result().Header.Get(shared.RequestIDHeader) || record.pathTruncated != testCase.truncated {
			t.Fatalf("%s: 읽은 값이 다르다: %+v\n%s", testCase.label, *record, text)
		}
		if record.class != testCase.wantClass {
			t.Fatalf("%s: 갈래가 %q (기대 %q — 규약 3.6): %s", testCase.label, record.class, testCase.wantClass, text)
		}
		if record.upstreamMilliseconds != nil || record.upstreamStatus != nil || record.slot != nil {
			t.Fatalf("%s: upstream_* · slot 은 없음이어야 한다: %+v", testCase.label, *record)
		}
	}
}

// 에이전트의 모든 받는 길에 한 줄 — 작업 중 409 · 인증 실패 401 · 틀린 입력 422 포함.
func TestAgentWritesOneAccessLineOnEveryRoute(t *testing.T) {
	agent := newPipelineAgent(t, &recordedCommands{})
	stageBody := `{"service":"llm","commit_sha":"` + requestSha + `","request_id":"gh-1"}`
	for _, sent := range []struct {
		method, target, body string
		status               int
		busy, unauthorized   bool
	}{
		{"POST", "/agent/ci", stageBody, 200, false, false},
		{"POST", "/agent/ci", stageBody, 409, true, false},
		{"POST", "/agent/cd", stageBody, 401, false, true},
		{"POST", "/agent/cd", `not json`, 422, false, false},
		{"GET", "/agent/git/commits?service=evil", "", 422, false, false},
		{"GET", "/agent/ci", "", 405, false, false},
		{"GET", "/no-such-route", "", 404, false, false},
		{"GET", "/health", "", 200, false, false},
	} {
		if sent.busy {
			agent.busy.Lock()
		}
		headers := map[string]string{"X-Caller-Service": "ci-master", "X-Request-Id": "trace-master-0001"}
		if sent.unauthorized {
			headers[shared.SecretHeader] = "wrong-value"
		}
		recorder, text := servedAgentLine(t, agent, sent.method, sent.target, sent.body, "127.0.0.1:40000", headers)
		if sent.busy {
			agent.busy.Unlock()
		}
		kind, record := classifyTrafficLine([]byte(text))
		if recorder.Code != sent.status || kind != trafficUnitLine || record.status != int64(sent.status) || record.caller != "ci-master" ||
			record.requestID != "trace-master-0001" || record.service != "ci-agent" {
			t.Fatalf("%s %s: 응답 %d · 줄 %s", sent.method, sent.target, recorder.Code, text)
		}
		if strings.Contains(text, "test-value") || strings.Contains(text, "wrong-value") {
			t.Fatalf("%s %s: 비밀값이 줄에 실렸다: %s", sent.method, sent.target, text)
		}
	}
}
