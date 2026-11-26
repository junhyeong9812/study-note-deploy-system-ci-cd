package master

// 마스터의 추적 ID(통신 규약 1.2 · 1.3 · 1.4 #9 · #10 · #11, 7.1 T2 ci-cd).
//   - 받는 경로 모두: 응답 머리글 X-Request-Id = 그 요청의 추적 ID(shared.TrackRequests 를 mux 전체에 씌운 모습으로 본다 — main.go 가 그렇게 씌운다).
//   - 배포 접수: 본문 request_id(일의 ID)의 동작은 그대로. 추적 ID 는 머리글에서. 두 ID 를 잇는 줄 한 줄:
//       <추적 ID>:ci-master:deploy accept job=<일의 ID 또는 invalid> -> <상태>
//     배포 상태 · 실패 출력의 조회도 같은 꼴(경로의 id 가 일의 ID — 꼴이 맞을 때만, 아니면 invalid):
//       <추적 ID>:ci-master:deploy status job=<…> -> <상태>   ·   <추적 ID>:ci-master:deploy output job=<…> -> <상태>
//     일의 ID 는 접수가 쓴 값(본문 값, 없거나 꼴이 틀리면 마스터가 만든 req-…). 그 값을 정하기 전에 끝난 요청(인증 실패 · 본문을 해석하지 못함)은 invalid.
//     지금 일의 ID 로 쓰는 줄(deploy accepted · deploy rejected · deploy failed · deploy succeeded …)은 그대로 일의 ID 로 남는다(1.2 — 추적 화면은 일의 ID 로 배포 하나를 찾는다).
//   - 배포의 진행(마스터 → 에이전트 /agent/ci · /agent/cd)은 들어온 요청 없이 시작되는 호출이다(1.2): 호출마다 새 32자, 잇는 줄
//       <새 추적 ID>:ci-master:deploy <ci|cd> call job=<일의 ID> -> <상태 | unreachable>
//     접수 요청의 추적 ID 를 끌고 가지 않는다. 본문은 지금 그대로(service · commit_sha · request_id=일의 ID).
//   - git 조회 중계는 들어온 요청 때문에 부르는 호출이다: 그 요청의 추적 ID(판정한 값)를 싣는다.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/junhyeong9812/study-note-deploy-system-ci-cd/internal/shared"
)

var masterTrackingIDPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

// trackedMasterCall — main.go 처럼 mux 전체에 감싸개를 씌우고, 정한 연결 주소 · 추적 머리글로 한 번 부른다.
func trackedMasterCall(t *testing.T, master *Master, method, target, body, secret, remoteAddress string, incoming ...string) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	master.Register(mux)
	handler := shared.TrackRequests(master.logger, mux)
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
		defer func() {
			if recovered := recover(); recovered != nil {
				t.Fatalf("%s %s: panic: %v", method, target, recovered)
			}
		}()
		handler.ServeHTTP(recorder, request)
	}()
	return recorder
}

// logLines — 로그에서 "<추적 ID>:<서버>:<내용>" 부분만(시각 · 수준을 뗀다).
func logLines(logged string) []string {
	lines := []string{}
	for _, line := range strings.Split(logged, "\n") {
		fields := strings.SplitN(line, " ", 3)
		if len(fields) == 3 {
			lines = append(lines, fields[2])
		}
	}
	return lines
}

func linesMatching(logged string, pattern *regexp.Regexp) []string {
	matched := []string{}
	for _, line := range logLines(logged) {
		if pattern.MatchString(line) {
			matched = append(matched, line)
		}
	}
	return matched
}

func TestMasterRoutesReturnTrackingIDOnEveryResponse(t *testing.T) {
	t.Setenv("SERVER_NAME", "ci-master")
	newFakeAgent(t) // git 조회의 성공 경로는 TestGitRelayCarriesTheJudgedTrackingIDOfTheRequest 가 본다
	master := newTestMaster(t)
	if recorder := submit(master, "gh-track-1"); recorder.Code != 202 {
		t.Fatalf("준비 접수 실패: %d %s", recorder.Code, recorder.Body.String())
	}
	waitFinished(t, master, "gh-track-1")

	cases := []struct {
		method, target, body, secret string
		status                       int
	}{
		{"POST", "/deploy", `{"service":"llm","commit_sha":"` + deploySha + `","request_id":"gh-track-1"}`, "test-value", 202},
		{"POST", "/deploy", `{}`, "", 401},
		{"POST", "/deploy", `{"service":"llm","commit_sha":"not-hex"}`, "test-value", 422},
		{"POST", "/deploy", `{"service":"llm","commit_sha":"` + deploySha + `","request_id":"gh-track-1","requested_by":"admin:x"}`, "test-value", 202},
		{"POST", "/deploy", `{"service":"llm","commit_sha":"abcdef1234","request_id":"gh-track-1"}`, "test-value", 409},
		{"GET", "/deploys/gh-track-1", "", "test-value", 200},
		{"GET", "/deploys/gh-none", "", "test-value", 404},
		{"GET", "/deploys/gh-track-1", "", "", 401},
		{"GET", "/deploys/gh-track-1/output", "", "test-value", 409}, // 성공한 건 — not_failed
		{"GET", "/deploys/bad.id/output", "", "test-value", 422},
		{"GET", "/deploys/gh-track-1/output", "", "", 401},
		{"GET", "/deploys?limit=5", "", "test-value", 200},
		{"GET", "/status", "", "test-value", 200},
		{"GET", "/status", "", "", 401},
		{"GET", "/deploys/git/commits?service=evil", "", "test-value", 422},
		{"GET", "/deploys/git/commit?service=llm&sha=" + relayToSha, "", "", 401},
		{"GET", "/no-such-route", "", "test-value", 404},
		{"DELETE", "/status", "", "test-value", 405},
	}
	for _, testCase := range cases {
		label := testCase.method + " " + testCase.target
		trusted := trackedMasterCall(t, master, testCase.method, testCase.target, testCase.body, testCase.secret, "10.0.0.1:40000", "trace-front-0101")
		if trusted.Code != testCase.status {
			t.Fatalf("%s: 상태가 %d (기대 %d) %s", label, trusted.Code, testCase.status, trusted.Body.String())
		}
		if got := trusted.Result().Header.Values(shared.RequestIDHeader); len(got) != 1 || got[0] != "trace-front-0101" {
			t.Errorf("%s (%d): 사설 연결의 추적 ID 를 응답 머리글로 돌려줘야 한다: %q", label, trusted.Code, got)
		}
		outside := trackedMasterCall(t, master, testCase.method, testCase.target, testCase.body, testCase.secret, "203.0.113.7:40000", "trace-forged-0101")
		if got := outside.Result().Header.Get(shared.RequestIDHeader); !masterTrackingIDPattern.MatchString(got) {
			t.Errorf("%s (%d): 바깥 연결의 값은 버리고 새 32자를 돌려줘야 한다: %q", label, outside.Code, got)
		}
	}
}

// 감싸개를 씌워도 마스터의 상태 · 본문은 바뀌지 않는다(같은 요청을 감싸개 없이 보낸 새 마스터와 바이트까지 같다).
func TestMasterResponsesUnchangedByTracking(t *testing.T) {
	newFakeAgent(t)
	requests := []struct{ method, target, body, secret string }{
		{"POST", "/deploy", `{"service":"llm","commit_sha":"` + deploySha + `","request_id":"gh-same-1"}`, "test-value"},
		{"POST", "/deploy", `{"service":"evil","commit_sha":"` + deploySha + `"}`, "test-value"},
		{"POST", "/deploy", `{"service":"llm","commit_sha":"bad"}`, "test-value"},
		{"POST", "/deploy", `not json`, "test-value"},
		{"POST", "/deploy", `{}`, "wrong-value"},
		{"GET", "/deploys/gh-none", "", "test-value"},
		{"GET", "/deploys/bad.id/output", "", "test-value"},
		{"GET", "/deploys/gh-none/output", "", "test-value"},
		{"GET", "/deploys/git/commits?service=llm&from=HEAD&to=" + relayToSha, "", "test-value"},
		{"GET", "/status", "", ""},
		{"GET", "/no-such-route", "", "test-value"},
	}
	bareMaster, trackedMaster := newTestMaster(t), newTestMaster(t)
	bareMux := http.NewServeMux()
	bareMaster.Register(bareMux)
	for _, sent := range requests {
		bareRequest := httptest.NewRequest(sent.method, sent.target, strings.NewReader(sent.body))
		if sent.secret != "" {
			bareRequest.Header.Set(shared.SecretHeader, sent.secret)
		}
		bare := httptest.NewRecorder()
		bareMux.ServeHTTP(bare, bareRequest)
		tracked := trackedMasterCall(t, trackedMaster, sent.method, sent.target, sent.body, sent.secret, "10.0.0.1:40000", "trace-front-0102")
		if bare.Code != tracked.Code || bare.Body.String() != tracked.Body.String() {
			t.Fatalf("%s %s: 감싸개가 답을 바꿨다: %d %q → %d %q", sent.method, sent.target, bare.Code, bare.Body.String(), tracked.Code, tracked.Body.String())
		}
		if bare.Header().Get("Content-Type") != tracked.Result().Header.Get("Content-Type") {
			t.Fatalf("%s %s: Content-Type 이 바뀌었다", sent.method, sent.target)
		}
	}
	waitSettled(t, bareMaster, "gh-same-1")
	waitSettled(t, trackedMaster, "gh-same-1")
}

func TestDeployIntakeLinksTrackingIDAndJobIDInOneLine(t *testing.T) {
	t.Setenv("SERVER_NAME", "ci-master")
	newFakeAgent(t)
	master := newTestMaster(t)
	body := func(fields string) string { return `{"service":"llm","commit_sha":"` + deploySha + `"` + fields + `}` }

	cases := []struct {
		label, body, secret, trace string
		status                     int
		job                        string // 빈 값이면 마스터가 만든 req-… 를 응답 본문에서 읽어 맞춘다
	}{
		{"새 접수", body(`,"request_id":"gh-77-1"`), "test-value", "trace-front-0201", 202, "gh-77-1"},
		{"같은 일의 재접수", body(`,"request_id":"gh-77-1"`), "test-value", "trace-front-0202", 202, "gh-77-1"},
		{"같은 일 · 다른 커밋", `{"service":"llm","commit_sha":"abcdef1234","request_id":"gh-77-1"}`, "test-value", "trace-front-0203", 409, "gh-77-1"},
		{"SHA 꼴 아님", `{"service":"llm","commit_sha":"bad-sha","request_id":"gh-77-2"}`, "test-value", "trace-front-0204", 422, "gh-77-2"},
		{"서비스 없음", `{"commit_sha":"` + deploySha + `","request_id":"gh-77-3"}`, "test-value", "trace-front-0205", 422, "gh-77-3"},
		{"인증 실패", body(`,"request_id":"gh-77-4"`), "wrong-value", "trace-front-0206", 401, "invalid"},
		{"JSON 아님", `not json`, "test-value", "trace-front-0207", 422, "invalid"},
		{"허용 목록 밖 서비스", `{"service":"evil","commit_sha":"` + deploySha + `","request_id":"gh-77-8"}`, "test-value", "trace-front-0208", 422, "gh-77-8"},
	}
	for _, testCase := range cases {
		var recorder *httptest.ResponseRecorder
		logged := captureMasterLog(t, func() {
			recorder = trackedMasterCall(t, master, "POST", "/deploy", testCase.body, testCase.secret, "10.0.0.1:40000", testCase.trace)
			if testCase.label == "새 접수" && recorder.Code == 202 {
				waitSettled(t, master, "gh-77-1") // 진행의 goroutine 이 표준 출력을 바꿔 끼우는 동안에 끝나게
			}
		})
		if recorder.Code != testCase.status {
			t.Fatalf("%s: 상태 %d (기대 %d) %s", testCase.label, recorder.Code, testCase.status, recorder.Body.String())
		}
		want := testCase.trace + ":ci-master:deploy accept job=" + testCase.job + " -> " + itoa(testCase.status)
		linking := linesMatching(logged, regexp.MustCompile(`deploy accept job=`))
		if len(linking) != 1 || linking[0] != want {
			t.Fatalf("%s: 잇는 줄은 하나 %q 여야 한다: %q\n%s", testCase.label, want, linking, logged)
		}
		for _, line := range logLines(logged) {
			if strings.HasPrefix(line, testCase.trace+":") && line != want {
				t.Fatalf("%s: 추적 ID 로는 잇는 줄만 남는다 — 일의 ID 로 쓰던 줄이 추적 ID 로 바뀌었다: %q", testCase.label, line)
			}
		}
		if testCase.label == "허용 목록 밖 서비스" {
			if rejected := linesMatching(logged, regexp.MustCompile(`^gh-77-8:ci-master:deploy rejected: unknown service "evil"$`)); len(rejected) != 1 {
				t.Fatalf("거절 줄은 지금처럼 일의 ID 로 남아야 한다:\n%s", logged)
			}
		}
		if testCase.label == "새 접수" {
			if accepted := linesMatching(logged, regexp.MustCompile(`^gh-77-1:ci-master:deploy accepted llm sha=`+deploySha+`$`)); len(accepted) != 1 {
				t.Fatalf("접수 줄은 지금처럼 일의 ID 로 남아야 한다:\n%s", logged)
			}
		}
	}
}

// 본문 request_id 가 꼴에 맞지 않으면 마스터가 일의 ID 를 만든다(지금 그대로). 잇는 줄의 job= 은 그 만든 값이고, 꼴 밖의 글자는 로그에 실리지 않는다.
func TestDeployIntakeLinkingLineNeverCarriesMalformedJobText(t *testing.T) {
	t.Setenv("SERVER_NAME", "ci-master")
	newFakeAgent(t)
	master := newTestMaster(t)
	for index, requestIDField := range []string{`,"request_id":"has:colon-MARKER"`, `,"request_id":"gh\nMARKER injected"`, `,"request_id":"` + strings.Repeat("M", 65) + `"`, ``, `,"request_id":""`} {
		trace := "trace-front-030" + itoa(index)
		var recorder *httptest.ResponseRecorder
		logged := captureMasterLog(t, func() {
			recorder = trackedMasterCall(t, master, "POST", "/deploy", `{"service":"llm","commit_sha":"`+deploySha+`"`+requestIDField+`}`, "test-value", "10.0.0.1:40000", trace)
			if recorder.Code == 202 {
				var accepted struct {
					Data struct {
						RequestID string `json:"request_id"`
					} `json:"data"`
				}
				json.Unmarshal(recorder.Body.Bytes(), &accepted)
				waitSettled(t, master, accepted.Data.RequestID)
			}
		})
		if recorder.Code != 202 {
			t.Fatalf("%q: 접수 %d %s", requestIDField, recorder.Code, recorder.Body.String())
		}
		var envelope struct {
			Data struct {
				RequestID string `json:"request_id"`
			} `json:"data"`
		}
		json.Unmarshal(recorder.Body.Bytes(), &envelope)
		issued := envelope.Data.RequestID
		if !regexp.MustCompile(`^req-[0-9a-f]+$`).MatchString(issued) {
			t.Fatalf("%q: 마스터가 만드는 일의 ID 꼴이 바뀌었다: %q", requestIDField, issued)
		}
		if issued == trace {
			t.Fatalf("%q: 추적 ID 가 일의 ID 로 올라갔다", requestIDField)
		}
		want := trace + ":ci-master:deploy accept job=" + issued + " -> 202"
		if linking := linesMatching(logged, regexp.MustCompile(`deploy accept job=`)); len(linking) != 1 || linking[0] != want {
			t.Fatalf("%q: 잇는 줄 %q 여야 한다: %q", requestIDField, want, linking)
		}
		if strings.Contains(logged, "MARKER") || strings.Contains(logged, strings.Repeat("M", 65)) {
			t.Fatalf("%q: 꼴 밖의 본문 글자가 로그에 실렸다:\n%s", requestIDField, logged)
		}
	}
}

// 일의 ID 는 본문에서만 온다 — 추적 머리글이 일의 ID 꼴이어도 일의 ID 가 되지 않고, 추적 ID 가 달라도 멱등은 그대로다.
func TestJobIDComesOnlyFromBodyAndIdempotencyIgnoresTrackingID(t *testing.T) {
	newFakeAgent(t)
	master := newTestMaster(t)
	recorder := trackedMasterCall(t, master, "POST", "/deploy", `{"service":"llm","commit_sha":"`+deploySha+`"}`, "test-value", "10.0.0.1:40000", "gh-55-1")
	if recorder.Code != 202 || strings.Contains(recorder.Body.String(), `"gh-55-1"`) {
		t.Fatalf("본문에 request_id 가 없으면 마스터가 일의 ID 를 만든다 — 추적 머리글을 일의 ID 로 쓰지 않는다: %d %s", recorder.Code, recorder.Body.String())
	}
	if lookupMissing := call(master, "GET", "/deploys/gh-55-1", ""); lookupMissing.Code != 404 {
		t.Fatalf("추적 머리글의 값으로 배포가 기록됐다: %d %s", lookupMissing.Code, lookupMissing.Body.String())
	}
	var envelope struct {
		Data struct {
			RequestID string `json:"request_id"`
		} `json:"data"`
	}
	json.Unmarshal(recorder.Body.Bytes(), &envelope)
	waitSettled(t, master, envelope.Data.RequestID)

	first := trackedMasterCall(t, master, "POST", "/deploy", `{"service":"llm","commit_sha":"`+deploySha+`","request_id":"gh-55-2"}`, "test-value", "10.0.0.1:40000", "trace-front-0401")
	second := trackedMasterCall(t, master, "POST", "/deploy", `{"service":"llm","commit_sha":"`+deploySha+`","request_id":"gh-55-2"}`, "test-value", "10.0.0.1:40000", "trace-front-0402")
	if first.Code != 202 || second.Code != 202 || !strings.Contains(second.Body.String(), `"duplicate":true`) || !strings.Contains(second.Body.String(), `"request_id":"gh-55-2"`) {
		t.Fatalf("같은 일의 ID 는 추적 ID 와 상관없이 재접수다: %s / %s", first.Body.String(), second.Body.String())
	}
	if first.Result().Header.Get(shared.RequestIDHeader) != "trace-front-0401" || second.Result().Header.Get(shared.RequestIDHeader) != "trace-front-0402" {
		t.Fatalf("응답 머리글은 각 요청의 추적 ID 다: %q %q", first.Result().Header.Get(shared.RequestIDHeader), second.Result().Header.Get(shared.RequestIDHeader))
	}

	// 들어온 머리글이 없으면 새 32자 — 본문의 일의 ID 를 머리글로 올리지 않는다
	third := trackedMasterCall(t, master, "POST", "/deploy", `{"service":"llm","commit_sha":"`+deploySha+`","request_id":"gh-55-2"}`, "test-value", "10.0.0.1:40000")
	if got := third.Result().Header.Get(shared.RequestIDHeader); got == "gh-55-2" || !masterTrackingIDPattern.MatchString(got) {
		t.Fatalf("일의 ID 가 응답의 추적 머리글로 올라갔다: %q", got)
	}
	waitSettled(t, master, "gh-55-2")
}

// trackingAgent — /agent/ci · /agent/cd 호출의 머리글 · 본문을 기록하는 가짜 에이전트. 처음 busyTimes 번은 409 agent_busy.
type trackingAgent struct {
	mutex     sync.Mutex
	busyTimes int
	received  []trackingAgentCall
}

type trackingAgentCall struct {
	path      string
	requestID []string
	body      map[string]any
}

func newTrackingAgent(t *testing.T, busyTimes int) *trackingAgent {
	t.Helper()
	agent := &trackingAgent{busyTimes: busyTimes}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		raw, _ := io.ReadAll(request.Body)
		var body map[string]any
		json.Unmarshal(raw, &body)
		agent.mutex.Lock()
		agent.received = append(agent.received, trackingAgentCall{path: request.URL.Path, requestID: request.Header.Values(shared.RequestIDHeader), body: body})
		busy := agent.busyTimes > 0
		if busy {
			agent.busyTimes--
		}
		agent.mutex.Unlock()
		if busy {
			shared.WriteFail(writer, http.StatusConflict, "agent_busy", "")
			return
		}
		shared.WriteOK(writer, map[string]any{"took_ms": 1})
	}))
	t.Cleanup(server.Close)
	t.Setenv("AGENT_URL_LLM", server.URL)
	return agent
}

func (agent *trackingAgent) calls() []trackingAgentCall {
	agent.mutex.Lock()
	defer agent.mutex.Unlock()
	return append([]trackingAgentCall(nil), agent.received...)
}

func TestDeployProgressCallsCarryFreshTrackingIDPerCallAndLinkTheJob(t *testing.T) {
	t.Setenv("SERVER_NAME", "ci-master")
	agent := newTrackingAgent(t, 2)
	master := newTestMaster(t)
	var intake *httptest.ResponseRecorder
	logged := captureMasterLog(t, func() {
		intake = trackedMasterCall(t, master, "POST", "/deploy", `{"service":"llm","commit_sha":"`+deploySha+`","request_id":"gh-88-1"}`, "test-value", "10.0.0.1:40000", "trace-front-0501")
		if intake.Code == 202 {
			waitSettled(t, master, "gh-88-1")
		}
	})
	if intake.Code != 202 {
		t.Fatalf("접수 %d %s", intake.Code, intake.Body.String())
	}
	calls := agent.calls()
	wantPaths, wantStatuses := []string{"/agent/ci", "/agent/ci", "/agent/ci", "/agent/cd"}, []string{"409", "409", "200", "200"}
	if len(calls) != len(wantPaths) {
		t.Fatalf("에이전트 호출 %d번 (기대 %d): %+v", len(calls), len(wantPaths), calls)
	}
	seen := map[string]bool{}
	for index, received := range calls {
		if received.path != wantPaths[index] {
			t.Fatalf("%d번째 호출 경로 %q (기대 %q)", index, received.path, wantPaths[index])
		}
		if len(received.requestID) != 1 || !masterTrackingIDPattern.MatchString(received.requestID[0]) {
			t.Fatalf("%d번째 %s: 배포의 진행 호출에 새 추적 ID(16진수 소문자 32자) 하나를 실어야 한다: %q", index, received.path, received.requestID)
		}
		identifier := received.requestID[0]
		if identifier == "trace-front-0501" || identifier == "gh-88-1" {
			t.Fatalf("%d번째 %s: 접수의 추적 ID · 일의 ID 를 끌고 갔다: %q", index, received.path, identifier)
		}
		if seen[identifier] {
			t.Fatalf("%d번째 %s: 호출마다 새 값이어야 한다 — 다시 나온 값 %q", index, received.path, identifier)
		}
		seen[identifier] = true
		if len(received.body) != 3 || received.body["service"] != "llm" || received.body["commit_sha"] != deploySha || received.body["request_id"] != "gh-88-1" {
			t.Fatalf("%d번째 %s: 본문이 바뀌었다(일의 ID 는 본문 request_id 그대로): %v", index, received.path, received.body)
		}
		stage := strings.TrimPrefix(received.path, "/agent/")
		want := identifier + ":ci-master:deploy " + stage + " call job=gh-88-1 -> " + wantStatuses[index]
		if linking := linesMatching(logged, regexp.MustCompile(`^`+identifier+`:`)); len(linking) != 1 || linking[0] != want {
			t.Fatalf("%d번째 %s: 잇는 줄 %q 하나여야 한다: %q", index, received.path, want, linking)
		}
	}
	if succeeded := linesMatching(logged, regexp.MustCompile(`^gh-88-1:ci-master:deploy succeeded llm sha=`+deploySha+` ci=\d+ms cd=\d+ms$`)); len(succeeded) != 1 {
		t.Fatalf("진행의 줄은 지금처럼 일의 ID 로 남아야 한다:\n%s", logged)
	}
	for _, line := range logLines(logged) {
		if strings.HasPrefix(line, "trace-front-0501:") && !strings.Contains(line, "deploy accept job=") {
			t.Fatalf("접수의 추적 ID 가 뒤쪽 진행의 줄에 실렸다: %q", line)
		}
	}
}

func TestDeployProgressLinkingLineWhenAgentUnreachable(t *testing.T) {
	t.Setenv("SERVER_NAME", "ci-master")
	t.Setenv("AGENT_URL_LLM", "http://127.0.0.1:1") // 닫힌 포트
	master := newTestMaster(t)
	logged := captureMasterLog(t, func() {
		if recorder := trackedMasterCall(t, master, "POST", "/deploy", `{"service":"llm","commit_sha":"`+deploySha+`","request_id":"gh-88-2"}`, "test-value", "10.0.0.1:40000", "trace-front-0502"); recorder.Code == 202 {
			waitSettled(t, master, "gh-88-2")
		}
	})
	linking := linesMatching(logged, regexp.MustCompile(`:ci-master:deploy ci call job=gh-88-2 -> unreachable$`))
	if len(linking) != 1 || !masterTrackingIDPattern.MatchString(strings.SplitN(linking[0], ":", 2)[0]) {
		t.Fatalf("닿지 못한 호출도 새 추적 ID 로 잇는 줄 하나: %q\n%s", linking, logged)
	}
	if failed := linesMatching(logged, regexp.MustCompile(`^gh-88-2:ci-master:deploy failed llm sha=`+deploySha+` stage=agent_unreachable `)); len(failed) != 1 {
		t.Fatalf("실패 줄은 지금처럼 일의 ID 로 남아야 한다:\n%s", logged)
	}
}

// trackingGitAgent — /agent/git/* 호출의 추적 머리글을 기록한다.
func trackingGitAgent(t *testing.T, status int, body string) *[]([]string) {
	t.Helper()
	var mutex sync.Mutex
	received := &[]([]string){}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		mutex.Lock()
		*received = append(*received, request.Header.Values(shared.RequestIDHeader))
		mutex.Unlock()
		writer.Header().Set("Content-Type", "application/json; charset=utf-8")
		writer.WriteHeader(status)
		io.WriteString(writer, body)
	}))
	t.Cleanup(server.Close)
	t.Setenv("AGENT_URL_LLM", server.URL)
	return received
}

func TestGitRelayCarriesTheJudgedTrackingIDOfTheRequest(t *testing.T) {
	received := trackingGitAgent(t, 200, `{"success":true,"data":{"commits":[],"truncated":false,"range_complete":true,"relation":"ancestor"}}`)
	master := newTestMaster(t)
	target := relayCommitsTarget("llm", relayFromSha, relayToSha)

	trusted := trackedMasterCall(t, master, "GET", target, "", "test-value", "10.0.0.1:40000", "trace-front-0601")
	outside := trackedMasterCall(t, master, "GET", target, "", "test-value", "198.51.100.9:40000", "trace-forged-0601")
	absent := trackedMasterCall(t, master, "GET", target, "", "test-value", "127.0.0.1:40000")
	twoHeaders := trackedMasterCall(t, master, "GET", target, "", "test-value", "10.0.0.1:40000", "trace-front-0602", "trace-front-0603")
	if len(*received) != 4 {
		t.Fatalf("에이전트 호출 %d번 (기대 4)", len(*received))
	}
	for index, recorder := range []*httptest.ResponseRecorder{trusted, outside, absent, twoHeaders} {
		if recorder.Code != 200 {
			t.Fatalf("%d: 중계 %d %s", index, recorder.Code, recorder.Body.String())
		}
		forwarded := (*received)[index]
		returned := recorder.Result().Header.Get(shared.RequestIDHeader)
		if len(forwarded) != 1 || forwarded[0] != returned {
			t.Fatalf("%d: 에이전트에 실은 추적 ID(%q)가 응답 머리글(%q)과 다르다", index, forwarded, returned)
		}
	}
	if trusted.Result().Header.Get(shared.RequestIDHeader) != "trace-front-0601" {
		t.Fatalf("사설 연결의 추적 ID 를 그대로 실어야 한다: %q", trusted.Result().Header.Get(shared.RequestIDHeader))
	}
	for index, recorder := range []*httptest.ResponseRecorder{outside, absent, twoHeaders} {
		if got := recorder.Result().Header.Get(shared.RequestIDHeader); !masterTrackingIDPattern.MatchString(got) {
			t.Fatalf("%d: 받지 못한 값 대신 새 32자를 실어야 한다: %q", index, got)
		}
	}
}

func TestGitRelayFailureLogUsesTheResponsesTrackingID(t *testing.T) {
	t.Setenv("SERVER_NAME", "ci-master")
	trackingGitAgent(t, 200, `not an envelope`)
	master := newTestMaster(t)
	for _, remoteAddress := range []string{"10.0.0.1:40000", "203.0.113.7:40000"} {
		var recorder *httptest.ResponseRecorder
		logged := captureMasterLog(t, func() {
			recorder = trackedMasterCall(t, master, "GET", relayCommitsTarget("llm", relayFromSha, relayToSha), "", "test-value", remoteAddress, "trace-front-0701")
		})
		requireRelayFailure(t, recorder, 502, "unexpected_response")
		returned := recorder.Result().Header.Get(shared.RequestIDHeader)
		want := returned + ":ci-master:git relay failed llm /agent/git/commits: unexpected_response"
		if lines := linesMatching(logged, regexp.MustCompile(`git relay failed`)); len(lines) != 1 || lines[0] != want {
			t.Fatalf("%s: 중계 실패 줄은 응답의 추적 ID 로 %q: %q", remoteAddress, want, lines)
		}
		if remoteAddress == "203.0.113.7:40000" && strings.Contains(logged, "trace-front-0701") {
			t.Fatalf("바깥 연결의 값이 로그에 실렸다:\n%s", logged)
		}
	}
}

func TestDeploymentLookupsLinkTrackingIDAndPathJobID(t *testing.T) {
	t.Setenv("SERVER_NAME", "ci-master")
	newFakeAgent(t)
	master := newTestMaster(t)
	if recorder := submit(master, "gh-66-1"); recorder.Code != 202 {
		t.Fatalf("준비 접수: %d %s", recorder.Code, recorder.Body.String())
	}
	waitSettled(t, master, "gh-66-1")
	cases := []struct {
		target, secret, trace string
		status                int
		want                  string
	}{
		{"/deploys/gh-66-1", "test-value", "trace-front-1201", 200, "deploy status job=gh-66-1 -> 200"},
		{"/deploys/gh-none", "test-value", "trace-front-1202", 404, "deploy status job=gh-none -> 404"},
		{"/deploys/bad.MARKER", "test-value", "trace-front-1203", 404, "deploy status job=invalid -> 404"},
		{"/deploys/gh-66-1", "", "trace-front-1204", 401, "deploy status job=gh-66-1 -> 401"},
		{"/deploys/gh-66-1/output", "test-value", "trace-front-1205", 409, "deploy output job=gh-66-1 -> 409"},
		{"/deploys/bad.MARKER/output", "test-value", "trace-front-1206", 422, "deploy output job=invalid -> 422"},
		{"/deploys/gh-none/output", "test-value", "trace-front-1207", 404, "deploy output job=gh-none -> 404"},
		{"/deploys/gh-66-1/output", "", "trace-front-1208", 401, "deploy output job=gh-66-1 -> 401"},
	}
	for _, testCase := range cases {
		var recorder *httptest.ResponseRecorder
		logged := captureMasterLog(t, func() {
			recorder = trackedMasterCall(t, master, "GET", testCase.target, "", testCase.secret, "10.0.0.1:40000", testCase.trace)
		})
		if recorder.Code != testCase.status || recorder.Result().Header.Get(shared.RequestIDHeader) != testCase.trace {
			t.Fatalf("%s: %d %q (기대 %d %q)", testCase.target, recorder.Code, recorder.Result().Header.Get(shared.RequestIDHeader), testCase.status, testCase.trace)
		}
		want := testCase.trace + ":ci-master:" + testCase.want
		if lines := linesMatching(logged, regexp.MustCompile(`^`+testCase.trace+`:`)); len(lines) != 1 || lines[0] != want {
			t.Fatalf("%s: 잇는 줄 %q 하나여야 한다: %q\n%s", testCase.target, want, lines, logged)
		}
		if strings.Contains(logged, "MARKER") {
			t.Fatalf("%s: 꼴 밖의 경로 글자가 로그에 실렸다:\n%s", testCase.target, logged)
		}
	}
}
