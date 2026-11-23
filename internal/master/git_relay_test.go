package master

// git 조회 중계 계약 테스트 — 기준: docs/cross-cutting/plans/2026-11-22/admin-skeleton/requirement-spec.md §12(12.7 우선).
// 구현을 보지 않고 명세에서 썼다(blind). agent 는 httptest 가짜 서버로 둔다.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/junhyeong9812/study-note-deploy-system-ci-cd/internal/shared"
)

const (
	relayFromSha = "1111111111111111111111111111111111111111"
	relayToSha   = "2222222222222222222222222222222222222222"
)

type receivedGitCall struct {
	method string
	path   string
	query  url.Values
	secret string
	body   string
}

// fakeGitAgent — /agent/git/* 호출을 기록하고 정해 둔 상태·본문으로 답한다.
type fakeGitAgent struct {
	mutex    sync.Mutex
	received []receivedGitCall
	status   int    // 0이면 200
	body     string // 비면 정상 봉투
	hold     chan struct{}
	server   *httptest.Server
}

// newFakeGitAgent — AGENT_URL_<서비스> 를 이 가짜 agent 로 둔다. newTestMaster 보다 먼저 불러야 한다.
func newFakeGitAgent(t *testing.T, service string) *fakeGitAgent {
	t.Helper()
	agent := &fakeGitAgent{}
	agent.server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, httpRequest *http.Request) {
		requestBody, _ := io.ReadAll(io.LimitReader(httpRequest.Body, 1024))
		agent.mutex.Lock()
		agent.received = append(agent.received, receivedGitCall{method: httpRequest.Method, path: httpRequest.URL.Path,
			query: httpRequest.URL.Query(), secret: httpRequest.Header.Get(shared.SecretHeader), body: string(requestBody)})
		status, body, hold := agent.status, agent.body, agent.hold
		agent.mutex.Unlock()
		if hold != nil {
			<-hold
		}
		if status == 0 {
			status = http.StatusOK
		}
		if body == "" {
			body = `{"success":true,"data":{"commits":[],"truncated":false,"range_complete":true,"relation":"ancestor"}}`
		}
		writer.Header().Set("Content-Type", "application/json; charset=utf-8")
		writer.WriteHeader(status)
		writer.Write([]byte(body))
	}))
	t.Cleanup(agent.server.Close)
	t.Setenv("AGENT_URL_"+strings.ToUpper(service), agent.server.URL)
	return agent
}

func (agent *fakeGitAgent) calls() []receivedGitCall {
	agent.mutex.Lock()
	defer agent.mutex.Unlock()
	return append([]receivedGitCall(nil), agent.received...)
}

func (agent *fakeGitAgent) respond(status int, body string) {
	agent.mutex.Lock()
	defer agent.mutex.Unlock()
	agent.status, agent.body = status, body
}

func relayCall(master *Master, method, target, secret string) *httptest.ResponseRecorder {
	mux := http.NewServeMux()
	master.Register(mux)
	httpRequest := httptest.NewRequest(method, target, nil)
	if secret != "" {
		httpRequest.Header.Set(shared.SecretHeader, secret)
	}
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, httpRequest)
	return recorder
}

func relayGet(master *Master, target string) *httptest.ResponseRecorder {
	return relayCall(master, http.MethodGet, target, "test-value")
}

func relayCommitsTarget(service, from, to string) string {
	return "/deploys/git/commits?" + url.Values{"service": {service}, "from": {from}, "to": {to}}.Encode()
}

func relayCommitTarget(service, sha string) string {
	return "/deploys/git/commit?" + url.Values{"service": {service}, "sha": {sha}}.Encode()
}

func decodeJSON(t *testing.T, body []byte) any {
	t.Helper()
	var decoded any
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("JSON 이 아니다: %v %.300s", err, body)
	}
	return decoded
}

func requireRelayFailure(t *testing.T, recorder *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	var envelope struct {
		Success bool `json:"success"`
		Error   struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil || recorder.Code != status || envelope.Success || envelope.Error.Code != code {
		t.Fatalf("%d %s 이어야 한다: %d %.300s", status, code, recorder.Code, recorder.Body.String())
	}
}

func TestGitRelayForwardsCommitsQueryAsGetWithSecret(t *testing.T) {
	agent := newFakeGitAgent(t, "llm")
	agentBody := `{"success":true,"data":{"commits":[{"sha":"` + relayToSha + `","author_name":"이름","authored_at":"2026-01-02T03:04:05Z","subject":"제목","body":"본문 <b>","body_truncated":false}],"truncated":false,"range_complete":false,"relation":"ancestor"}}`
	agent.respond(200, agentBody)
	master := newTestMaster(t)
	recorder := relayGet(master, relayCommitsTarget("llm", relayFromSha, relayToSha[:7]))
	if recorder.Code != 200 || !reflect.DeepEqual(decodeJSON(t, recorder.Body.Bytes()), decodeJSON(t, []byte(agentBody))) {
		t.Fatalf("agent 의 봉투를 그대로 돌려줘야 한다: %d %.500s", recorder.Code, recorder.Body.String())
	}
	calls := agent.calls()
	if len(calls) != 1 {
		t.Fatalf("agent 를 한 번 불러야 한다: %d", len(calls))
	}
	call := calls[0]
	wantQuery := url.Values{"service": {"llm"}, "from": {relayFromSha}, "to": {relayToSha[:7]}}
	if call.method != http.MethodGet || call.path != "/agent/git/commits" || call.secret != "test-value" ||
		!reflect.DeepEqual(call.query, wantQuery) || call.body != "" {
		t.Fatalf("agent 에 보낸 요청이 다르다: %+v", call)
	}
}

func TestGitRelayForwardsCommitQueryAsGetWithSecret(t *testing.T) {
	agent := newFakeGitAgent(t, "llm")
	agentBody := `{"success":true,"data":{"sha":"` + relayToSha + `","author_name":"a","authored_at":"2026-01-02T03:04:05Z","subject":"s","body":"","body_truncated":false,"files":[{"path":"탭\t이름","previous_path":null,"status":"added","additions":1,"deletions":0,"binary":false,"patch":"@@ -0,0 +1 @@\n+x","patch_truncated":false}],"files_truncated":false}}`
	agent.respond(200, agentBody)
	master := newTestMaster(t)
	recorder := relayGet(master, relayCommitTarget("llm", relayToSha))
	if recorder.Code != 200 || !reflect.DeepEqual(decodeJSON(t, recorder.Body.Bytes()), decodeJSON(t, []byte(agentBody))) {
		t.Fatalf("agent 의 봉투를 그대로 돌려줘야 한다: %d %.500s", recorder.Code, recorder.Body.String())
	}
	calls := agent.calls()
	if len(calls) != 1 || calls[0].method != http.MethodGet || calls[0].path != "/agent/git/commit" || calls[0].secret != "test-value" ||
		!reflect.DeepEqual(calls[0].query, url.Values{"service": {"llm"}, "sha": {relayToSha}}) {
		t.Fatalf("agent 에 보낸 요청이 다르다: %+v", calls)
	}
}

func TestGitRelayRequiresSecretBeforeCheckingInput(t *testing.T) {
	agent := newFakeGitAgent(t, "llm")
	master := newTestMaster(t)
	for _, target := range []string{
		relayCommitsTarget("llm", relayFromSha, relayToSha), relayCommitTarget("llm", relayToSha),
		relayCommitsTarget("evil", "HEAD", "a..b"), "/deploys/git/commit", "/deploys/git/commits",
	} {
		for _, secret := range []string{"", "wrong-value"} {
			requireRelayFailure(t, relayCall(master, http.MethodGet, target, secret), 401, "unauthorized")
		}
	}
	if calls := agent.calls(); len(calls) != 0 {
		t.Fatalf("인증 전에 agent 를 불렀다: %+v", calls)
	}
}

func TestGitRelayRejectsInvalidInputWithoutCallingAgent(t *testing.T) {
	agent := newFakeGitAgent(t, "llm")
	t.Setenv("AGENT_URL_FRONT", "") // 허용 목록에는 있지만 agent 설정이 없는 서비스
	master := newTestMaster(t)
	commits := func(pairs ...string) string { return "/deploys/git/commits?" + rawQuery(pairs...) }
	commit := func(pairs ...string) string { return "/deploys/git/commit?" + rawQuery(pairs...) }
	cases := map[string]string{
		"commits 허용 목록 밖":     commits("service", "evil", "from", relayFromSha, "to", relayToSha),
		"commits agent 설정 없음": commits("service", "front", "from", relayFromSha, "to", relayToSha),
		"commits 옵션 꼴":        commits("service", "llm", "from", "--output=/tmp/x", "to", relayToSha),
		"commits HEAD":        commits("service", "llm", "from", "HEAD", "to", relayToSha),
		"commits 범위 글자":       commits("service", "llm", "from", relayFromSha, "to", "1111111..2222222"),
		"commits 대문자":         commits("service", "llm", "from", "ABCDEF1234", "to", relayToSha),
		"commits 6자":          commits("service", "llm", "from", "abcdef", "to", relayToSha),
		"commits 빈 값":         commits("service", "llm", "from", "", "to", relayToSha),
		"commits to 없음":       commits("service", "llm", "from", relayFromSha),
		"commits service 없음":  commits("from", relayFromSha, "to", relayToSha),
		"commits from 중복":     commits("service", "llm", "from", relayFromSha, "from", relayFromSha, "to", relayToSha),
		"commits service 중복":  commits("service", "llm", "service", "llm", "from", relayFromSha, "to", relayToSha),
		"commit 허용 목록 밖":      commit("service", "evil", "sha", relayToSha),
		"commit agent 설정 없음":  commit("service", "front", "sha", relayToSha),
		"commit 옵션 꼴":         commit("service", "llm", "sha", "--output=/tmp/x"),
		"commit HEAD":         commit("service", "llm", "sha", "HEAD"),
		"commit 대문자":          commit("service", "llm", "sha", "ABCDEF1234567"),
		"commit 빈 값":          commit("service", "llm", "sha", ""),
		"commit sha 없음":       commit("service", "llm"),
		"commit sha 중복":       commit("service", "llm", "sha", relayToSha, "sha", relayToSha),
		"commit 빈 쿼리":         "/deploys/git/commit",
	}
	for name, target := range cases {
		recorder := relayGet(master, target)
		if recorder.Code != 422 || !strings.Contains(recorder.Body.String(), `"invalid_request"`) {
			t.Fatalf("%s: 422 invalid_request 여야 한다: %d %.300s", name, recorder.Code, recorder.Body.String())
		}
	}
	if calls := agent.calls(); len(calls) != 0 {
		t.Fatalf("틀린 입력인데 agent 를 불렀다: %+v", calls)
	}
}

// rawQuery — 순서·중복을 그대로 둔 쿼리 글자.
func rawQuery(pairs ...string) string {
	var parts []string
	for index := 0; index+1 < len(pairs); index += 2 {
		parts = append(parts, url.QueryEscape(pairs[index])+"="+url.QueryEscape(pairs[index+1]))
	}
	return strings.Join(parts, "&")
}

func TestGitRelayPassesAgentFailuresThroughUnchanged(t *testing.T) {
	agent := newFakeGitAgent(t, "llm")
	master := newTestMaster(t)
	for _, failure := range []struct {
		status int
		code   string
	}{
		{404, "commit_not_found"}, {404, "parent_unavailable"}, {429, "too_many_requests"},
		{504, "git_timeout"}, {500, "git_failed"},
	} {
		agent.respond(failure.status, `{"success":false,"error":{"code":"`+failure.code+`"}}`)
		for _, target := range []string{relayCommitsTarget("llm", relayFromSha, relayToSha), relayCommitTarget("llm", relayToSha)} {
			requireRelayFailure(t, relayGet(master, target), failure.status, failure.code)
		}
	}
}

func TestGitRelayReportsUnreachableAgent(t *testing.T) {
	t.Setenv("AGENT_URL_LLM", "http://127.0.0.1:1") // 닫힌 포트
	master := newTestMaster(t)
	requireRelayFailure(t, relayGet(master, relayCommitsTarget("llm", relayFromSha, relayToSha)), 502, "agent_unreachable")
	requireRelayFailure(t, relayGet(master, relayCommitTarget("llm", relayToSha)), 502, "agent_unreachable")
}

func TestGitRelayTimesOutSlowAgent(t *testing.T) {
	agent := newFakeGitAgent(t, "llm")
	release := make(chan struct{})
	t.Cleanup(func() { close(release) }) // 서버를 닫기 전에 붙잡은 답을 푼다(Cleanup 은 뒤에서부터)
	agent.hold = release
	master := newTestMaster(t)
	master.gitRelayTimeout = 200 * time.Millisecond
	for _, target := range []string{relayCommitsTarget("llm", relayFromSha, relayToSha), relayCommitTarget("llm", relayToSha)} {
		startedAt := time.Now()
		recorder := relayGet(master, target)
		if elapsed := time.Since(startedAt); elapsed > 3*time.Second {
			t.Fatalf("기한(200ms)이 지나고도 %v 걸렸다", elapsed)
		}
		requireRelayFailure(t, recorder, 504, "agent_timeout")
	}
}

func TestGitRelayDefaultTimeoutIsFourSeconds(t *testing.T) {
	if master := newTestMaster(t); master.gitRelayTimeout != 4*time.Second {
		t.Fatalf("중계 기한 기본값은 4초여야 한다: %v", master.gitRelayTimeout)
	}
}

func TestGitRelayRejectsNonEnvelopeResponses(t *testing.T) {
	agent := newFakeGitAgent(t, "llm")
	master := newTestMaster(t)
	for name, response := range map[string]struct {
		status int
		body   string
	}{
		"200 글자":          {200, "hello"},
		"200 코드 없는 실패 봉투": {200, `{"success":false}`},
		"500 HTML":        {500, "<html>oops</html>"},
		"404 코드 없음":       {404, `{"success":false,"error":{}}`},
	} {
		agent.respond(response.status, response.body)
		recorder := relayGet(master, relayCommitTarget("llm", relayToSha))
		if recorder.Code != 502 || !strings.Contains(recorder.Body.String(), `"unexpected_response"`) {
			t.Fatalf("%s: 502 unexpected_response 여야 한다: %d %.300s", name, recorder.Code, recorder.Body.String())
		}
	}
}

func TestGitRelayPassesLargeResponseWhole(t *testing.T) {
	agent := newFakeGitAgent(t, "llm")
	master := newTestMaster(t)
	agentBody := `{"success":true,"data":{"sha":"` + relayToSha + `","author_name":"a","authored_at":"2026-01-02T03:04:05Z","subject":"s","body":"` +
		strings.Repeat("p", 1<<20) + `","body_truncated":false,"files":[],"files_truncated":false}}`
	agent.respond(200, agentBody)
	recorder := relayGet(master, relayCommitTarget("llm", relayToSha))
	if recorder.Code != 200 || !reflect.DeepEqual(decodeJSON(t, recorder.Body.Bytes()), decodeJSON(t, []byte(agentBody))) {
		t.Fatalf("1MB 응답이 잘리지 않고 전달돼야 한다(16KB 상한은 배포 호출에만): %d %d바이트", recorder.Code, recorder.Body.Len())
	}
}

func TestGitRelayRejectsResponseOverFourMegabytes(t *testing.T) {
	agent := newFakeGitAgent(t, "llm")
	master := newTestMaster(t)
	agent.respond(200, `{"success":true,"data":{"body":"`+strings.Repeat("p", 5<<20)+`"}}`)
	recorder := relayGet(master, relayCommitTarget("llm", relayToSha))
	if recorder.Body.Len() > 1<<20 {
		t.Fatalf("4MB 를 넘는 응답을 그대로 흘렸다: %d바이트", recorder.Body.Len())
	}
	requireRelayFailure(t, recorder, 502, "unexpected_response")
}

func TestGitRelayRejectsNonGetMethods(t *testing.T) {
	agent := newFakeGitAgent(t, "llm")
	master := newTestMaster(t)
	for _, target := range []string{relayCommitsTarget("llm", relayFromSha, relayToSha), relayCommitTarget("llm", relayToSha)} {
		for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
			if recorder := relayCall(master, method, target, "test-value"); recorder.Code != http.StatusMethodNotAllowed {
				t.Fatalf("%s %s: 405 여야 한다: %d", method, target, recorder.Code)
			}
		}
	}
	if calls := agent.calls(); len(calls) != 0 {
		t.Fatalf("GET 이 아닌데 agent 를 불렀다: %+v", calls)
	}
}

func TestGitRelayAcceptsAdminService(t *testing.T) {
	agent := newFakeGitAgent(t, "admin")
	master := newTestMaster(t)
	if master.agents["admin"] != agent.server.URL {
		t.Fatalf("AGENT_URL_ADMIN 을 허용 목록으로 읽지 않았다: %v", master.agents)
	}
	if recorder := relayGet(master, relayCommitsTarget("admin", relayFromSha, relayToSha)); recorder.Code != 200 {
		t.Fatalf("admin 조회가 중계되지 않았다: %d %.300s", recorder.Code, recorder.Body.String())
	}
	if calls := agent.calls(); len(calls) != 1 || calls[0].query.Get("service") != "admin" {
		t.Fatalf("admin agent 를 부르지 않았다: %+v", calls)
	}
	if recorder := request(master, true, `{"service":"admin","commit_sha":"abc1234def","request_id":"gh-admin-1"}`); recorder.Code != 202 {
		t.Fatalf("admin 배포 접수가 거절됐다: %d %s", recorder.Code, recorder.Body.String())
	}
	waitFinished(t, master, "gh-admin-1") // 뒤에서 도는 진행이 끝난 뒤에 정리한다
}

// agent 의 401 은 master 와 agent 의 비밀값 불일치(설정 실수) — 어드민의 비밀값이 틀린 401 과 구분해 502 agent_unauthorized.
// 기준: requirement-spec.md §12.8 오류 사상 표의 예외.
func TestGitRelayReportsAgentUnauthorizedAsBadGateway(t *testing.T) {
	agent := newFakeGitAgent(t, "llm")
	agent.respond(401, `{"success":false,"error":{"code":"unauthorized"}}`)
	master := newTestMaster(t)
	for _, target := range []string{relayCommitsTarget("llm", relayFromSha, relayToSha), relayCommitTarget("llm", relayToSha)} {
		requireRelayFailure(t, relayGet(master, target), 502, "agent_unauthorized")
	}
}

// 200 이어도 data 가 그 경로의 필수 칸을 갖추지 않으면 502 unexpected_response — 기준: requirement-spec.md §12.9.
func TestGitRelayRejectsSuccessEnvelopeWithoutRequiredFields(t *testing.T) {
	agent := newFakeGitAgent(t, "llm")
	master := newTestMaster(t)
	commitsData := `{"commits":[],"truncated":false,"range_complete":true,"relation":"ancestor"}`
	commitData := `{"sha":"` + relayToSha + `","author_name":"a","authored_at":"2026-01-02T03:04:05Z","subject":"s","body":"","body_truncated":false,"files":[],"files_truncated":false}`
	commits := relayCommitsTarget("llm", relayFromSha, relayToSha)
	commit := relayCommitTarget("llm", relayToSha)
	for _, broken := range []struct{ name, target, body string }{
		{"목록 data 없음", commits, `{"success":true}`},
		{"목록 data 빈 객체", commits, `{"success":true,"data":{}}`},
		{"목록 data 배열", commits, `{"success":true,"data":[]}`},
		{"목록 commits 가 배열 아님", commits, `{"success":true,"data":{"commits":{},"truncated":false,"range_complete":true,"relation":"ancestor"}}`},
		{"목록 relation 없음", commits, `{"success":true,"data":{"commits":[],"truncated":false,"range_complete":true}}`},
		{"목록 relation 이 글자 아님", commits, `{"success":true,"data":{"commits":[],"truncated":false,"range_complete":true,"relation":1}}`},
		{"목록 truncated 없음", commits, `{"success":true,"data":{"commits":[],"range_complete":true,"relation":"ancestor"}}`},
		{"목록 range_complete 가 참거짓 아님", commits, `{"success":true,"data":{"commits":[],"truncated":false,"range_complete":"yes","relation":"ancestor"}}`},
		{"상세 data 없음", commit, `{"success":true}`},
		{"상세 data 빈 객체", commit, `{"success":true,"data":{}}`},
		{"상세 data 배열", commit, `{"success":true,"data":[]}`},
		{"상세 sha 없음", commit, `{"success":true,"data":{"files":[],"files_truncated":false}}`},
		{"상세 files 가 배열 아님", commit, `{"success":true,"data":{"sha":"` + relayToSha + `","files":"x","files_truncated":false}}`},
		{"상세 files_truncated 없음", commit, `{"success":true,"data":{"sha":"` + relayToSha + `","files":[]}}`},
		{"목록 경로에 상세 data", commits, `{"success":true,"data":` + commitData + `}`},
	} {
		agent.respond(200, broken.body)
		recorder := relayGet(master, broken.target)
		if recorder.Code != 502 || !strings.Contains(recorder.Body.String(), `"unexpected_response"`) {
			t.Fatalf("%s: 502 unexpected_response 여야 한다: %d %.300s", broken.name, recorder.Code, recorder.Body.String())
		}
	}
	for target, data := range map[string]string{commits: commitsData, commit: commitData} {
		body := `{"success":true,"data":` + data + `}`
		agent.respond(200, body)
		recorder := relayGet(master, target)
		if recorder.Code != 200 || !reflect.DeepEqual(decodeJSON(t, recorder.Body.Bytes()), decodeJSON(t, []byte(body))) {
			t.Fatalf("온전한 응답은 그대로 200 이어야 한다: %d %.300s", recorder.Code, recorder.Body.String())
		}
	}
}

// agent 의 503 repository_changing(§12.9)과 422 invalid_request 는 그대로 전달한다.
func TestGitRelayPassesRepositoryChangingAndInvalidRequestThrough(t *testing.T) {
	agent := newFakeGitAgent(t, "llm")
	master := newTestMaster(t)
	for _, failure := range []struct {
		status int
		code   string
	}{{503, "repository_changing"}, {422, "invalid_request"}} {
		agent.respond(failure.status, `{"success":false,"error":{"code":"`+failure.code+`"}}`)
		for _, target := range []string{relayCommitsTarget("llm", relayFromSha, relayToSha), relayCommitTarget("llm", relayToSha)} {
			requireRelayFailure(t, relayGet(master, target), failure.status, failure.code)
		}
	}
}

// master 의 4MB 읽기 상한을 필수 칸을 갖춘 응답으로 고정 — 기준: requirement-spec.md §12.10.
// 상한 바로 아래는 본문 바이트 그대로 200, 위는 502 unexpected_response. 두 경로 모두.
func TestGitRelayFourMegabyteLimitWithCompleteEnvelopes(t *testing.T) {
	agent := newFakeGitAgent(t, "llm")
	master := newTestMaster(t)
	envelopeOfSize := func(prefix, suffix string, size int) string {
		return prefix + strings.Repeat("p", size-len(prefix)-len(suffix)) + suffix
	}
	routes := []struct {
		name, target, prefix, suffix string
	}{
		{"상세", relayCommitTarget("llm", relayToSha),
			`{"success":true,"data":{"sha":"` + relayToSha + `","author_name":"a","authored_at":"2026-01-02T03:04:05Z","subject":"s","body":"`,
			`","body_truncated":false,"files":[],"files_truncated":false}}`},
		{"목록", relayCommitsTarget("llm", relayFromSha, relayToSha),
			`{"success":true,"data":{"commits":[{"sha":"` + relayToSha + `","author_name":"a","authored_at":"2026-01-02T03:04:05Z","subject":"s","body":"`,
			`","body_truncated":false}],"truncated":false,"range_complete":true,"relation":"ancestor"}}`},
	}
	for _, route := range routes {
		under := envelopeOfSize(route.prefix, route.suffix, 4<<20-1<<10)
		agent.respond(200, under)
		recorder := relayGet(master, route.target)
		if recorder.Code != 200 || recorder.Body.String() != under {
			t.Fatalf("%s: 상한 바로 아래(4MB-1KB)는 본문 바이트 그대로 200 이어야 한다: %d %d바이트(보낸 %d)", route.name, recorder.Code, recorder.Body.Len(), len(under))
		}
		over := envelopeOfSize(route.prefix, route.suffix, 4<<20+1<<10)
		agent.respond(200, over)
		recorder = relayGet(master, route.target)
		if recorder.Body.Len() > 1<<20 {
			t.Fatalf("%s: 4MB 를 넘는 응답을 흘렸다: %d바이트", route.name, recorder.Body.Len())
		}
		requireRelayFailure(t, recorder, 502, "unexpected_response")
	}
}
