package master

// 마스터가 받은 요청마다 쓰는 접속 줄(통신 규약 3.4 · 7.1 의 T4 | ci-cd 줄 · ⑬ T4c). main 처럼 TrackRequests(logger, WriteAccessLines("ci-master", mux)) 로 본다.
//   - 모든 받는 길(접수 · 조회 · 실패 출력 · 목록 · 상태 · git 중계 · 경로 없음 · 메서드 · 인증 실패 · 큰 본문)에 정확히 한 줄, service = ci-master.
//   - request_id = 응답 머리글 = 그 요청의 로그 줄(잇는 줄 · 중계 실패 줄)의 ID = git 중계가 에이전트로 싣는 X-Request-Id.
//   - caller = 사설 대역에서 온 닫힌 목록의 이름(front · admin · alert …) · 그 밖은 outside.
//   - 마스터가 에이전트를 부르면(T4b 의 X-Caller-Service: ci-master), 받는 에이전트의 줄에 caller = ci-master 와 마스터가 실은 추적 ID 가 적힌다.
//   - 지금의 로그 줄 · 응답은 그대로.

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

var masterTrackingShape = regexp.MustCompile(`^[0-9a-f]{32}$`)

// masterAccessLines — 표준 출력에서 {"v": 로 시작하는 줄을 칸 이름 → 값으로 읽는다(읽지 못하면 실패).
func masterAccessLines(t *testing.T, captured string) []map[string]any {
	t.Helper()
	lines := []map[string]any{}
	for _, text := range strings.Split(captured, "\n") {
		if !strings.HasPrefix(text, `{"v":`) {
			if strings.Contains(text, `{"v":`) {
				t.Fatalf("접속 줄이 다른 글과 한 줄에 섞였다: %q", text)
			}
			continue
		}
		var line map[string]any
		if err := json.Unmarshal([]byte(text), &line); err != nil {
			t.Fatalf("접속 줄이 JSON 이 아니다: %v %q", err, text)
		}
		lines = append(lines, line)
	}
	return lines
}

func accessCall(t *testing.T, master *Master, method, target, body, secret, remoteAddress string, headers map[string]string) (*httptest.ResponseRecorder, string) {
	t.Helper()
	mux := http.NewServeMux()
	master.Register(mux)
	handler := shared.TrackRequests(master.logger, shared.WriteAccessLines("ci-master", mux))
	request := httptest.NewRequest(method, target, strings.NewReader(body))
	request.RemoteAddr = remoteAddress
	if secret != "" {
		request.Header.Set(shared.SecretHeader, secret)
	}
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	recorder := httptest.NewRecorder()
	captured := captureMasterLog(t, func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				t.Errorf("%s %s: panic: %v", method, target, recovered)
			}
		}()
		handler.ServeHTTP(recorder, request)
	})
	return recorder, captured
}

func TestMasterWritesOneAccessLinePerRequestOnEveryRoute(t *testing.T) {
	t.Setenv("SERVER_NAME", "ci-master")
	newFakeAgent(t)
	master := newTestMaster(t)
	if recorder := submit(master, "gh-access-1"); recorder.Code != 202 {
		t.Fatalf("준비 접수: %d", recorder.Code)
	}
	waitSettled(t, master, "gh-access-1")
	bigBody := `{"service":"llm","commit_sha":"` + deploySha + `","pad":"` + strings.Repeat("x", 8000) + `"}`
	cases := []struct {
		method, target, body, secret string
		status                       int
	}{
		{"POST", "/deploy", `{"service":"llm","commit_sha":"` + deploySha + `","request_id":"gh-access-1"}`, "test-value", 202},
		{"POST", "/deploy", `{}`, "", 401},
		{"POST", "/deploy", `{"service":"llm","commit_sha":"bad"}`, "test-value", 422},
		{"POST", "/deploy", bigBody, "test-value", 422}, // 4096 바이트에서 읽기를 멈춘다 — 413 은 없다
		{"GET", "/deploys/gh-access-1", "", "test-value", 200},
		{"GET", "/deploys/gh-none", "", "test-value", 404},
		{"GET", "/deploys/gh-access-1/output", "", "test-value", 409},
		{"GET", "/deploys/bad.id/output", "", "test-value", 422},
		{"GET", "/deploys?limit=5", "", "test-value", 200},
		{"GET", "/status", "", "test-value", 200},
		{"GET", "/status", "", "wrong-value", 401},
		{"GET", "/deploys/git/commits?service=evil", "", "test-value", 422},
		{"GET", "/no-such-route", "", "test-value", 404},
		{"DELETE", "/status", "", "test-value", 405},
	}
	for _, testCase := range cases {
		label := testCase.method + " " + testCase.target
		if len(label) > 60 {
			label = label[:60]
		}
		recorder, captured := accessCall(t, master, testCase.method, testCase.target, testCase.body, testCase.secret, "10.0.0.1:40000",
			map[string]string{"X-Caller-Service": "front", "X-Request-Id": "trace-front-0101"})
		if recorder.Code != testCase.status {
			t.Fatalf("%s: 상태 %d (기대 %d)", label, recorder.Code, testCase.status)
		}
		lines := masterAccessLines(t, captured)
		if len(lines) != 1 {
			t.Fatalf("%s: 접속 줄은 정확히 하나: %d\n%s", label, len(lines), captured)
		}
		line := lines[0]
		if line["service"] != "ci-master" || line["caller"] != "front" || line["request_id"] != "trace-front-0101" ||
			line["status"] != float64(testCase.status) || line["method"] != testCase.method || line["loopback"] != false {
			t.Fatalf("%s: 줄의 값이 틀렸다: %v", label, line)
		}
		if strings.Contains(captured, "test-value") || strings.Contains(captured, "wrong-value") {
			t.Fatalf("%s: 비밀값 · 쿼리가 줄에 실렸다:\n%s", label, captured)
		}
		if path, _ := line["path"].(string); strings.Contains(path, "?") || strings.Contains(path, "limit") || strings.Contains(path, "service=") {
			t.Fatalf("%s: path 에 쿼리가 실렸다: %q", label, path)
		}
	}
}

// request_id = 응답 머리글 = 그 요청의 로그 줄(접수의 잇는 줄)의 ID. 지금의 로그 줄은 그대로 남는다.
func TestMasterAccessLineRequestIDMatchesLogLines(t *testing.T) {
	t.Setenv("SERVER_NAME", "ci-master")
	newFakeAgent(t)
	master := newTestMaster(t)
	for _, remoteAddress := range []string{"10.0.0.1:40000", "203.0.113.7:40000"} {
		var recorder *httptest.ResponseRecorder
		var captured string
		recorder, captured = accessCall(t, master, "POST", "/deploy", `{"service":"llm","commit_sha":"`+deploySha+`","request_id":"gh-access-2"}`, "test-value", remoteAddress,
			map[string]string{"X-Request-Id": "trace-front-0201", "X-Caller-Service": "admin"})
		waitSettled(t, master, "gh-access-2")
		lines := masterAccessLines(t, captured)
		if len(lines) != 1 {
			t.Fatalf("%s: 접속 줄 %d", remoteAddress, len(lines))
		}
		returned := recorder.Result().Header.Get(shared.RequestIDHeader)
		if lines[0]["request_id"] != returned {
			t.Fatalf("%s: request_id(%v)가 응답 머리글(%q)과 다르다", remoteAddress, lines[0]["request_id"], returned)
		}
		if remoteAddress == "203.0.113.7:40000" && (!masterTrackingShape.MatchString(returned) || lines[0]["caller"] != "outside") {
			t.Fatalf("바깥 연결: 새 32자 · outside 여야 한다: %q %v", returned, lines[0]["caller"])
		}
		if remoteAddress == "10.0.0.1:40000" && (returned != "trace-front-0201" || lines[0]["caller"] != "admin") {
			t.Fatalf("사설 연결: 받은 값: %q %v", returned, lines[0]["caller"])
		}
		job := "gh-access-2 -> 202"
		if !regexp.MustCompile(`(?m) ` + regexp.QuoteMeta(returned) + `:ci-master:deploy accept job=` + regexp.QuoteMeta(job) + `$`).MatchString(captured) {
			t.Fatalf("%s: 잇는 줄이 같은 추적 ID 로 그대로 남아야 한다:\n%s", remoteAddress, captured)
		}
	}
}

// git 중계: 에이전트로 싣는 X-Request-Id · 중계 실패 줄의 ID · 접속 줄의 request_id 가 같다.
func TestMasterAccessLineRequestIDMatchesRelayedHeaderAndRelayLog(t *testing.T) {
	t.Setenv("SERVER_NAME", "ci-master")
	var mutex sync.Mutex
	forwarded := []string{}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		mutex.Lock()
		forwarded = append(forwarded, request.Header.Get(shared.RequestIDHeader))
		mutex.Unlock()
		writer.Header().Set("Content-Type", "application/json; charset=utf-8")
		io.WriteString(writer, `not an envelope`)
	}))
	defer server.Close()
	t.Setenv("AGENT_URL_LLM", server.URL)
	master := newTestMaster(t)
	recorder, captured := accessCall(t, master, "GET", relayCommitsTarget("llm", relayFromSha, relayToSha), "", "test-value", "198.51.100.20:40000",
		map[string]string{"X-Request-Id": "trace-forged-0301", "X-Caller-Service": "admin"})
	if recorder.Code != 502 {
		t.Fatalf("중계 실패 502 여야 한다: %d", recorder.Code)
	}
	lines := masterAccessLines(t, captured)
	if len(lines) != 1 || len(forwarded) != 1 {
		t.Fatalf("줄 %d · 에이전트 호출 %d", len(lines), len(forwarded))
	}
	identifier, _ := lines[0]["request_id"].(string)
	if !masterTrackingShape.MatchString(identifier) || identifier != forwarded[0] || identifier != recorder.Result().Header.Get(shared.RequestIDHeader) {
		t.Fatalf("접속 줄 %q · 에이전트로 실은 값 %q · 응답 머리글 %q 가 같아야 한다", identifier, forwarded[0], recorder.Result().Header.Get(shared.RequestIDHeader))
	}
	if !strings.Contains(captured, " "+identifier+":ci-master:git relay failed llm /agent/git/commits: unexpected_response") {
		t.Fatalf("중계 실패 줄이 같은 ID 로:\n%s", captured)
	}
	if lines[0]["caller"] != "outside" || strings.Contains(captured, "trace-forged-0301") {
		t.Fatalf("바깥 연결의 호출자 · ID 를 믿었다: %v\n%s", lines[0]["caller"], captured)
	}
}

// 마스터 → 에이전트(T4b 의 X-Caller-Service: ci-master): 받는 쪽(에이전트 대역을 같은 감싸개로 감쌈)의 접속 줄에 caller = ci-master,
// request_id = 마스터가 실은 새 추적 ID, service = ci-agent, loopback = true(같은 호스트의 루프백 연결).
func TestAgentSideAccessLineRecordsMasterAsCaller(t *testing.T) {
	t.Setenv("SERVER_NAME", "ci-master")
	var mutex sync.Mutex
	sent := []string{}
	agentMux := http.NewServeMux()
	agentMux.HandleFunc("POST /agent/{stage}", func(writer http.ResponseWriter, request *http.Request) {
		io.Copy(io.Discard, request.Body)
		mutex.Lock()
		sent = append(sent, shared.TrackingID(request.Context()))
		mutex.Unlock()
		shared.WriteOK(writer, map[string]any{"took_ms": 1})
	})
	server := httptest.NewServer(shared.TrackRequests(shared.NewLogger(), shared.WriteAccessLines("ci-agent", agentMux)))
	defer server.Close()
	t.Setenv("AGENT_URL_LLM", server.URL)
	master := newTestMaster(t)
	captured := captureMasterLog(t, func() {
		if recorder := submit(master, "gh-access-3"); recorder.Code == 202 {
			waitSettled(t, master, "gh-access-3")
		}
	})
	var agentLines []map[string]any
	for _, line := range masterAccessLines(t, captured) {
		if line["service"] == "ci-agent" {
			agentLines = append(agentLines, line)
		}
	}
	if len(agentLines) != 2 || len(sent) != 2 {
		t.Fatalf("에이전트 쪽 줄 %d · 받은 호출 %d (기대 2 — ci · cd)\n%s", len(agentLines), len(sent), captured)
	}
	for index, line := range agentLines {
		if line["caller"] != "ci-master" || line["request_id"] != sent[index] || line["loopback"] != true || line["method"] != "POST" ||
			line["path"] != []string{"/agent/ci", "/agent/cd"}[index] {
			t.Fatalf("%d번째 에이전트 쪽 줄: %v (받은 추적 ID %q)", index, line, sent[index])
		}
	}
}
