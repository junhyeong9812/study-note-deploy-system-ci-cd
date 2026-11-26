package master

// 실패 글의 끝부분도 가린다(master) — 기준: requirement-spec.md(2026-11-25 admin-incident-assist) §22.
//
// 정한 계약:
//   - 이력 · 알림 · 로그로 가는 모든 detail 은 master 가 쓰기 전에 가린다(agent 가 가렸어도 한 번 더 — 옛 agent 는 가리지 않는다):
//     가린 글(master.redact = shared.CaptureOutput)에서 shared.CutTail 로 — 이력 2000 · 알림의 몸 800(앞의 "커밋 <sha>\n" 은 그대로) · 로그 200.
//   - detail 의 원천 전부: agent 단계 실패의 detail · agent_rejected("<코드>: <detail>") · agent_unreachable(HTTP 오류 글) ·
//     unexpected_response("status N: <본문 16KB>") · store_failed(DB 오류 글) · interrupted(고정 글 — 가릴 것이 없다).
//   - 길이 · 칸 · 알림의 key · 제목 · 등급 · 로그 줄의 앞머리는 그대로. 가림이 없는 detail 은 바이트까지 그대로(특성 테스트).
//   - 가림이 실패(패닉)하면 detail · 알림의 끝 · 로그의 끝은 shared.DetailRedactionFailed 이고, 실패 출력은 남기지 않는다(로그 한 줄).
//     배포의 결과 · 이력 · 알림은 그대로 남고 master 는 계속 돈다.

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/junhyeong9812/study-note-deploy-system-ci-cd/internal/shared"
)

var nextMasterLogEntry = regexp.MustCompile(`\n\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z `)

// failedLogEntry — "deploy failed" 로그 한 건 전체(끝부분의 줄바꿈까지).
func failedLogEntry(t *testing.T, logged string) string {
	t.Helper()
	start := strings.Index(logged, "deploy failed llm")
	if start < 0 {
		t.Fatalf("배포 실패 로그가 없다:\n%s", logged)
	}
	entry := logged[start:]
	if next := nextMasterLogEntry.FindStringIndex(entry); next != nil {
		entry = entry[:next[0]]
	}
	return strings.TrimSuffix(entry, "\n")
}

func requireMasked(t *testing.T, where, text string) {
	t.Helper()
	if strings.Contains(text, "Zq81") || strings.Contains(text, "10.0.0.1") || strings.Contains(text, "ops@example.com") {
		t.Fatalf("%s 에 가리지 않은 값이 있다:\n%s", where, text)
	}
	if index := strings.LastIndex(text, "[가림:"); index >= 0 && !strings.Contains(text[index:], "]") {
		t.Fatalf("%s 의 표시가 잘렸다: %q", where, text[index:])
	}
	if strings.Contains(text, "\n가림:") || strings.HasPrefix(text, "가림:") {
		t.Fatalf("%s 가 표시의 가운데에서 시작한다: %q", where, text)
	}
}

// failureEverywhere — 한 배포의 이력 detail · 알림의 몸 · 로그 한 건.
type failureEverywhere struct {
	deployment Deployment
	alert      map[string]string
	logEntry   string
}

func runFailure(t *testing.T, master *Master, alerts *alertRecorder, requestID string) failureEverywhere {
	t.Helper()
	var result failureEverywhere
	logged := captureMasterLog(t, func() {
		submit(master, requestID)
		result.deployment = waitSettled(t, master, requestID)
	})
	events := alerts.waitEventsOfKind("deploy_failed", 1, 2*time.Second)
	if len(events) != 1 {
		t.Fatalf("배포 실패 알림이 한 건이어야 한다: %v", events)
	}
	result.alert, result.logEntry = events[0], failedLogEntry(t, logged)
	return result
}

func requireMaskedEverywhere(t *testing.T, label string, result failureEverywhere) {
	t.Helper()
	requireMasked(t, label+" 이력 detail", result.deployment.Detail)
	requireMasked(t, label+" 알림의 몸", result.alert["detail"])
	requireMasked(t, label+" 로그", result.logEntry)
}

func rawSecretDetail() string {
	return strings.Repeat("collected line ok\n", 120) + strings.Join([]string{
		"using " + masterFakeToken(40)[:4] + "Zq81" + strings.Repeat("T", 32),
		"password=Zq81pw",
		"Authorization: Bearer Zq81bearer",
		"https://ci:Zq81url@git.example.com/x",
		"db 10.0.0.1 mail ops@example.com",
	}, "\n")
}

func TestMasterMasksDetailAlertAndLogOfOldAgent(t *testing.T) {
	alerts := newAlertRecorder(t, http.StatusAccepted)
	scriptedAgent(t, map[string]string{"/agent/ci": failureBody("ci_test", rawSecretDetail())}) // 옛 agent — 가리지 않은 detail, output 없음
	master := newTestMaster(t)
	result := runFailure(t, master, alerts, "gh-m1-1")
	requireMaskedEverywhere(t, "옛 agent", result)
	if result.deployment.State != StateFailed || result.deployment.FailedStage != "ci_test" || len(result.deployment.Detail) > 2000 ||
		!strings.HasSuffix(result.deployment.Detail, "[가림:private_ip] mail [가림:email]") {
		t.Fatalf("이력: 단계 · 길이 · 끝이 다르다: %+v", result.deployment)
	}
	body := result.alert["detail"]
	if !strings.HasPrefix(body, "커밋 "+deploySha+"\n") || len(body)-len("커밋 "+deploySha+"\n") > 800 ||
		result.alert["title"] != "배포 실패 llm — 단계 ci_test" || result.alert["key"] != "deploy-failed:llm:"+deploySha || result.alert["severity"] != "warning" {
		t.Fatalf("알림의 칸 · 길이가 바뀌었다: %v", result.alert)
	}
	prefix := "deploy failed llm sha=" + deploySha + " stage=ci_test "
	if !strings.HasPrefix(result.logEntry, prefix) || len(result.logEntry)-len(prefix) > 200 {
		t.Fatalf("로그 줄의 앞머리 · 길이가 바뀌었다: %q", result.logEntry)
	}
	for _, mask := range []string{"[가림:token]", "[가림:secret_field]", "[가림:authorization]", "[가림:url_credential]", "[가림:private_ip]", "[가림:email]"} {
		if !strings.Contains(result.deployment.Detail, mask) || !strings.Contains(body, mask) {
			t.Fatalf("이력 · 알림에 %s 가 없다", mask)
		}
	}
}

func TestSecretAcrossEachTailBoundaryLeavesNoHalf(t *testing.T) {
	token := "gh" + "p_" + "Zq81" + strings.Repeat("K", 32)
	for _, limit := range []int{2000, 800, 200} {
		for _, shift := range []int{3, 13, 20, 37} { // 끝에서 limit 바이트 자리가 토큰의 가운데
			alerts := newAlertRecorder(t, http.StatusAccepted)
			detail := strings.Repeat("y ", 1200) + token + " " + strings.Repeat("z", limit-shift-1)
			scriptedAgent(t, map[string]string{"/agent/ci": failureBody("ci_test", detail)})
			master := newTestMaster(t)
			result := runFailure(t, master, alerts, "gh-m2-1")
			requireMaskedEverywhere(t, "경계", result)
			for where, text := range map[string]string{"이력": result.deployment.Detail, "알림": result.alert["detail"], "로그": result.logEntry} {
				if strings.Contains(text, "KKKKKKKK") {
					t.Fatalf("끝 %d 의 경계에 걸친 토큰(shift %d)의 반쪽이 %s 에 남았다", limit, shift, where)
				}
			}
		}
	}
}

// answeringAgent — 경로마다 상태와 본문을 정해 둔 agent 대역.
func answeringAgent(t *testing.T, path string, status int, body string) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		io.Copy(io.Discard, request.Body)
		if request.URL.Path == path {
			writer.WriteHeader(status)
			io.WriteString(writer, body)
			return
		}
		shared.WriteOK(writer, map[string]any{"took_ms": 1})
	}))
	t.Cleanup(server.Close)
	t.Setenv("AGENT_URL_LLM", server.URL)
}

// secretErrorStore — 정해 둔 상태를 기록하려 하면 비밀값이 든 오류를 낸다(DB 오류 글이 detail 의 원천이다).
type secretErrorStore struct {
	deploymentStore
	failOnState string
}

func (store *secretErrorStore) Update(deployment Deployment) error {
	if deployment.State == store.failOnState {
		return errors.New("disk I/O error at 10.0.0.1 token=Zq81LeakStore")
	}
	return store.deploymentStore.Update(deployment)
}

func TestEveryDetailSourceIsMasked(t *testing.T) {
	cases := []struct {
		name, stage string
		prepare     func(t *testing.T)
		adjust      func(master *Master)
	}{
		{"agent 단계 실패", "cd_up", func(t *testing.T) {
			answeringAgent(t, "/agent/cd", 500, failureBody("cd_up", "Error: pull failed password=Zq81LeakStage"))
		}, nil},
		{"agent_rejected", StageAgentRejected, func(t *testing.T) {
			answeringAgent(t, "/agent/ci", 422, failureBody("service_not_configured", "DEPLOY_SVC_LLM token=Zq81LeakRejected"))
		}, nil},
		{"unexpected_response", StageAgentRejected, func(t *testing.T) {
			answeringAgent(t, "/agent/ci", 200, "<html>proxy Authorization: Bearer Zq81LeakProxy from 10.0.0.1</html>")
		}, nil},
		{"agent_unreachable", StageAgentUnreachable, func(t *testing.T) {
			t.Setenv("AGENT_URL_LLM", "http://127.0.0.1:1/x?token=Zq81LeakUnreachable")
		}, nil},
		{"store_failed(단계 기록)", StageStore, func(t *testing.T) { answeringAgent(t, "/none", 200, "") },
			func(master *Master) {
				master.store = &secretErrorStore{deploymentStore: master.store, failOnState: StateDelivering}
			}},
		{"store_failed(성공 기록)", StageStore, func(t *testing.T) { answeringAgent(t, "/none", 200, "") },
			func(master *Master) {
				master.store = &secretErrorStore{deploymentStore: master.store, failOnState: StateSucceeded}
			}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			alerts := newAlertRecorder(t, http.StatusAccepted)
			testCase.prepare(t)
			master := newTestMaster(t)
			if testCase.adjust != nil {
				testCase.adjust(master)
			}
			result := runFailure(t, master, alerts, "gh-m3-1")
			if result.deployment.FailedStage != testCase.stage {
				t.Fatalf("단계가 바뀌었다: %s", result.deployment.FailedStage)
			}
			requireMaskedEverywhere(t, testCase.name, result)
			if !strings.Contains(result.deployment.Detail, "[가림:") {
				t.Fatalf("detail 에 가린 자리가 없다: %q", result.deployment.Detail)
			}
		})
	}
}

func TestRedactionPanicLeavesDeploymentResultAlertAndHistory(t *testing.T) {
	alerts := newAlertRecorder(t, http.StatusAccepted)
	agent := newOutputAgent(t)
	master, _, _ := newOutputMaster(t)
	master.redact = func(string) shared.CapturedOutput { panic("redaction broke") }
	agent.script("llm", outputScript{path: "/agent/ci", code: "ci_test", detail: "password=Zq81LeakPanic", output: rawOutput("password=Zq81LeakPanic")})
	var deployment Deployment
	logged := captureMasterLog(t, func() {
		submitService(master, "llm", "gh-m4-1")
		deployment = waitSettled(t, master, "gh-m4-1")
	})
	if deployment.State != StateFailed || deployment.FailedStage != "ci_test" || deployment.Detail != shared.DetailRedactionFailed {
		t.Fatalf("가림이 실패해도 결과 · 단계는 그대로, detail 은 고정 글: %+v", deployment)
	}
	events := alerts.waitEventsOfKind("deploy_failed", 1, 2*time.Second)
	if len(events) != 1 || events[0]["detail"] != "커밋 "+deploySha+"\n"+shared.DetailRedactionFailed || events[0]["title"] != "배포 실패 llm — 단계 ci_test" {
		t.Fatalf("가림이 실패해도 알림은 간다(끝부분은 고정 글): %v", events)
	}
	if strings.Contains(logged, "Zq81") || !strings.Contains(failedLogEntry(t, logged), shared.DetailRedactionFailed) || !strings.Contains(logged, "failure output not saved") {
		t.Fatalf("로그에 비밀값이 없고 · 끝은 고정 글 · 출력을 남기지 못했다는 줄이 있다:\n%s", logged)
	}
	requireFailure(t, lookupOutput(master, "gh-m4-1"), http.StatusNotFound, "output_not_recorded")
	agent.script("llm", outputScript{})
	submitService(master, "llm", "gh-m4-2")
	if next := waitSettled(t, master, "gh-m4-2"); next.State != StateSucceeded {
		t.Fatalf("가림이 실패한 뒤에도 master 는 계속 돈다: %+v", next)
	}
}
