package agent

// 실패 글의 끝부분도 가린다(agent) — 기준: requirement-spec.md(2026-11-25 admin-incident-assist) §22.
//
// 정한 계약:
//   - 단계 실패 봉투의 detail = 실패 글 전체를 가린 글(shared.CaptureOutput)에서 shared.CutTail 로 뗀 끝 2000바이트 이하.
//     로그 줄 "agent <단계> failed … : <끝>" 의 끝도 같은 가린 글의 끝 200바이트 이하. 청했는지(X-Deploy-Failure-Output)와 무관하다.
//   - 봉투의 칸은 그대로: 청하지 않으면 {code, detail}, 청하면 {code, detail, output}.
//   - 가림이 없는 글(보이는 글자 · 줄바꿈 · 탭만)은 detail 이 바이트까지 옛 꼴과 같다. 제어열 · \r 이 든 글은 정리된 꼴로 실린다.
//   - 단계 오류가 아닌 실패(<단계>_failed — execute 가 stageError 가 아닌 오류를 낸 경우)의 detail 도 같다.
//   - 가림이 실패(패닉)하면 detail · 로그의 끝은 shared.DetailRedactionFailed 이고 output 은 싣지 않는다. 응답 · 잠금 풀기는 그대로.
//   - 큰 실패 글(20MB)도 응답이 10초(-race 4배) 안.

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/junhyeong9812/study-note-deploy-system-ci-cd/internal/shared"
)

// secretTail — 끝 200바이트 안에 들어가는 비밀값들(줄마다 하나 — 각각 다른 규칙). 모두 Zq81 을 품는다.
func secretTail() (text string, secrets []string) {
	token := "gh" + "p_" + "Zq81" + strings.Repeat("T", 32)
	lines := []string{
		"using " + token,
		"password=Zq81pw",
		"Authorization: Bearer Zq81bearer",
		"https://ci:Zq81url@git.example.com/x",
		"db 10.0.0.1 mail ops@example.com",
	}
	return strings.Join(lines, "\n"), []string{token, "Zq81pw", "Zq81bearer", "Zq81url", "10.0.0.1", "ops@example.com"}
}

func requireNoSecrets(t *testing.T, where, text string, secrets []string) {
	t.Helper()
	for _, secret := range secrets {
		if strings.Contains(text, secret) {
			t.Fatalf("%s 에 가리지 않은 %q 가 있다:\n%s", where, secret, text)
		}
	}
	if strings.Contains(text, "Zq81") {
		t.Fatalf("%s 에 비밀값의 조각(Zq81)이 남았다:\n%s", where, text)
	}
	if index := strings.LastIndex(text, "[가림:"); index >= 0 && !strings.Contains(text[index:], "]") {
		t.Fatalf("%s 의 표시가 잘렸다: %q", where, text[index:])
	}
	if strings.HasPrefix(text, "가림:") || strings.Contains(text, "\n가림:") {
		t.Fatalf("%s 가 표시의 가운데에서 시작한다", where)
	}
}

func failingTest(output string) *scriptedCommands {
	return &scriptedCommands{outputs: map[string]string{"--target test": output}, failures: map[string]bool{"--target test": true}}
}

// nextLogEntry — 로그 한 건은 시각으로 시작한다(끝부분에 줄바꿈이 들어 있어 한 건이 여러 줄일 수 있다).
var nextLogEntry = regexp.MustCompile(`\n\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z `)

// agentLogLine — "agent ci failed" 로그 한 건 전체(그 뒤의 끝부분까지).
func agentLogLine(t *testing.T, logged string) string {
	t.Helper()
	start := strings.Index(logged, "agent ci failed")
	if start < 0 {
		t.Fatalf("agent 실패 로그 줄이 없다:\n%s", logged)
	}
	entry := logged[start:]
	if next := nextLogEntry.FindStringIndex(entry); next != nil {
		entry = entry[:next[0]]
	}
	return strings.TrimSuffix(entry, "\n")
}

func TestStageFailureDetailAndLogAreMasked(t *testing.T) {
	tailText, secrets := secretTail()
	output := strings.Repeat("collected line ok\n", 300) + tailText
	for _, outputRequest := range []string{"", "v1"} {
		agent := newScriptedAgent(t, failingTest(output))
		var recorder *httptest.ResponseRecorder
		logged := captureStandardOutput(t, func() { recorder = postAsking(agent, "/agent/ci", outputRequest) })
		envelope := decodeStageEnvelope(t, recorder.Body.Bytes())
		if recorder.Code != 500 || envelope.Error.Code != "ci_test" || len(envelope.Error.Detail) > 2000 || len(envelope.Error.Detail) < 1900 {
			t.Fatalf("머리 %q: 500 ci_test · detail 2000바이트 이하: %d %s %d", outputRequest, recorder.Code, envelope.Error.Code, len(envelope.Error.Detail))
		}
		requireNoSecrets(t, "detail(머리 "+outputRequest+")", envelope.Error.Detail, secrets)
		for _, kind := range []string{"[가림:token]", "[가림:secret_field]", "[가림:authorization]", "[가림:url_credential]", "[가림:private_ip]", "[가림:email]"} {
			if !strings.Contains(envelope.Error.Detail, kind) {
				t.Fatalf("detail 에 %s 가 없다:\n%s", kind, envelope.Error.Detail)
			}
		}
		if !strings.HasSuffix(envelope.Error.Detail, "[가림:email]\nexit status 1") {
			t.Fatalf("detail 의 끝은 실패 글의 끝이다: %q", envelope.Error.Detail[max(0, len(envelope.Error.Detail)-60):])
		}
		line := agentLogLine(t, logged)
		requireNoSecrets(t, "agent 로그 줄", line, secrets)
		if !strings.Contains(line, "agent ci failed llm stage=ci_test ") || !strings.HasSuffix(line, "exit status 1") {
			t.Fatalf("로그 줄의 앞머리 · 끝이 바뀌었다: %q", line)
		}
		var raw map[string]map[string]json.RawMessage
		json.Unmarshal(recorder.Body.Bytes(), &raw)
		keys := []string{}
		for key := range raw["error"] {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		if want := map[string]string{"": "code,detail", "v1": "code,detail,output"}[outputRequest]; strings.Join(keys, ",") != want {
			t.Fatalf("머리 %q: 실패 봉투의 칸이 바뀌었다: %v", outputRequest, keys)
		}
	}
}

func TestSecretAcrossTheDetailBoundaryLeavesNoHalf(t *testing.T) {
	token := "gh" + "p_" + "Zq81" + strings.Repeat("K", 32) // 40바이트
	for _, limitFromEnd := range []int{2000, 200} {         // detail 의 끝 2000 · 로그의 끝 200
		suffix := "\nexit status 1"
		for shift := 1; shift < len(token); shift += 3 { // 끝에서 limitFromEnd 바이트 자리가 토큰 가운데에 떨어지게
			after := strings.Repeat("z", limitFromEnd-shift-len(suffix)-1)
			output := strings.Repeat("y ", 1500) + token + " " + after
			agent := newScriptedAgent(t, failingTest(output))
			var recorder *httptest.ResponseRecorder
			logged := captureStandardOutput(t, func() { recorder = postAsking(agent, "/agent/ci", "") })
			envelope := decodeStageEnvelope(t, recorder.Body.Bytes())
			requireNoSecrets(t, "detail", envelope.Error.Detail, []string{token[:12], token[len(token)-12:]})
			requireNoSecrets(t, "로그 줄", agentLogLine(t, logged), []string{token[:12], token[len(token)-12:]})
			if strings.Contains(envelope.Error.Detail, "KKKKKKKK") || strings.Contains(agentLogLine(t, logged), "KKKKKKKK") {
				t.Fatalf("끝 %d 의 경계에 걸친 토큰의 반쪽이 남았다(shift %d)", limitFromEnd, shift)
			}
		}
	}
}

func TestDetailThatIsOnePemBodyIsMasked(t *testing.T) {
	var body strings.Builder
	for line := range 80 {
		body.WriteString("Zq81" + strings.Repeat(string(rune('A'+line%26)), 60) + "\n")
	}
	output := "loading key\n-----BEGIN " + "RSA PRIVATE KEY-----\n" + body.String() + "-----END " + "RSA PRIVATE KEY-----"
	agent := newScriptedAgent(t, failingTest(output))
	envelope := decodeStageEnvelope(t, postAsking(agent, "/agent/ci", "").Body.Bytes())
	requireNoSecrets(t, "detail", envelope.Error.Detail, nil)
	if !strings.Contains(envelope.Error.Detail, "[가림:pem]") || !strings.HasSuffix(envelope.Error.Detail, "exit status 1") {
		t.Fatalf("끝부분이 통째로 PEM 이면 표시만 남는다: %q", envelope.Error.Detail)
	}
}

func TestPlainDetailIsByteForByteTheSame(t *testing.T) {
	output := strings.Repeat("--- FAIL: TestQueue (0.01s)\n    queue_test.go:42: expected 3, got 2\n", 60) + "FAIL\tgithub.com/example/app\t0.123s"
	agent := newScriptedAgent(t, failingTest(output))
	var recorder *httptest.ResponseRecorder
	logged := captureStandardOutput(t, func() { recorder = postAsking(agent, "/agent/ci", "") })
	failureText := output + "\nexit status 1"
	if want := `{"error":{"code":"ci_test","detail":` + jsonString(failureText[len(failureText)-2000:]) + `},"success":false}` + "\n"; recorder.Body.String() != want {
		t.Fatalf("가릴 것이 없는 실패 봉투가 옛 꼴과 다르다:\n got %q\nwant %q", recorder.Body.String(), want)
	}
	if line := agentLogLine(t, logged); !strings.HasSuffix(line, ": "+failureText[len(failureText)-200:]) {
		t.Fatalf("가릴 것이 없는 로그 줄의 끝이 옛 꼴과 다르다: %q", line)
	}
}

func jsonString(value string) string {
	var builder strings.Builder
	encoder := json.NewEncoder(&builder)
	encoder.Encode(value)
	return strings.TrimSuffix(builder.String(), "\n")
}

func TestDetailWithControlSequencesIsCleaned(t *testing.T) {
	agent := newScriptedAgent(t, failingTest("\x1b[31mFAIL\x1b[0m: TestQueue\r\nprogress 10%\rdone\x00"))
	envelope := decodeStageEnvelope(t, postAsking(agent, "/agent/ci", "").Body.Bytes())
	if envelope.Error.Detail != "FAIL: TestQueue\nprogress 10%\ndone�\nexit status 1" {
		t.Fatalf("제어열 · \\r 이 든 실패 글은 정리된 꼴로 실린다: %q", envelope.Error.Detail)
	}
}

func TestNonStageFailureDetailIsMasked(t *testing.T) {
	agent := newScriptedAgent(t, &scriptedCommands{})
	mux := http.NewServeMux()
	mux.HandleFunc("POST /agent/x", agent.handleStage("ci", func(string, string) (string, error) {
		return "prepared\npassword=Zq81plain", errors.New("boom")
	}))
	request := httptest.NewRequest("POST", "/agent/x", strings.NewReader(`{"service":"llm","commit_sha":"`+requestSha+`","request_id":"gh-1"}`))
	request.Header.Set(shared.SecretHeader, "test-value")
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, request)
	envelope := decodeStageEnvelope(t, recorder.Body.Bytes())
	if envelope.Error.Code != "ci_failed" || envelope.Error.Detail != "prepared\npassword=[가림:secret_field]" {
		t.Fatalf("단계 오류가 아닌 실패의 detail 도 가린다: %s %q", envelope.Error.Code, envelope.Error.Detail)
	}
}

func TestRedactionPanicLeavesTheStageAnswer(t *testing.T) {
	tailText, secrets := secretTail()
	for _, outputRequest := range []string{"", "v1"} {
		agent := newScriptedAgent(t, failingTest(tailText))
		agent.redact = func(string) shared.CapturedOutput { panic("redaction broke") }
		var recorder *httptest.ResponseRecorder
		logged := captureStandardOutput(t, func() { recorder = postAsking(agent, "/agent/ci", outputRequest) })
		envelope := decodeStageEnvelope(t, recorder.Body.Bytes())
		if recorder.Code != 500 || envelope.Error.Code != "ci_test" || envelope.Error.Detail != shared.DetailRedactionFailed || len(envelope.Error.Output) != 0 {
			t.Fatalf("머리 %q: 가림이 실패해도 단계는 그대로, detail 은 고정 글, output 은 없다: %d %s", outputRequest, recorder.Code, recorder.Body.String())
		}
		line := agentLogLine(t, logged)
		requireNoSecrets(t, "로그 줄", line, secrets)
		if !strings.HasSuffix(line, shared.DetailRedactionFailed) {
			t.Fatalf("로그 줄의 끝은 고정 글: %q", line)
		}
		if !agent.busy.TryLock() {
			t.Fatal("가림이 실패한 뒤 단계 잠금이 풀리지 않았다")
		}
		agent.busy.Unlock()
	}
}

func TestLargeFailureTextIsMaskedWithinTimeLimit(t *testing.T) {
	tailText, secrets := secretTail()
	output := strings.Repeat("compiling module ok step 1234\n", (20<<20)/30) + tailText // 20MB
	agent := newScriptedAgent(t, failingTest(output))
	done := make(chan *httptest.ResponseRecorder, 1)
	started := time.Now()
	go func() { done <- postAsking(agent, "/agent/ci", "") }()
	select {
	case recorder := <-done:
		envelope := decodeStageEnvelope(t, recorder.Body.Bytes())
		requireNoSecrets(t, "20MB 의 detail", envelope.Error.Detail, secrets)
		if !strings.HasSuffix(envelope.Error.Detail, "exit status 1") {
			t.Fatalf("detail 의 끝이 다르다: %q", envelope.Error.Detail[max(0, len(envelope.Error.Detail)-40):])
		}
		t.Logf("20MB 실패 글: %v", time.Since(started))
	case <-time.After(10 * time.Second * timeBudgetFactor):
		t.Fatal("20MB 실패 글의 끝부분을 10초 안에 만들지 못했다")
	}
}
