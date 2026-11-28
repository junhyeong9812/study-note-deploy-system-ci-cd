package agent

// 단계 실패 봉투의 전체 출력 — 기준: requirement-spec.md(2026-11-25 admin-incident-assist) §15.
// 정한 계약: master 가 청할 때만(요청 머리 `X-Deploy-Failure-Output: v1` — 값이 정확히 v1 일 때만) 단계 실패 봉투의 error 에 output 이 더해진다 —
//   {"code":"ci_test","detail":"<끝 2000바이트 — 그대로>","output":{"text":"…","truncated":false,"omitted_bytes":0,"redactions":{…}}}
//   output 은 detail 이 끝부분인 그 실패 글 전체를 shared.CaptureOutput 으로 가리고 줄인 것이다(보내기 전에 가린다 · 256KB 이하).
//   머리가 없거나(옛 master) 값이 v1 이 아니면 실패 봉투는 지금과 바이트까지 같다(옛 master 의 16KB 읽기에 걸리지 않는다).
//   성공 봉투 · 단계에 들기 전의 거절(401 · 422 · 409)에는 output 이 없다.

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/junhyeong9812/study-note-deploy-system-ci-cd/internal/shared"
)

func agentFakeToken(seed int64) string {
	generator := rand.New(rand.NewSource(seed))
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	body := make([]byte, 36)
	for index := range body {
		body[index] = alphabet[generator.Intn(len(alphabet))]
	}
	return "gh" + "p_" + string(body)
}

// postAsking — 실패 출력을 청하는 master 의 요청(머리 값 그대로 — 빈 값이면 머리를 싣지 않는다).
func postAsking(agent *Agent, path, outputRequest string) *httptest.ResponseRecorder {
	mux := http.NewServeMux()
	agent.Register(mux)
	req := httptest.NewRequest("POST", path, strings.NewReader(`{"service":"llm","commit_sha":"`+requestSha+`","request_id":"gh-1"}`))
	req.Header.Set(shared.SecretHeader, "test-value")
	if outputRequest != "" {
		req.Header.Set("X-Deploy-Failure-Output", outputRequest)
	}
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, req)
	return recorder
}

func decodeStageOutput(t *testing.T, envelope stageEnvelope) shared.CapturedOutput {
	t.Helper()
	if len(envelope.Error.Output) == 0 {
		t.Fatalf("실패 봉투에 output 이 없다: %+v", envelope.Error)
	}
	var output shared.CapturedOutput
	if err := json.Unmarshal(envelope.Error.Output, &output); err != nil {
		t.Fatalf("output 의 꼴이 다르다: %v %s", err, envelope.Error.Output)
	}
	return output
}

func TestStageFailureCarriesRedactedFullOutput(t *testing.T) {
	token := agentFakeToken(1)
	password := "pw" + strings.Repeat("Q7x", 6)
	testOutput := "#9 [test 3/4] RUN pytest\n" +
		"using " + token + " to fetch fixtures\n" +
		"DB_PASSWORD=" + password + "\n" +
		strings.Repeat("collected line ok\n", 400) + // detail(끝 2000바이트) 밖으로 밀어낸다
		"FAILED tests/test_queue.py::test_order - assert 1 == 2\n"
	agent := newScriptedAgent(t, &scriptedCommands{
		outputs:  map[string]string{"--target test": testOutput},
		failures: map[string]bool{"--target test": true},
	})
	recorder := postAsking(agent, "/agent/ci", "v1")
	envelope := decodeStageEnvelope(t, recorder.Body.Bytes())
	if recorder.Code != 500 || envelope.Error.Code != "ci_test" {
		t.Fatalf("테스트 실패는 그대로 500 ci_test: %d %s", recorder.Code, envelope.Error.Code)
	}
	output := decodeStageOutput(t, envelope)
	if strings.Contains(recorder.Body.String(), token[4:20]) || strings.Contains(recorder.Body.String(), password) {
		t.Fatal("보내는 봉투에 가리지 않은 비밀값이 있다")
	}
	for _, want := range []string{"#9 [test 3/4] RUN pytest\n", "using [가림:token] to fetch fixtures\n", "DB_PASSWORD=[가림:secret_field]\n",
		"FAILED tests/test_queue.py::test_order - assert 1 == 2\n", "exit status 1"} {
		if !strings.Contains(output.Text, want) {
			t.Fatalf("전체 출력에 %q 가 없다", want)
		}
	}
	if output.Truncated || output.OmittedBytes != 0 || output.Redactions["token"] != 1 || output.Redactions["secret_field"] != 1 {
		t.Fatalf("상한 밑의 출력은 잘리지 않고 가린 횟수를 싣는다: %+v", output.Redactions)
	}
	failureText := testOutput + "\nexit status 1"
	if envelope.Error.Detail != failureText[len(failureText)-2000:] {
		t.Fatal("detail 은 그대로 실패 글의 끝 2000바이트여야 한다")
	}
}

func TestStageFailureOutputIsCappedBeforeSending(t *testing.T) {
	var builder strings.Builder
	for number := 0; builder.Len() < 3<<20; number++ {
		fmt.Fprintf(&builder, "step output line %07d\n", number)
	}
	agent := newScriptedAgent(t, &scriptedCommands{
		outputs:  map[string]string{"compose build": builder.String()},
		failures: map[string]bool{"compose build": true},
	})
	recorder := postAsking(agent, "/agent/ci", "v1")
	envelope := decodeStageEnvelope(t, recorder.Body.Bytes())
	output := decodeStageOutput(t, envelope)
	if envelope.Error.Code != "ci_build" || !output.Truncated || output.OmittedBytes <= 0 || len(output.Text) > shared.FailureOutputLimit {
		t.Fatalf("3MB 의 빌드 출력은 상한 안으로 잘려 실린다: code=%s truncated=%v omitted=%d len=%d",
			envelope.Error.Code, output.Truncated, output.OmittedBytes, len(output.Text))
	}
	if recorder.Body.Len() > 2<<20 {
		t.Fatalf("봉투가 너무 크다: %d바이트", recorder.Body.Len())
	}
	if !strings.HasPrefix(output.Text, "step output line 0000000\n") || !strings.HasSuffix(output.Text, "exit status 1") {
		t.Fatal("앞(무엇을 돌렸나)과 끝(실패의 까닭)이 남아야 한다")
	}
}

func TestDeliveryFailureCarriesOutput(t *testing.T) {
	agent := newScriptedAgent(t, &scriptedCommands{
		outputs:  map[string]string{"compose up": "Error response from daemon: driver failed on 10.0.0.1"},
		failures: map[string]bool{"compose up": true},
	})
	envelope := decodeStageEnvelope(t, postAsking(agent, "/agent/cd", "v1").Body.Bytes())
	output := decodeStageOutput(t, envelope)
	if envelope.Error.Code != "cd_up" || output.Text != "Error response from daemon: driver failed on [가림:private_ip]\nexit status 1" {
		t.Fatalf("CD 실패에도 가린 전체 출력이 실린다: %s %q", envelope.Error.Code, output.Text)
	}
}

func TestRefusalsAndSuccessCarryNoFailureOutput(t *testing.T) {
	agent := newScriptedAgent(t, &scriptedCommands{})
	if envelope := decodeStageEnvelope(t, postAsking(agent, "/agent/ci", "v1").Body.Bytes()); !envelope.Success || len(envelope.Error.Output) != 0 {
		t.Fatalf("성공에는 실패 출력이 없다: %+v", envelope)
	}
	agent.busy.Lock()
	busy := postAsking(agent, "/agent/ci", "v1")
	agent.busy.Unlock()
	if busy.Code != 409 || busy.Body.String() != `{"error":{"code":"agent_busy"},"success":false}`+"\n" {
		t.Fatalf("작업 중 거절의 봉투가 바뀌었다: %d %q", busy.Code, busy.Body.String())
	}
	agent.composeServices = map[string]string{}
	if refused := postAsking(agent, "/agent/ci", "v1"); refused.Code != 422 || strings.Contains(refused.Body.String(), `"output"`) {
		t.Fatalf("단계 전의 거절에는 output 이 없다: %d %s", refused.Code, refused.Body.String())
	}
}

func TestFailureEnvelopeWithoutOutputRequestIsByteForByteTheSame(t *testing.T) {
	token := agentFakeToken(9)
	failing := &scriptedCommands{
		outputs:  map[string]string{"compose up": "Error response from daemon: " + token + " rejected"},
		failures: map[string]bool{"compose up": true},
	}
	// 봉투의 꼴(칸 · 차례)은 옛 꼴 그대로, detail 은 가린 글의 끝(§22 — 12-2b 에서 "가리지 않은 토큰"에서 바꿨다)
	want := `{"error":{"code":"cd_up","detail":"Error response from daemon: [가림:token] rejected\nexit status 1"},"success":false}` + "\n"
	for _, outputRequest := range []string{"", "1", "V1", "v2", "true", "v1 "} { // 없음(옛 master) · 모르는 값 → 싣지 않는다
		recorder := postAsking(newScriptedAgent(t, failing), "/agent/cd", outputRequest)
		if recorder.Code != 500 || recorder.Body.String() != want {
			t.Fatalf("머리 %q: 청하지 않은 실패 봉투가 지금과 달라졌다:\n got %q\nwant %q", outputRequest, recorder.Body.String(), want)
		}
	}
	if envelope := decodeStageEnvelope(t, postAsking(newScriptedAgent(t, failing), "/agent/cd", "v1").Body.Bytes()); len(envelope.Error.Output) == 0 {
		t.Fatal("v1 을 청하면 output 이 실린다")
	}
}

func TestLargeFailureWithoutOutputRequestStaysWithinOldReadLimit(t *testing.T) {
	agent := newScriptedAgent(t, &scriptedCommands{
		outputs:  map[string]string{"--target test": strings.Repeat("<collected> \"line\"\n", 200_000)},
		failures: map[string]bool{"--target test": true},
	})
	if recorder := post(agent, "/agent/ci"); recorder.Body.Len() > 16384 {
		t.Fatalf("옛 master(16KB 까지 읽음)에게 보내는 봉투가 %d바이트다", recorder.Body.Len())
	}
}
