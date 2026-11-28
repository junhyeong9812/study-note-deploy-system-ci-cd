package agent

// 에이전트의 추적 ID(통신 규약 1.2 · 1.3 · 1.4 #10 · #11, 7.1 T2 ci-cd).
//   - 받는 경로 모두(/agent/ci · /agent/cd · /agent/git/*): 응답 머리글 X-Request-Id = 그 요청의 추적 ID(감싸개를 mux 전체에 씌운 모습으로 본다).
//   - /agent/ci · /agent/cd 는 본문 request_id(일의 ID)가 있는 경로다: 지금 일의 ID 로 쓰는 줄(agent ci start · ok · failed)은 그대로 두고,
//     두 ID 를 잇는 줄 한 줄을 추적 ID 로 남긴다:  <추적 ID>:ci-agent:agent <ci|cd> job=<일의 ID 또는 invalid> -> <상태>
//     일의 ID 는 에이전트가 쓴 값(본문 값, 없거나 꼴이 틀리면 에이전트가 만든 req-…), 그 값을 정하기 전에 끝난 요청은 invalid.
//   - git 조회의 줄은 판정한 추적 ID 로 남는다(지금은 머리글을 꼴만 보고 받는다 — git_read.go 의 AcceptOrIssue).
//   - 배포 중의 헬스 · 자리 확인 · 정리 알림은 들어온 요청 없이 시작되는 호출로 본다: 호출마다 새 32자.
//   - 바깥(git 명령 — GitHub)과 도커 API 에는 싣지 않는다.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/junhyeong9812/study-note-deploy-system-ci-cd/internal/shared"
)

var agentTrackingIDPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

// trackedAgentCall — main.go 처럼 mux 전체에 감싸개를 씌우고, 정한 연결 주소 · 추적 머리글로 한 번 부른다.
func trackedAgentCall(t *testing.T, agent *Agent, method, target, body, secret, remoteAddress string, incoming ...string) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	agent.Register(mux)
	handler := shared.TrackRequests(agent.logger, mux)
	request := httptest.NewRequest(method, target, strings.NewReader(body))
	request.RemoteAddr = remoteAddress
	if secret != "" {
		request.Header.Set(shared.SecretHeader, secret)
	}
	for _, value := range incoming {
		request.Header.Add(shared.RequestIDHeader, value)
	}
	recorder := httptest.NewRecorder()
	func() {
		defer failOnPanic(t)
		handler.ServeHTTP(recorder, request)
	}()
	return recorder
}

// agentLogLines — 로그에서 "<ID>:<서버>:<내용>" 부분만(시각 · 수준을 뗀다).
func agentLogLines(logged string) []string {
	lines := []string{}
	for _, line := range strings.Split(logged, "\n") {
		if fields := strings.SplitN(line, " ", 3); len(fields) == 3 {
			lines = append(lines, fields[2])
		}
	}
	return lines
}

func agentLinesMatching(logged string, pattern *regexp.Regexp) []string {
	matched := []string{}
	for _, line := range agentLogLines(logged) {
		if pattern.MatchString(line) {
			matched = append(matched, line)
		}
	}
	return matched
}

func stageBody(requestIDField string) string {
	return `{"service":"llm","commit_sha":"` + requestSha + `"` + requestIDField + `}`
}

func TestAgentRoutesReturnTrackingIDOnEveryResponse(t *testing.T) {
	agent := newPipelineAgent(t, &recordedCommands{})
	agent.gitProgram = "/nonexistent/git-for-tracking-test"
	cases := []struct {
		method, target, body, secret string
		status                       int
	}{
		{"POST", "/agent/ci", stageBody(`,"request_id":"gh-1"`), "test-value", 200},
		{"POST", "/agent/cd", stageBody(`,"request_id":"gh-1"`), "test-value", 200},
		{"POST", "/agent/ci", `{}`, "", 401},
		{"POST", "/agent/ci", `not json`, "test-value", 422},
		{"POST", "/agent/cd", `{"service":"other","commit_sha":"` + requestSha + `"}`, "test-value", 422},
		{"GET", "/agent/git/commits?service=llm&from=" + headSha + "&to=" + requestSha, "", "test-value", 500},
		{"GET", "/agent/git/commit?service=evil&sha=" + requestSha, "", "test-value", 422},
		{"GET", "/agent/git/commit?service=llm&sha=" + requestSha, "", "", 401},
		{"GET", "/agent/ci", "", "test-value", 405},
		{"GET", "/no-such-route", "", "test-value", 404},
	}
	for _, testCase := range cases {
		label := testCase.method + " " + testCase.target
		trusted := trackedAgentCall(t, agent, testCase.method, testCase.target, testCase.body, testCase.secret, "127.0.0.1:40000", "0123456789abcdef0123456789abcdef")
		if trusted.Code != testCase.status {
			t.Fatalf("%s: 상태 %d (기대 %d) %s", label, trusted.Code, testCase.status, trusted.Body.String())
		}
		if got := trusted.Result().Header.Values(shared.RequestIDHeader); len(got) != 1 || got[0] != "0123456789abcdef0123456789abcdef" {
			t.Errorf("%s (%d): 루프백 연결의 추적 ID 를 응답 머리글로 돌려줘야 한다: %q", label, trusted.Code, got)
		}
		outside := trackedAgentCall(t, agent, testCase.method, testCase.target, testCase.body, testCase.secret, "192.0.2.10:40000", "trace-forged-0801")
		if got := outside.Result().Header.Get(shared.RequestIDHeader); !agentTrackingIDPattern.MatchString(got) {
			t.Errorf("%s (%d): 바깥 연결의 값은 버리고 새 32자: %q", label, outside.Code, got)
		}
	}

	agent.busy.Lock()
	busy := trackedAgentCall(t, agent, "POST", "/agent/ci", stageBody(`,"request_id":"gh-1"`), "test-value", "10.0.0.1:40000", "trace-master-0802")
	agent.busy.Unlock()
	if busy.Code != 409 || busy.Result().Header.Get(shared.RequestIDHeader) != "trace-master-0802" {
		t.Fatalf("작업 중 409 에도 추적 ID: %d %q", busy.Code, busy.Result().Header.Get(shared.RequestIDHeader))
	}
}

// 감싸개를 씌워도 /agent/ci · /agent/cd 의 상태 · 본문은 그대로다.
func TestAgentStageResponsesUnchangedByTracking(t *testing.T) {
	for _, path := range []string{"/agent/ci", "/agent/cd"} {
		for _, sent := range []struct{ body, secret string }{
			{stageBody(`,"request_id":"gh-1"`), ""}, {`not json`, "test-value"}, {`{"service":"other"}`, "test-value"},
			{stageBody(`,"request_id":"gh-1"`), "test-value"},
		} {
			bareAgent := newPipelineAgent(t, &recordedCommands{failOn: "docker build --target test"})
			mux := http.NewServeMux()
			bareAgent.Register(mux)
			bareRequest := httptest.NewRequest("POST", path, strings.NewReader(sent.body))
			if sent.secret != "" {
				bareRequest.Header.Set(shared.SecretHeader, sent.secret)
			}
			bare := httptest.NewRecorder()
			mux.ServeHTTP(bare, bareRequest)
			tracked := trackedAgentCall(t, newPipelineAgent(t, &recordedCommands{failOn: "docker build --target test"}), "POST", path, sent.body, sent.secret, "127.0.0.1:40000", "trace-master-0803")
			if bare.Code != tracked.Code || stripTookMs(bare.Body.String()) != stripTookMs(tracked.Body.String()) {
				t.Fatalf("%s %q: 감싸개가 답을 바꿨다: %d %q → %d %q", path, sent.body, bare.Code, bare.Body.String(), tracked.Code, tracked.Body.String())
			}
		}
	}
}

func stripTookMs(body string) string {
	return regexp.MustCompile(`"took_ms":\d+`).ReplaceAllString(body, `"took_ms":0`)
}

func TestAgentStageLinksTrackingIDAndJobIDWhileJobLinesStay(t *testing.T) {
	t.Setenv("SERVER_NAME", "ci-agent")
	cases := []struct {
		label, path, body, secret, remoteAddress, trace string
		failOn                                          string
		busy                                            bool
		status                                          int
		job                                             string // 빈 값이면 에이전트가 만든 req-… 를 시작 줄에서 읽는다
	}{
		{"CI 성공", "/agent/ci", stageBody(`,"request_id":"gh-1"`), "test-value", "127.0.0.1:40000", "0123456789abcdef0123456789abcdef", "", false, 200, "gh-1"},
		{"CD 성공", "/agent/cd", stageBody(`,"request_id":"gh-2"`), "test-value", "10.0.0.1:40000", "fedcba9876543210fedcba9876543210", "", false, 200, "gh-2"},
		{"CI 단계 실패", "/agent/ci", stageBody(`,"request_id":"gh-3"`), "test-value", "127.0.0.1:40000", "trace-master-0901", "docker build --target test", false, 500, "gh-3"},
		{"작업 중", "/agent/ci", stageBody(`,"request_id":"gh-4"`), "test-value", "127.0.0.1:40000", "trace-master-0902", "", true, 409, "gh-4"},
		{"SHA 꼴 아님", "/agent/cd", `{"service":"llm","commit_sha":"bad","request_id":"gh-5"}`, "test-value", "127.0.0.1:40000", "trace-master-0903", "", false, 422, "gh-5"},
		{"인증 실패", "/agent/ci", stageBody(`,"request_id":"gh-6"`), "wrong-value", "127.0.0.1:40000", "trace-master-0904", "", false, 401, "invalid"},
		{"JSON 아님", "/agent/ci", `not json`, "test-value", "127.0.0.1:40000", "trace-master-0905", "", false, 422, "invalid"},
		{"꼴 틀린 일의 ID", "/agent/ci", stageBody(`,"request_id":"bad:MARKER"`), "test-value", "127.0.0.1:40000", "trace-master-0906", "", false, 200, ""},
		{"바깥 연결", "/agent/ci", stageBody(`,"request_id":"gh-7"`), "test-value", "198.51.100.20:40000", "trace-forged-0907", "", false, 200, "gh-7"},
	}
	for _, testCase := range cases {
		agent := newPipelineAgent(t, &recordedCommands{failOn: testCase.failOn})
		if testCase.busy {
			agent.busy.Lock()
		}
		var recorder *httptest.ResponseRecorder
		logged := captureStandardOutput(t, func() {
			recorder = trackedAgentCall(t, agent, "POST", testCase.path, testCase.body, testCase.secret, testCase.remoteAddress, testCase.trace)
		})
		if testCase.busy {
			agent.busy.Unlock()
		}
		if recorder.Code != testCase.status {
			t.Fatalf("%s: 상태 %d (기대 %d) %s", testCase.label, recorder.Code, testCase.status, recorder.Body.String())
		}
		stage := strings.TrimPrefix(testCase.path, "/agent/")
		trace := recorder.Result().Header.Get(shared.RequestIDHeader)
		if testCase.label == "바깥 연결" {
			if trace == testCase.trace || !agentTrackingIDPattern.MatchString(trace) {
				t.Fatalf("%s: 바깥의 값을 버리고 새 32자여야 한다: %q", testCase.label, trace)
			}
		} else if trace != testCase.trace {
			t.Fatalf("%s: 응답 머리글 %q (기대 %q)", testCase.label, trace, testCase.trace)
		}
		job := testCase.job
		if job == "" {
			started := agentLinesMatching(logged, regexp.MustCompile(`^req-[0-9a-f]+:ci-agent:agent `+stage+` start llm sha=`))
			if len(started) != 1 {
				t.Fatalf("%s: 에이전트가 만든 일의 ID 로 시작 줄이 있어야 한다:\n%s", testCase.label, logged)
			}
			job = strings.SplitN(started[0], ":", 2)[0]
		}
		want := trace + ":ci-agent:agent " + stage + " job=" + job + " -> " + strconv.Itoa(testCase.status)
		if linking := agentLinesMatching(logged, regexp.MustCompile(`:agent `+stage+` job=`)); len(linking) != 1 || linking[0] != want {
			t.Fatalf("%s: 잇는 줄 %q 하나여야 한다: %q\n%s", testCase.label, want, linking, logged)
		}
		for _, line := range agentLogLines(logged) {
			if strings.HasPrefix(line, trace+":") && line != want {
				t.Fatalf("%s: 추적 ID 로는 잇는 줄만 — 일의 ID 로 쓰던 줄이 추적 ID 로 바뀌었다: %q", testCase.label, line)
			}
		}
		if strings.Contains(logged, "MARKER") || strings.Contains(logged, "trace-forged-0907") {
			t.Fatalf("%s: 꼴 밖의 일의 ID 나 바깥의 추적 값이 로그에 실렸다:\n%s", testCase.label, logged)
		}
		if testCase.status == 200 {
			if lines := agentLinesMatching(logged, regexp.MustCompile(`^`+regexp.QuoteMeta(job)+`:ci-agent:agent `+stage+` start llm sha=`+requestSha+`$`)); len(lines) != 1 {
				t.Fatalf("%s: 시작 줄은 지금처럼 일의 ID 로: %q\n%s", testCase.label, lines, logged)
			}
			if lines := agentLinesMatching(logged, regexp.MustCompile(`^`+regexp.QuoteMeta(job)+`:ci-agent:agent `+stage+` ok llm sha=`+requestSha+` \d+ms$`)); len(lines) != 1 {
				t.Fatalf("%s: 끝 줄은 지금처럼 일의 ID 로: %q\n%s", testCase.label, lines, logged)
			}
		}
		if testCase.status == 500 {
			if lines := agentLinesMatching(logged, regexp.MustCompile(`^gh-3:ci-agent:agent ci failed llm stage=ci_test `)); len(lines) != 1 {
				t.Fatalf("%s: 실패 줄은 지금처럼 일의 ID 로:\n%s", testCase.label, logged)
			}
		}
	}
}

func TestAgentGitReadLogsWithTheJudgedTrackingID(t *testing.T) {
	t.Setenv("SERVER_NAME", "ci-agent")
	agent := newTestAgent(t)
	agent.gitProgram = "/nonexistent/git-for-tracking-test" // 실행하지 못함 → 500 git_failed 와 오류 줄
	target := "/agent/git/commit?service=llm&sha=" + requestSha
	for _, sent := range []struct {
		remoteAddress string
		incoming      []string
		trusted       bool
	}{
		{"127.0.0.1:40000", []string{"trace-master-1001"}, true},
		{"[::ffff:10.0.0.1]:40000", []string{"trace-master-1002"}, true},
		{"203.0.113.30:40000", []string{"trace-forged-1003"}, false},
		{"127.0.0.1:40000", []string{"trace-master-1004", "trace-master-1005"}, false},
		{"127.0.0.1:40000", []string{"bad:id"}, false},
	} {
		var recorder *httptest.ResponseRecorder
		logged := captureStandardOutput(t, func() {
			recorder = trackedAgentCall(t, agent, "GET", target, "", "test-value", sent.remoteAddress, sent.incoming...)
		})
		returned := recorder.Result().Header.Get(shared.RequestIDHeader)
		if sent.trusted && returned != sent.incoming[0] {
			t.Fatalf("%s %q: 받아야 할 값을 받지 않았다: %q", sent.remoteAddress, sent.incoming, returned)
		}
		if !sent.trusted && !agentTrackingIDPattern.MatchString(returned) {
			t.Fatalf("%s %q: 버리고 새 32자여야 한다: %q", sent.remoteAddress, sent.incoming, returned)
		}
		lines := agentLinesMatching(logged, regexp.MustCompile(`:ci-agent:agent git commit `))
		if len(lines) != 1 || !strings.HasPrefix(lines[0], returned+":ci-agent:agent git commit failed llm ") {
			t.Fatalf("%s %q: git 조회의 줄은 응답의 추적 ID(%q)로: %q\n%s", sent.remoteAddress, sent.incoming, returned, lines, logged)
		}
		for _, value := range sent.incoming {
			if !sent.trusted && strings.Contains(logged, value) {
				t.Fatalf("%s: 버린 값 %q 가 로그에 실렸다:\n%s", sent.remoteAddress, value, logged)
			}
		}
	}
}

// headerRecorder — 받은 요청의 X-Request-Id 를 차례대로 쌓는 대역(배포한 서비스의 헬스 · nginx 의 자리 확인 · 정리 알림).
type headerRecorder struct {
	mutex    sync.Mutex
	received [][]string
	server   *httptest.Server
}

func newHeaderRecorder(t *testing.T) *headerRecorder {
	t.Helper()
	recorder := &headerRecorder{}
	recorder.server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		recorder.mutex.Lock()
		recorder.received = append(recorder.received, request.Header.Values(shared.RequestIDHeader))
		recorder.mutex.Unlock()
		writer.Header().Set(slotHeader, slotBlue)
		writer.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(recorder.server.Close)
	return recorder
}

func (recorder *headerRecorder) values() [][]string {
	recorder.mutex.Lock()
	defer recorder.mutex.Unlock()
	return append([][]string(nil), recorder.received...)
}

func TestDeployProbesCarryFreshTrackingIDPerCall(t *testing.T) {
	recorder := newHeaderRecorder(t)
	for index := 0; index < 2; index++ {
		if err := waitHealthy(recorder.server.URL+"/health", 2*time.Second); err != nil {
			t.Fatalf("헬스: %v", err)
		}
		if slot, err := probeSlot(recorder.server.URL + "/health"); err != nil || slot != slotBlue {
			t.Fatalf("자리 확인: %q %v", slot, err)
		}
		if err := notifyDrain(recorder.server.URL + "/drain"); err != nil {
			t.Fatalf("정리 알림: %v", err)
		}
	}
	received := recorder.values()
	if len(received) != 6 {
		t.Fatalf("호출 %d번 (기대 6)", len(received))
	}
	seen := map[string]bool{}
	for index, values := range received {
		if len(values) != 1 || !agentTrackingIDPattern.MatchString(values[0]) {
			t.Fatalf("%d번째 호출: 새 추적 ID(16진수 소문자 32자) 하나를 실어야 한다: %q", index, values)
		}
		if seen[values[0]] {
			t.Fatalf("%d번째 호출: 호출마다 새 값이어야 한다 — 다시 나온 값 %q", index, values[0])
		}
		seen[values[0]] = true
	}
}

// 바깥에는 싣지 않는다: git 명령(GitHub 로 가는 fetch)의 인자 · 환경과 도커 API 요청에 추적 ID 가 없다.
func TestTrackingIDIsNotSentOutside(t *testing.T) {
	const trace = "trace-master-1101"
	recorded := &recordedCommands{runningImage: "llm-wrapper:" + runningSha}
	agent := newPipelineAgent(t, recorded)
	if recorder := trackedAgentCall(t, agent, "POST", "/agent/ci", stageBody(`,"request_id":"gh-1"`), "test-value", "127.0.0.1:40000", trace); recorder.Code != 200 ||
		recorder.Result().Header.Get(shared.RequestIDHeader) != trace {
		t.Fatalf("CI: %d %q %s", recorder.Code, recorder.Result().Header.Get(shared.RequestIDHeader), recorder.Body.String())
	}
	if !recorded.has("fetch") {
		t.Fatalf("fetch 를 부르지 않았다 — 이 시험의 전제가 깨졌다: %q", recorded.lines)
	}
	for index, line := range recorded.lines {
		if strings.Contains(line, trace) || strings.Contains(strings.ToLower(line), "request-id") || strings.Contains(line, "extraHeader") {
			t.Fatalf("명령 %q 에 추적 ID 가 실렸다", line)
		}
		for _, variable := range recorded.environments[index] {
			if strings.Contains(variable, trace) || strings.Contains(strings.ToLower(variable), "request") {
				t.Fatalf("명령 %q 의 환경 %q 에 추적 ID 가 실렸다", line, variable)
			}
		}
	}

	dockerHeaders := make(chan http.Header, 1)
	dockerServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		dockerHeaders <- request.Header.Clone()
		writer.Header().Set("Content-Type", "application/json")
		writer.Write([]byte(`[]`))
	}))
	defer dockerServer.Close()
	docker := &dockerAPI{baseURL: dockerServer.URL, client: dockerServer.Client()}
	// 추적된 요청의 context 에서 불러도(감싸개가 context 에 둔 값을 읽는 전송이 있어도) 도커에는 싣지 않는다
	handler := shared.TrackRequests(agent.logger, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var listed []dockerListedContainer
		if err := docker.decode(request.Context(), http.MethodGet, "/containers/json", nil, &listed); err != nil {
			t.Errorf("도커 대역: %v", err)
		}
	}))
	request := httptest.NewRequest("GET", "/", nil)
	request.RemoteAddr = "127.0.0.1:40000"
	request.Header.Set(shared.RequestIDHeader, trace)
	func() {
		defer failOnPanic(t)
		handler.ServeHTTP(httptest.NewRecorder(), request)
	}()
	select {
	case headers := <-dockerHeaders:
		if values := headers.Values(shared.RequestIDHeader); len(values) != 0 {
			t.Fatalf("도커 API 에 추적 ID 를 실었다: %q", values)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("도커 대역에 요청이 오지 않았다")
	}
}

// 배포 단계가 응답을 쓰기 전에 panic 하면 에이전트의 잇는 줄은 "-> none" 이다. panic 은 막지 않는다.
func TestAgentStageLinkingLineSaysNoneWhenStagePanicsBeforeWriting(t *testing.T) {
	t.Setenv("SERVER_NAME", "ci-agent")
	agent := newPipelineAgent(t, &recordedCommands{})
	agent.run = func(context.Context, string, []string, ...string) (string, error) { panic("runner exploded") }
	mux := http.NewServeMux()
	agent.Register(mux)
	handler := shared.TrackRequests(agent.logger, mux)
	request := httptest.NewRequest("POST", "/agent/ci", strings.NewReader(stageBody(`,"request_id":"gh-pn-9"`)))
	request.RemoteAddr = "127.0.0.1:40000"
	request.Header.Set(shared.SecretHeader, "test-value")
	request.Header.Set(shared.RequestIDHeader, "trace-master-1401")
	recorder := httptest.NewRecorder()
	var escaped any
	logged := captureStandardOutput(t, func() {
		defer func() { escaped = recover() }()
		handler.ServeHTTP(recorder, request)
	})
	if escaped != "runner exploded" {
		t.Fatalf("panic 이 그대로 올라가야 한다: %v", escaped)
	}
	if recorder.Body.Len() != 0 {
		t.Fatalf("panic 대신 응답을 썼다: %d %q", recorder.Code, recorder.Body.String())
	}
	want := "trace-master-1401:ci-agent:agent ci job=gh-pn-9 -> none"
	if lines := agentLinesMatching(logged, regexp.MustCompile(`^trace-master-1401:`)); len(lines) != 1 || lines[0] != want {
		t.Fatalf("잇는 줄 %q 하나여야 한다: %q\n%s", want, lines, logged)
	}
	if agent.busy.TryLock() {
		agent.busy.Unlock()
	} else {
		t.Fatal("panic 뒤에 작업 잠금이 풀리지 않았다")
	}
}
