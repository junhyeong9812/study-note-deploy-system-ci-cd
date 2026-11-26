package master

// 배포 실패의 전체 출력 — 남기기 · 가린 뒤 저장 · 보관 · 조회. 기준: requirement-spec.md(2026-11-25 admin-incident-assist) §2 · §15.
//
// 정한 계약(이 파일이 고정한다):
//   - 자리: DEPLOY_OUTPUT_DIR(비면 DEPLOY_DB_PATH 의 폴더 아래 failure-outputs). master 가 뜰 때 0700 으로 만든다 — 만들지 못해도 뜬다(로그 한 줄).
//     요청 ID 하나에 파일 하나 `<요청 ID>.json`(0600, 같은 폴더의 임시 파일에 쓴 뒤 이름 바꾸기).
//   - 청하기: master 는 agent 의 단계 요청(/agent/ci · /agent/cd)마다 머리 `X-Deploy-Failure-Output: v1` 을 싣는다 —
//     agent 는 그 머리가 있을 때만 error.output 을 싣는다(옛 master 와 섞여도 봉투가 커지지 않는다).
//   - 무엇을: agent 단계(ci_fetch · ci_test · ci_build · cd_up · cd_health · cd_switch)의 실패만. agent 의 error.output.text 가 있으면 그것,
//     없으면(옛 agent) detail 을 master 가 shared.CaptureOutput 으로 한 번 더 가리고 줄여 남긴다(agent 를 믿지 않는다).
//     truncated · omitted_bytes · redactions 는 agent 의 값과 master 의 값을 합친다. 실패가 조회에 보이기 전에 남긴다.
//     성공 · master 단계의 실패(agent_unreachable · agent_rejected · store_failed · interrupted)는 남기지 않는다.
//   - 파일의 꼴(= 조회 응답의 data): request_id · service · commit_sha · failed_stage · recorded_at(RFC3339Nano UTC — 주입한 시계) ·
//     output · truncated · omitted_bytes · bytes(output 의 바이트 수) · redactions.
//   - 보관: 14일(남긴 시각부터 14일까지는 남고, 그 뒤는 지운 것) 또는 최근 50건. 지우는 때 = 남길 때마다 · master 가 뜰 때. 조회는 14일이
//     지난 것을 지운 것으로 답한다(지우기 전이라도). 지운 자리에는 출력 없는 표지(request_id · failed_stage · recorded_at · expired_at)가 남는다.
//     표지는 지운 때(expired_at)부터 90일까지 남고(그때까지 410), 그 뒤에는 정리 때 지워지고 조회는 404 output_not_recorded(정리 전이라도).
//     `<요청 ID 꼴>.json` 이 아닌 이름 · 일반 파일이 아닌 것(폴더 · 심볼릭 링크) · 읽을 수 없는 파일은 건드리지 않는다.
//   - 조회: GET /deploys/{requestID}/output — 인증은 기존 조회와 같다. 응답 머리 Cache-Control: no-store · X-Content-Type-Options: nosniff.
//     | 상황 | 상태 | code |
//     | 비밀값 없음 · 틀림 | 401 | unauthorized |
//     | 요청 ID 가 `^[A-Za-z0-9_-]{1,64}$` 이 아님 | 422 | invalid_request_id |
//     | 이력에 없음 | 404 | not_found |
//     | 진행 중 | 409 | not_finished |
//     | 성공한 배포 | 409 | not_failed |
//     | 실패 · 남긴 것이 지워짐(보관 기간 · 50건) | 410 | output_expired |
//     | 실패 · 남긴 것이 없음(master 단계의 실패 · 남기기 실패 · 이 기능 전의 실패) | 404 | output_not_recorded |
//     | 출력 파일을 읽지 못함(심볼릭 링크 · 폴더를 쓸 수 없음 · 깨진 파일) | 500 | output_read_failed |
//   - 남기지 못하면: 결과 · 이력 · 알림은 그대로, 로그에 `failure output not saved` 한 줄(요청 ID 와 함께 — 출력의 내용은 싣지 않는다).

import (
	"encoding/json"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/junhyeong9812/study-note-deploy-system-ci-cd/internal/shared"
)

// ---- 대역 ----

func masterFakeToken(seed int64) string {
	generator := rand.New(rand.NewSource(seed))
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	body := make([]byte, 36)
	for index := range body {
		body[index] = alphabet[generator.Intn(len(alphabet))]
	}
	return "gh" + "p_" + string(body)
}

type outputScript struct {
	path   string                 // 실패시킬 경로(/agent/ci · /agent/cd)
	code   string                 // 실패 단계
	detail string                 // error.detail
	output *shared.CapturedOutput // nil 이면 봉투에 output 이 없다(옛 agent)
}

// outputAgent — 서비스마다 실패의 봉투를 정해 둔다. holds 가 있는 서비스는 풀릴 때까지 붙잡는다.
type outputAgent struct {
	mutex   sync.Mutex
	scripts map[string]outputScript
	holds   map[string]chan struct{}
	arrived chan string
	calls   map[string]int
	asked   []string // 단계 요청마다 X-Deploy-Failure-Output 의 값
}

func newOutputAgent(t *testing.T) *outputAgent {
	agent := &outputAgent{scripts: map[string]outputScript{}, holds: map[string]chan struct{}{}, arrived: make(chan string, 16), calls: map[string]int{}}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var payload struct {
			Service string `json:"service"`
		}
		json.NewDecoder(request.Body).Decode(&payload)
		agent.mutex.Lock()
		agent.calls[payload.Service+request.URL.Path]++
		agent.asked = append(agent.asked, request.URL.Path+"="+request.Header.Get("X-Deploy-Failure-Output"))
		script, scripted := agent.scripts[payload.Service]
		hold := agent.holds[payload.Service]
		agent.mutex.Unlock()
		if hold != nil {
			agent.arrived <- payload.Service
			<-hold
		}
		if !scripted || script.path != request.URL.Path {
			shared.WriteOK(writer, map[string]any{"took_ms": 1})
			return
		}
		failure := map[string]any{"code": script.code, "detail": script.detail}
		if script.output != nil {
			failure["output"] = script.output
		}
		body, _ := json.Marshal(map[string]any{"success": false, "error": failure})
		writer.Header().Set("Content-Type", "application/json; charset=utf-8")
		writer.WriteHeader(http.StatusInternalServerError)
		writer.Write(body)
	}))
	t.Cleanup(server.Close)
	t.Setenv("AGENT_URL_LLM", server.URL)
	t.Setenv("AGENT_URL_FRONT", server.URL)
	return agent
}

func (agent *outputAgent) script(service string, script outputScript) {
	agent.mutex.Lock()
	defer agent.mutex.Unlock()
	agent.scripts[service] = script
}

func (agent *outputAgent) callCount(key string) int {
	agent.mutex.Lock()
	defer agent.mutex.Unlock()
	return agent.calls[key]
}

func rawOutput(text string) *shared.CapturedOutput {
	return &shared.CapturedOutput{Text: text, Redactions: map[string]int{}}
}

type fakeClock struct {
	mutex   sync.Mutex
	current time.Time
}

func (clock *fakeClock) Now() time.Time {
	clock.mutex.Lock()
	defer clock.mutex.Unlock()
	return clock.current
}

func (clock *fakeClock) Advance(duration time.Duration) {
	clock.mutex.Lock()
	defer clock.mutex.Unlock()
	clock.current = clock.current.Add(duration)
}

var clockStart = time.Date(2026, 11, 26, 3, 0, 0, 0, time.UTC)

func newOutputMaster(t *testing.T) (*Master, string, *fakeClock) {
	t.Helper()
	directory := filepath.Join(t.TempDir(), "failure-outputs") // 아직 없다 — master 가 만든다
	t.Setenv("DEPLOY_OUTPUT_DIR", directory)
	master := newTestMaster(t)
	clock := &fakeClock{current: clockStart}
	master.clock = clock.Now
	return master, directory, clock
}

func submitService(master *Master, service, requestID string) *httptest.ResponseRecorder {
	return call(master, "POST", "/deploy", `{"service":"`+service+`","commit_sha":"`+deploySha+`","request_id":"`+requestID+`"}`)
}

func failDeployment(t *testing.T, master *Master, agent *outputAgent, requestID string, script outputScript) Deployment {
	t.Helper()
	agent.script("llm", script)
	if recorder := submitService(master, "llm", requestID); recorder.Code != 202 {
		t.Fatalf("접수 실패: %d %s", recorder.Code, recorder.Body.String())
	}
	return waitSettled(t, master, requestID)
}

func lookupOutput(master *Master, requestID string) *httptest.ResponseRecorder {
	return call(master, "GET", "/deploys/"+requestID+"/output", "")
}

type outputRecord struct {
	RequestID    string         `json:"request_id"`
	Service      string         `json:"service"`
	CommitSha    string         `json:"commit_sha"`
	FailedStage  string         `json:"failed_stage"`
	RecordedAt   string         `json:"recorded_at"`
	Output       string         `json:"output"`
	Truncated    bool           `json:"truncated"`
	OmittedBytes int64          `json:"omitted_bytes"`
	Bytes        int            `json:"bytes"`
	Redactions   map[string]int `json:"redactions"`
}

func decodeOutputRecord(t *testing.T, recorder *httptest.ResponseRecorder) outputRecord {
	t.Helper()
	if recorder.Code != 200 {
		t.Fatalf("출력 조회가 200 이 아니다: %d %s", recorder.Code, recorder.Body.String())
	}
	var envelope struct {
		Success bool         `json:"success"`
		Data    outputRecord `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil || !envelope.Success {
		t.Fatalf("봉투가 아니다: %v %s", err, recorder.Body.String())
	}
	return envelope.Data
}

func requireFailure(t *testing.T, recorder *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if want := `{"error":{"code":"` + code + `"},"success":false}` + "\n"; recorder.Code != status || recorder.Body.String() != want {
		t.Fatalf("답이 다르다:\n  받은 것 %d %q\n  기대     %d %q", recorder.Code, recorder.Body.String(), status, want)
	}
}

// directoryBytes — 폴더 안 모든 파일(숨은 임시 파일 포함)의 바이트를 이어 붙인다.
func directoryBytes(t *testing.T, directory string) string {
	t.Helper()
	var builder strings.Builder
	filepath.Walk(directory, func(path string, info os.FileInfo, err error) error {
		if err == nil && info.Mode().IsRegular() {
			content, _ := os.ReadFile(path)
			builder.Write(content)
		}
		return nil
	})
	return builder.String()
}

// storedFile — 저장된 파일을 JSON 으로 읽는다.
func storedFile(t *testing.T, directory, requestID string) map[string]json.RawMessage {
	t.Helper()
	content, err := os.ReadFile(filepath.Join(directory, requestID+".json"))
	if err != nil {
		t.Fatalf("%s 의 출력 파일을 읽지 못했다: %v", requestID, err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(content, &fields); err != nil {
		t.Fatalf("출력 파일이 JSON 이 아니다: %v", err)
	}
	return fields
}

func filesWithOutput(t *testing.T, directory string) int {
	t.Helper()
	entries, _ := os.ReadDir(directory)
	count := 0
	for _, entry := range entries {
		content, err := os.ReadFile(filepath.Join(directory, entry.Name()))
		if err != nil {
			continue
		}
		var fields map[string]json.RawMessage
		if json.Unmarshal(content, &fields) == nil && fields["output"] != nil {
			count++
		}
	}
	return count
}

// ---- 1. 남긴다 ----

func TestFailedDeploymentOutputIsKeptAndServedByRequestID(t *testing.T) {
	agent := newOutputAgent(t)
	master, _, _ := newOutputMaster(t)
	token := masterFakeToken(1)
	agent.script("llm", outputScript{path: "/agent/ci", code: "ci_test", detail: "exit status 1",
		output: rawOutput("#9 RUN pytest\nusing " + token + "\nFAILED tests/test_queue.py::test_order\nexit status 1")})
	submitService(master, "llm", "gh-o1-1")
	waitFinished(t, master, "gh-o1-1") // 실패가 조회에 보이는 순간 — 출력은 이미 남아 있어야 한다
	recorder := lookupOutput(master, "gh-o1-1")
	record := decodeOutputRecord(t, recorder)
	if record.RequestID != "gh-o1-1" || record.Service != "llm" || record.CommitSha != deploySha || record.FailedStage != "ci_test" ||
		record.RecordedAt != clockStart.Format(time.RFC3339Nano) {
		t.Fatalf("누구의 · 어느 단계의 · 언제의 출력인지가 다르다: %+v", record)
	}
	if record.Output != "#9 RUN pytest\nusing [가림:token]\nFAILED tests/test_queue.py::test_order\nexit status 1" ||
		record.Bytes != len(record.Output) || record.Truncated || record.OmittedBytes != 0 || record.Redactions["token"] != 1 {
		t.Fatalf("남긴 출력이 다르다: %+v", record)
	}
	keys := dataKeys(t, recorder)
	sort.Strings(keys)
	if strings.Join(keys, ",") != "bytes,commit_sha,failed_stage,omitted_bytes,output,recorded_at,redactions,request_id,service,truncated" {
		t.Fatalf("응답의 칸이 다르다: %v", keys)
	}
	if recorder.Header().Get("Content-Type") != "application/json; charset=utf-8" || recorder.Header().Get("Cache-Control") != "no-store" ||
		recorder.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("응답 머리가 다르다: %v", recorder.Header())
	}
}

func TestEachAgentStageFailureIsKeptWithItsStage(t *testing.T) {
	agent := newOutputAgent(t)
	master, _, _ := newOutputMaster(t)
	for index, testCase := range []struct{ path, code string }{
		{"/agent/ci", "ci_fetch"}, {"/agent/ci", "ci_build"}, {"/agent/cd", "cd_up"}, {"/agent/cd", "cd_health"}, {"/agent/cd", "cd_switch"},
	} {
		requestID := "gh-o2-" + string(rune('a'+index))
		failDeployment(t, master, agent, requestID, outputScript{path: testCase.path, code: testCase.code, detail: "tail", output: rawOutput("output of " + testCase.code)})
		if record := decodeOutputRecord(t, lookupOutput(master, requestID)); record.FailedStage != testCase.code || record.Output != "output of "+testCase.code {
			t.Fatalf("%s 실패의 출력이 그 단계와 함께 남아야 한다: %+v", testCase.code, record)
		}
	}
}

func TestSucceededDeploymentKeepsNoOutput(t *testing.T) {
	newOutputAgent(t)
	master, directory, _ := newOutputMaster(t)
	submitService(master, "llm", "gh-o3-1")
	if deployment := waitSettled(t, master, "gh-o3-1"); deployment.State != StateSucceeded {
		t.Fatalf("성공해야 한다: %+v", deployment)
	}
	requireFailure(t, lookupOutput(master, "gh-o3-1"), http.StatusConflict, "not_failed")
	if _, err := os.Stat(filepath.Join(directory, "gh-o3-1.json")); !os.IsNotExist(err) {
		t.Fatalf("성공한 배포의 출력 파일이 생겼다: %v", err)
	}
}

func TestMasterStageFailuresKeepNoOutput(t *testing.T) {
	agent := newOutputAgent(t)
	master, directory, _ := newOutputMaster(t)
	// agent 가 단계 전에 거절 → agent_rejected
	failDeployment(t, master, agent, "gh-o4-1", outputScript{path: "/agent/ci", code: "service_not_configured", detail: "DEPLOY_SVC_LLM", output: rawOutput("never kept")})
	requireFailure(t, lookupOutput(master, "gh-o4-1"), http.StatusNotFound, "output_not_recorded")
	if strings.Contains(directoryBytes(t, directory), "never kept") {
		t.Fatal("agent 단계가 아닌 실패의 출력을 남겼다")
	}
}

func TestOldAgentWithoutOutputKeepsRedactedDetail(t *testing.T) {
	agent := newOutputAgent(t)
	master, _, _ := newOutputMaster(t)
	token := masterFakeToken(2)
	failDeployment(t, master, agent, "gh-o5-1", outputScript{path: "/agent/ci", code: "ci_test", detail: "fetch with " + token + "\nFAILED test_order"})
	record := decodeOutputRecord(t, lookupOutput(master, "gh-o5-1"))
	if record.Output != "fetch with [가림:token]\nFAILED test_order" || record.Redactions["token"] != 1 {
		t.Fatalf("output 이 없는 봉투는 detail 을 가려 남긴다: %+v", record)
	}
	if deployment := lookup(t, master, "gh-o5-1"); deployment.Detail != "fetch with "+token+"\nFAILED test_order" {
		t.Fatalf("이력의 detail 은 그대로다: %q", deployment.Detail)
	}
}

func TestAgentCountsAreMergedWithMasterCounts(t *testing.T) {
	agent := newOutputAgent(t)
	master, _, _ := newOutputMaster(t)
	agentOutput := &shared.CapturedOutput{Text: "head\n[가운데 1000바이트를 버렸다]\nusing " + masterFakeToken(3) + " tail [가림:email]",
		Truncated: true, OmittedBytes: 1000, Redactions: map[string]int{"email": 2}}
	failDeployment(t, master, agent, "gh-o6-1", outputScript{path: "/agent/ci", code: "ci_test", detail: "tail", output: agentOutput})
	record := decodeOutputRecord(t, lookupOutput(master, "gh-o6-1"))
	if !record.Truncated || record.OmittedBytes != 1000 || record.Redactions["email"] != 2 || record.Redactions["token"] != 1 ||
		record.Output != "head\n[가운데 1000바이트를 버렸다]\nusing [가림:token] tail [가림:email]" {
		t.Fatalf("agent 가 줄이고 가린 것과 master 가 더 가린 것을 합쳐야 한다: %+v", record)
	}
}

func TestResubmittingFailedRequestKeepsTheFirstOutput(t *testing.T) {
	agent := newOutputAgent(t)
	master, directory, clock := newOutputMaster(t)
	failDeployment(t, master, agent, "gh-o7-1", outputScript{path: "/agent/ci", code: "ci_test", detail: "tail", output: rawOutput("first run")})
	before, _ := os.ReadFile(filepath.Join(directory, "gh-o7-1.json"))
	clock.Advance(time.Hour)
	agent.script("llm", outputScript{path: "/agent/ci", code: "ci_build", detail: "tail", output: rawOutput("second run")})
	if recorder := submitService(master, "llm", "gh-o7-1"); recorder.Code != 202 || !strings.Contains(recorder.Body.String(), `"duplicate":true`) {
		t.Fatalf("같은 요청 ID 는 재접수다: %d %s", recorder.Code, recorder.Body.String())
	}
	time.Sleep(50 * time.Millisecond)
	after, _ := os.ReadFile(filepath.Join(directory, "gh-o7-1.json"))
	if string(before) != string(after) || agent.callCount("llm/agent/ci") != 1 {
		t.Fatalf("재접수가 출력을 바꿨다(또는 배포를 다시 돌렸다): calls=%d", agent.callCount("llm/agent/ci"))
	}
}

func TestConcurrentFailuresKeepTheirOwnOutputs(t *testing.T) {
	agent := newOutputAgent(t)
	master, directory, _ := newOutputMaster(t)
	release := make(chan struct{})
	agent.holds["llm"], agent.holds["front"] = release, release
	agent.script("llm", outputScript{path: "/agent/ci", code: "ci_test", detail: "llm tail", output: rawOutput(strings.Repeat("llm-output-line\n", 2000))})
	agent.script("front", outputScript{path: "/agent/ci", code: "ci_build", detail: "front tail", output: rawOutput(strings.Repeat("front-output-line\n", 2000))})
	submitService(master, "llm", "gh-o8-llm")
	submitService(master, "front", "gh-o8-front")
	for range 2 {
		select {
		case <-agent.arrived:
		case <-time.After(5 * time.Second):
			t.Fatal("두 배포가 agent 에 닿지 않았다")
		}
	}
	close(release)
	waitSettled(t, master, "gh-o8-llm")
	waitSettled(t, master, "gh-o8-front")
	llm := decodeOutputRecord(t, lookupOutput(master, "gh-o8-llm"))
	front := decodeOutputRecord(t, lookupOutput(master, "gh-o8-front"))
	if llm.Output != strings.Repeat("llm-output-line\n", 2000) || llm.FailedStage != "ci_test" || llm.Service != "llm" {
		t.Fatalf("llm 의 출력이 섞이거나 바뀌었다: stage=%s len=%d", llm.FailedStage, len(llm.Output))
	}
	if front.Output != strings.Repeat("front-output-line\n", 2000) || front.FailedStage != "ci_build" || front.Service != "front" {
		t.Fatalf("front 의 출력이 섞이거나 바뀌었다: stage=%s len=%d", front.FailedStage, len(front.Output))
	}
	if count := filesWithOutput(t, directory); count != 2 {
		t.Fatalf("출력 파일은 둘이어야 한다: %d", count)
	}
}

// ---- 2. 가린 뒤 저장 · 크기 상한 ----

func TestStoredFileOnDiskIsRedacted(t *testing.T) {
	agent := newOutputAgent(t)
	master, directory, _ := newOutputMaster(t)
	token := masterFakeToken(4)
	password := "Zq" + strings.Repeat("9fK", 6)
	raw := "Authorization: Bearer " + password + "x\nDB_PASSWORD=" + password + "\nusing " + token + "\nmail ci@example.com from 10.0.0.1\n"
	failDeployment(t, master, agent, "gh-o9-1", outputScript{path: "/agent/ci", code: "ci_test", detail: "exit status 1", output: rawOutput(raw)})
	onDisk := directoryBytes(t, directory)
	for _, secret := range []string{token[4:20], password, "ci@example.com", "10.0.0.1"} {
		if strings.Contains(onDisk, secret) {
			t.Fatalf("디스크의 파일에 가리지 않은 값 %q 가 있다", secret)
		}
	}
	var output string
	json.Unmarshal(storedFile(t, directory, "gh-o9-1")["output"], &output)
	if output != "Authorization: Bearer [가림:authorization]\nDB_PASSWORD=[가림:secret_field]\nusing [가림:token]\nmail [가림:email] from [가림:private_ip]\n" {
		t.Fatalf("파일의 출력이 가린 글이 아니다: %q", output)
	}
	info, err := os.Stat(filepath.Join(directory, "gh-o9-1.json"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("출력 파일은 0600 이어야 한다: %v %v", info.Mode(), err)
	}
	if folder, err := os.Stat(directory); err != nil || folder.Mode().Perm() != 0o700 {
		t.Fatalf("출력 폴더는 0700 이어야 한다: %v", err)
	}
}

func TestOversizedOutputFromAgentIsCappedByMaster(t *testing.T) {
	agent := newOutputAgent(t)
	master, _, _ := newOutputMaster(t)
	var builder strings.Builder
	for builder.Len() < 1<<20 {
		builder.WriteString("unbounded agent output line\n")
	}
	failDeployment(t, master, agent, "gh-o10-1", outputScript{path: "/agent/ci", code: "ci_test", detail: "tail", output: rawOutput(builder.String())})
	record := decodeOutputRecord(t, lookupOutput(master, "gh-o10-1"))
	if !record.Truncated || record.OmittedBytes <= 0 || record.Bytes > shared.FailureOutputLimit || !strings.Contains(record.Output, "바이트를 버렸다]") {
		t.Fatalf("상한을 넘는 출력은 master 도 줄여 남긴다: truncated=%v omitted=%d bytes=%d", record.Truncated, record.OmittedBytes, record.Bytes)
	}
}

func TestEscapeHeavyOutputStillRecordsAgentStage(t *testing.T) {
	agent := newOutputAgent(t)
	master, _, _ := newOutputMaster(t)
	heavy := strings.Repeat(`<a href="x">&</a>`+"\n", 14000) // 252,000바이트 — 상한 밑이지만 JSON 으로는 600KB 를 넘는다(옛 읽기 상한 16KB)
	deployment := failDeployment(t, master, agent, "gh-o11-1", outputScript{path: "/agent/ci", code: "ci_test", detail: "exit status 1", output: rawOutput(heavy)})
	if deployment.FailedStage != "ci_test" || deployment.Detail != "exit status 1" {
		t.Fatalf("큰 실패 봉투도 agent 의 단계로 기록해야 한다: %+v", deployment)
	}
	if record := decodeOutputRecord(t, lookupOutput(master, "gh-o11-1")); record.Output != heavy {
		t.Fatal("이스케이프가 많은 출력이 그대로 남지 않았다")
	}
}

// ---- 4. 보관 ----

func TestOutputExpiresAfterFourteenDays(t *testing.T) {
	agent := newOutputAgent(t)
	master, directory, clock := newOutputMaster(t)
	failDeployment(t, master, agent, "gh-o12-1", outputScript{path: "/agent/ci", code: "ci_test", detail: "tail", output: rawOutput("expiring-output-text")})
	clock.Advance(14 * 24 * time.Hour)
	decodeOutputRecord(t, lookupOutput(master, "gh-o12-1")) // 14일째까지는 남는다
	clock.Advance(time.Second)
	requireFailure(t, lookupOutput(master, "gh-o12-1"), http.StatusGone, "output_expired") // 지우기 전이라도 지운 것으로 답한다
	failDeployment(t, master, agent, "gh-o12-2", outputScript{path: "/agent/ci", code: "ci_test", detail: "tail", output: rawOutput("fresh-output-text")})
	if strings.Contains(directoryBytes(t, directory), "expiring-output-text") {
		t.Fatal("14일이 지난 출력이 남길 때 지워지지 않았다")
	}
	requireFailure(t, lookupOutput(master, "gh-o12-1"), http.StatusGone, "output_expired")
	if fields := storedFile(t, directory, "gh-o12-1"); fields["output"] != nil || fields["expired_at"] == nil {
		t.Fatalf("지운 자리에는 출력 없는 표지가 남는다: %v", fields)
	}
	decodeOutputRecord(t, lookupOutput(master, "gh-o12-2"))
}

func TestOnlyFiftyNewestOutputsAreKept(t *testing.T) {
	agent := newOutputAgent(t)
	master, directory, clock := newOutputMaster(t)
	for index := range 51 {
		failDeployment(t, master, agent, "gh-o13-"+itoa(index), outputScript{path: "/agent/ci", code: "ci_test", detail: "tail",
			output: rawOutput("output number " + itoa(index) + " end")})
		clock.Advance(time.Minute)
	}
	requireFailure(t, lookupOutput(master, "gh-o13-0"), http.StatusGone, "output_expired")
	for _, index := range []int{1, 25, 50} {
		if record := decodeOutputRecord(t, lookupOutput(master, "gh-o13-"+itoa(index))); record.Output != "output number "+itoa(index)+" end" {
			t.Fatalf("최근 50건 안의 출력이 다르다: %+v", record)
		}
	}
	if count := filesWithOutput(t, directory); count != 50 {
		t.Fatalf("출력이 든 파일은 50개여야 한다: %d", count)
	}
	if strings.Contains(directoryBytes(t, directory), "output number 0 end") {
		t.Fatal("51번째로 오래된 출력이 디스크에 남았다")
	}
}

func itoa(value int) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

func TestRetentionTouchesOnlyRequestIDFiles(t *testing.T) {
	agent := newOutputAgent(t)
	master, directory, clock := newOutputMaster(t)
	failDeployment(t, master, agent, "gh-o14-0", outputScript{path: "/agent/ci", code: "ci_test", detail: "tail", output: rawOutput("first")})
	foreign := map[string]string{
		"notes.txt":     "operator notes",
		".hidden.json":  `{"output":"hidden"}`,
		"bad name.json": `{"output":"spaced"}`,
		"README":        "readme",
		"gh-junk.json":  "not json at all",
		"gh-old.txt":    `{"recorded_at":"2020-01-01T00:00:00Z","output":"x"}`,
	}
	for name, content := range foreign {
		if err := os.WriteFile(filepath.Join(directory, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(directory, "gh-folder.json"), 0o755); err != nil {
		t.Fatal(err)
	}
	clock.Advance(15 * 24 * time.Hour)
	for index := 1; index <= 52; index++ {
		failDeployment(t, master, agent, "gh-o14-"+itoa(index), outputScript{path: "/agent/ci", code: "ci_test", detail: "tail", output: rawOutput("later")})
		clock.Advance(time.Minute)
	}
	for name, content := range foreign {
		if kept, err := os.ReadFile(filepath.Join(directory, name)); err != nil || string(kept) != content {
			t.Fatalf("요청 ID 의 출력이 아닌 %q 를 건드렸다: %v %q", name, err, kept)
		}
	}
	if info, err := os.Stat(filepath.Join(directory, "gh-folder.json")); err != nil || !info.IsDir() {
		t.Fatalf("폴더를 건드렸다: %v", err)
	}
	requireFailure(t, lookupOutput(master, "gh-o14-0"), http.StatusGone, "output_expired")
}

func TestStartupExpiresOldOutputs(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "deploys.db")
	directory := filepath.Join(root, "failure-outputs")
	store, err := openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	recordedAt := time.Now().UTC().Add(-15 * 24 * time.Hour)
	store.Insert(Deployment{RequestID: "gh-o15-1", Service: "llm", CommitSha: deploySha, State: StateFailed, FailedStage: "ci_test",
		Detail: "tail", AcceptedAt: recordedAt.Format(time.RFC3339), FinishedAt: recordedAt.Format(time.RFC3339)})
	store.database.Close()
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	old, _ := json.Marshal(map[string]any{"request_id": "gh-o15-1", "service": "llm", "commit_sha": deploySha, "failed_stage": "ci_test",
		"recorded_at": recordedAt.Format(time.RFC3339Nano), "sequence": 1, "output": "stale-output-text", "truncated": false, "omitted_bytes": 0,
		"bytes": 17, "redactions": map[string]int{}})
	if err := os.WriteFile(filepath.Join(directory, "gh-o15-1.json"), old, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DEPLOY_OUTPUT_DIR", "") // 기본 자리 = DEPLOY_DB_PATH 의 폴더 아래 failure-outputs
	master, err := startMasterOn(t, path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(directoryBytes(t, directory), "stale-output-text") {
		t.Fatal("master 가 뜰 때 14일이 지난 출력을 지우지 않았다")
	}
	requireFailure(t, lookupOutput(master, "gh-o15-1"), http.StatusGone, "output_expired")
}

// ---- 5. 조회 ----

func TestOutputLookupAnswersAreDistinguishable(t *testing.T) {
	agent := newOutputAgent(t)
	master, _, clock := newOutputMaster(t)
	requireFailure(t, lookupOutput(master, "gh-none"), http.StatusNotFound, "not_found")

	release := make(chan struct{})
	agent.holds["llm"] = release
	submitService(master, "llm", "gh-o16-1")
	select {
	case <-agent.arrived:
	case <-time.After(5 * time.Second):
		t.Fatal("배포가 agent 에 닿지 않았다")
	}
	requireFailure(t, lookupOutput(master, "gh-o16-1"), http.StatusConflict, "not_finished")
	close(release)
	waitSettled(t, master, "gh-o16-1")
	agent.holds["llm"] = nil
	requireFailure(t, lookupOutput(master, "gh-o16-1"), http.StatusConflict, "not_failed")

	failDeployment(t, master, agent, "gh-o16-2", outputScript{path: "/agent/ci", code: "ci_test", detail: "tail", output: rawOutput("x")})
	clock.Advance(15 * 24 * time.Hour)
	requireFailure(t, lookupOutput(master, "gh-o16-2"), http.StatusGone, "output_expired")
}

func TestOutputLookupRequiresSecret(t *testing.T) {
	agent := newOutputAgent(t)
	master, _, _ := newOutputMaster(t)
	failDeployment(t, master, agent, "gh-o17-1", outputScript{path: "/agent/ci", code: "ci_test", detail: "tail", output: rawOutput("guarded-output")})
	mux := http.NewServeMux()
	master.Register(mux)
	for _, secret := range []string{"", "wrong-value"} {
		request := httptest.NewRequest("GET", "/deploys/gh-o17-1/output", nil)
		if secret != "" {
			request.Header.Set(shared.SecretHeader, secret)
		}
		recorder := httptest.NewRecorder()
		mux.ServeHTTP(recorder, request)
		requireFailure(t, recorder, http.StatusUnauthorized, "unauthorized")
	}
}

func TestOutputLookupRejectsMalformedRequestIDs(t *testing.T) {
	agent := newOutputAgent(t)
	master, directory, _ := newOutputMaster(t)
	failDeployment(t, master, agent, "gh-o18-1", outputScript{path: "/agent/ci", code: "ci_test", detail: "tail", output: rawOutput("inside")})
	// 저장 폴더 바로 밖에 출력 파일 꼴의 미끼를 둔다 — 경로를 그대로 이으면 이것이 읽힌다
	bait, _ := json.Marshal(map[string]any{"request_id": "escape", "service": "llm", "commit_sha": deploySha, "failed_stage": "ci_test",
		"recorded_at": clockStart.Format(time.RFC3339Nano), "output": "OUTSIDE-SENTINEL", "truncated": false, "omitted_bytes": 0, "bytes": 16, "redactions": map[string]int{}})
	os.WriteFile(filepath.Join(directory, "..", "escape.json"), bait, 0o600)
	os.WriteFile(filepath.Join(directory, "..", "escape"), bait, 0o600)
	for _, path := range []string{
		"/deploys/..%2Fescape/output",
		"/deploys/%2E%2E%2Fescape/output",
		"/deploys/gh-o18-1%2F..%2F..%2Fescape/output",
		"/deploys/gh%00x/output",
		"/deploys/gh%20x/output",
		"/deploys/gh-o18-1.json/output",
		"/deploys/%ED%95%9C/output",
		"/deploys/" + strings.Repeat("a", 65) + "/output",
	} {
		recorder := lookupOutput(master, strings.TrimSuffix(strings.TrimPrefix(path, "/deploys/"), "/output"))
		if strings.Contains(recorder.Body.String(), "OUTSIDE-SENTINEL") {
			t.Fatalf("%s: 저장 폴더 밖을 읽었다", path)
		}
		requireFailure(t, recorder, http.StatusUnprocessableEntity, "invalid_request_id")
	}
	for _, path := range []string{"/deploys/../escape/output", "/deploys/%2e%2e/output"} { // 라우터가 먼저 정리하는 꼴 — 어떻게 답하든 밖을 읽지 않는다
		recorder := lookupOutput(master, strings.TrimSuffix(strings.TrimPrefix(path, "/deploys/"), "/output"))
		if recorder.Code == 200 || strings.Contains(recorder.Body.String(), "OUTSIDE-SENTINEL") {
			t.Fatalf("%s: 밖을 읽었다: %d %s", path, recorder.Code, recorder.Body.String())
		}
	}
	requireFailure(t, lookupOutput(master, strings.Repeat("a", 64)), http.StatusNotFound, "not_found") // 64자는 꼴에 맞다
}

func TestOutputLookupDoesNotFollowSymlinks(t *testing.T) {
	agent := newOutputAgent(t)
	master, directory, _ := newOutputMaster(t)
	failDeployment(t, master, agent, "gh-o19-1", outputScript{path: "/agent/ci", code: "ci_test", detail: "tail", output: rawOutput("real")})
	outside := filepath.Join(t.TempDir(), "outside.json")
	bait, _ := json.Marshal(map[string]any{"request_id": "gh-o19-1", "service": "llm", "commit_sha": deploySha, "failed_stage": "ci_test",
		"recorded_at": clockStart.Format(time.RFC3339Nano), "output": "OUTSIDE-SENTINEL", "truncated": false, "omitted_bytes": 0, "bytes": 16, "redactions": map[string]int{}})
	os.WriteFile(outside, bait, 0o600)
	os.Remove(filepath.Join(directory, "gh-o19-1.json"))
	if err := os.Symlink(outside, filepath.Join(directory, "gh-o19-1.json")); err != nil {
		t.Fatal(err)
	}
	recorder := lookupOutput(master, "gh-o19-1")
	if strings.Contains(recorder.Body.String(), "OUTSIDE-SENTINEL") {
		t.Fatal("심볼릭 링크를 따라 폴더 밖을 읽었다")
	}
	requireFailure(t, recorder, http.StatusInternalServerError, "output_read_failed")
	// 보관 정리도 링크를 따라가 밖의 파일을 고치지 않는다
	for index := range 51 {
		failDeployment(t, master, agent, "gh-o19-x"+itoa(index), outputScript{path: "/agent/ci", code: "ci_test", detail: "tail", output: rawOutput("later")})
	}
	if kept, _ := os.ReadFile(outside); string(kept) != string(bait) {
		t.Fatal("보관 정리가 링크 너머의 파일을 고쳤다")
	}
	if info, err := os.Lstat(filepath.Join(directory, "gh-o19-1.json")); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("링크 자체도 건드리지 않는다: %v", err)
	}
}

// ---- 6. 남기지 못해도 배포는 그대로 ----

func requireSameFailureEverywhere(t *testing.T, master *Master, alerts *alertRecorder, requestID string, deployment Deployment) {
	t.Helper()
	if deployment.State != StateFailed || deployment.FailedStage != "ci_test" || deployment.Detail != "exit status 1" {
		t.Fatalf("남기기 실패가 배포의 결과를 바꿨다: %+v", deployment)
	}
	if listing := call(master, "GET", "/deploys?service=llm", "").Body.String(); !strings.Contains(listing, `"request_id":"`+requestID+`"`) ||
		!strings.Contains(listing, `"failed_stage":"ci_test"`) {
		t.Fatalf("이력이 바뀌었다: %s", listing)
	}
	found := false
	for _, event := range alerts.waitEventsOfKind("deploy_failed", 1, 2*time.Second) {
		if event["title"] == "배포 실패 llm — 단계 ci_test" && event["detail"] == "커밋 "+deploySha+"\nexit status 1" {
			found = true
		}
	}
	if !found {
		t.Fatalf("알림이 바뀌었다: %v", alerts.eventsOfKind("deploy_failed"))
	}
}

func requireSaveFailureLogged(t *testing.T, logged, requestID string) {
	t.Helper()
	lines := []string{}
	for _, line := range strings.Split(logged, "\n") {
		if strings.Contains(line, "failure output not saved") {
			lines = append(lines, line)
		}
	}
	if len(lines) != 1 || !strings.Contains(lines[0], requestID+":") {
		t.Fatalf("남기기 실패는 요청 ID 와 함께 로그 한 줄: %q", lines)
	}
	if strings.Contains(logged, "UNSAVED-OUTPUT-SENTINEL") {
		t.Fatal("로그에 출력의 내용을 실었다")
	}
}

func TestOutputSaveFailureLeavesDeploymentAlone(t *testing.T) {
	alerts := newAlertRecorder(t, http.StatusAccepted)
	agent := newOutputAgent(t)
	blocker := filepath.Join(t.TempDir(), "regular-file")
	os.WriteFile(blocker, []byte("x"), 0o600)
	t.Setenv("DEPLOY_OUTPUT_DIR", filepath.Join(blocker, "failure-outputs")) // 파일 아래 — 만들 수도 쓸 수도 없다(root 여도)
	master := newTestMaster(t)                                               // 그래도 뜬다
	master.clock = (&fakeClock{current: clockStart}).Now
	var deployment Deployment
	logged := captureMasterLog(t, func() {
		deployment = failDeployment(t, master, agent, "gh-o20-1", outputScript{path: "/agent/ci", code: "ci_test", detail: "exit status 1",
			output: rawOutput("UNSAVED-OUTPUT-SENTINEL")})
	})
	requireSameFailureEverywhere(t, master, alerts, "gh-o20-1", deployment)
	requireSaveFailureLogged(t, logged, "gh-o20-1")
	requireFailure(t, lookupOutput(master, "gh-o20-1"), http.StatusInternalServerError, "output_read_failed")
}

func TestOutputDirectoryLostAtRuntimeLeavesDeploymentAlone(t *testing.T) {
	alerts := newAlertRecorder(t, http.StatusAccepted)
	agent := newOutputAgent(t)
	master, directory, _ := newOutputMaster(t)
	if err := os.RemoveAll(directory); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(directory, []byte("disk replaced"), 0o600) // 폴더 자리에 파일 — 쓰기가 실패한다
	var deployment Deployment
	logged := captureMasterLog(t, func() {
		deployment = failDeployment(t, master, agent, "gh-o21-1", outputScript{path: "/agent/ci", code: "ci_test", detail: "exit status 1",
			output: rawOutput("UNSAVED-OUTPUT-SENTINEL")})
	})
	requireSameFailureEverywhere(t, master, alerts, "gh-o21-1", deployment)
	requireSaveFailureLogged(t, logged, "gh-o21-1")
	agent.script("llm", outputScript{})
	submitService(master, "llm", "gh-o21-2")
	if next := waitSettled(t, master, "gh-o21-2"); next.State != StateSucceeded {
		t.Fatalf("남기기 실패 뒤의 배포도 그대로 돈다: %+v", next)
	}
}

// ---- 청하기 · 표지의 정리 ----

func TestMasterAsksForFailureOutputOnEveryStageCall(t *testing.T) {
	agent := newOutputAgent(t)
	master, _, _ := newOutputMaster(t)
	submitService(master, "llm", "gh-o22-1") // 성공 — CI · CD 둘 다 부른다
	waitSettled(t, master, "gh-o22-1")
	failDeployment(t, master, agent, "gh-o22-2", outputScript{path: "/agent/cd", code: "cd_health", detail: "tail", output: rawOutput("x")})
	agent.mutex.Lock()
	asked := strings.Join(agent.asked, ",")
	agent.mutex.Unlock()
	if asked != "/agent/ci=v1,/agent/cd=v1,/agent/ci=v1,/agent/cd=v1" {
		t.Fatalf("모든 단계 요청이 실패 출력을 청해야 한다: %s", asked)
	}
}

func TestExpiredMarkerIsRemovedAfterNinetyDays(t *testing.T) {
	agent := newOutputAgent(t)
	master, directory, clock := newOutputMaster(t)
	failDeployment(t, master, agent, "gh-o23-1", outputScript{path: "/agent/ci", code: "ci_test", detail: "tail", output: rawOutput("old")})
	clock.Advance(15 * 24 * time.Hour)
	failDeployment(t, master, agent, "gh-o23-2", outputScript{path: "/agent/ci", code: "ci_test", detail: "tail", output: rawOutput("x")}) // 여기서 표지가 된다
	if fields := storedFile(t, directory, "gh-o23-1"); fields["expired_at"] == nil {
		t.Fatalf("표지가 아니다: %v", fields)
	}
	clock.Advance(90 * 24 * time.Hour)
	requireFailure(t, lookupOutput(master, "gh-o23-1"), http.StatusGone, "output_expired") // 지운 때부터 정확히 90일까지는 410
	clock.Advance(time.Second)
	requireFailure(t, lookupOutput(master, "gh-o23-1"), http.StatusNotFound, "output_not_recorded") // 정리 전이라도
	if _, err := os.Stat(filepath.Join(directory, "gh-o23-1.json")); err != nil {
		t.Fatalf("정리는 남길 때 · 뜰 때만 — 조회는 지우지 않는다: %v", err)
	}
	failDeployment(t, master, agent, "gh-o23-3", outputScript{path: "/agent/ci", code: "ci_test", detail: "tail", output: rawOutput("y")})
	if _, err := os.Stat(filepath.Join(directory, "gh-o23-1.json")); !os.IsNotExist(err) {
		t.Fatalf("90일이 지난 표지를 정리하지 않았다: %v", err)
	}
	if _, err := os.Stat(filepath.Join(directory, "gh-o23-2.json")); err != nil {
		t.Fatalf("다른 표지까지 지웠다: %v", err)
	}
	requireFailure(t, lookupOutput(master, "gh-o23-1"), http.StatusNotFound, "output_not_recorded")
}
