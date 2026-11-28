package agent

// 배포 대상 claude(Claude 실행 자리) — 기준: requirement-spec.md(2026-11-25 claude-fallback) §7 · §10 끝 "배포의 선행".
//
// 정한 계약:
//   - agent 는 서비스 이름 claude 의 설정을 읽는다: DEPLOY_DIR_CLAUDE · DEPLOY_SVC_CLAUDE(실행 자리의 compose 서비스 claude-runner) ·
//     DEPLOY_HEALTH_CLAUDE(포트 15200 · 경로 /health) · DEPLOY_TEST_CONTEXT_CLAUDE(없으면 ".").
//     DEPLOY_MODE_CLAUDE 는 두지 않는다 — 단일 컨테이너 방식(지금 있는 방식 그대로: 테스트 → 빌드 → up -d → 헬스 200 → 성공).
//   - 헬스가 200 이 아니면(토큰이 없어 503) cd_health 로 실패한다 — 이전 이미지를 지우지 않는다(지금의 단일 컨테이너 규칙).
//   - 실패의 끝부분 · 전체 출력의 가림(12-2 · 12-2b)이 이 서비스에도 걸린다 — 구독 토큰 줄 `CLAUDE_CODE_OAUTH_TOKEN=…` 도 가려진다.

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/junhyeong9812/study-note-deploy-system-ci-cd/internal/shared"
)

const claudeHealthURL = "http://192.0.2.58:15200/health" // 문서용 주소 — 실행 자리는 LAN 주소(BIND_ADDR)에만 15200 을 연다

func newClaudeAgent(t *testing.T, recorded *recordedCommands) *Agent {
	t.Setenv("DEPLOY_DIR_CLAUDE", "/srv/claude")
	t.Setenv("DEPLOY_SVC_CLAUDE", "claude-runner")
	t.Setenv("DEPLOY_HEALTH_CLAUDE", claudeHealthURL)
	agent := newPipelineAgent(t, recorded)
	return agent
}

func postService(agent *Agent, path, service string) *httptest.ResponseRecorder {
	mux := http.NewServeMux()
	agent.Register(mux)
	request := httptest.NewRequest("POST", path, strings.NewReader(`{"service":"`+service+`","commit_sha":"`+requestSha+`","request_id":"gh-claude-1"}`))
	request.Header.Set(shared.SecretHeader, "test-value")
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, request)
	return recorder
}

func TestAgentLoadsClaudeDeploySettings(t *testing.T) {
	agent := newClaudeAgent(t, &recordedCommands{})
	if agent.directories["claude"] != "/srv/claude" || agent.composeServices["claude"] != "claude-runner" ||
		agent.healthURLs["claude"] != claudeHealthURL || agent.modes["claude"] == modeBlueGreen || agent.testContext("claude") != "." {
		t.Fatalf("claude 의 배포 설정을 읽지 못했다: dir=%q svc=%q health=%q mode=%q test=%q",
			agent.directories["claude"], agent.composeServices["claude"], agent.healthURLs["claude"], agent.modes["claude"], agent.testContext("claude"))
	}
	for _, lookalike := range []string{"Claude", "CLAUDE", "claude-runner", "claudes"} {
		if recorder := postService(agent, "/agent/ci", lookalike); recorder.Code != 422 || !strings.Contains(recorder.Body.String(), "unknown_service") {
			t.Fatalf("%q 는 허용 목록 밖이다: %d %s", lookalike, recorder.Code, recorder.Body.String())
		}
	}
}

func TestClaudeIntegrationRunsTestThenBuildsRunnerImage(t *testing.T) {
	recorded := &recordedCommands{runningImage: "claude-runner:" + runningSha}
	recorder := postService(newClaudeAgent(t, recorded), "/agent/ci", "claude")
	if recorder.Code != 200 {
		t.Fatalf("CI 성공이어야 한다: %d %s", recorder.Code, recorder.Body.String())
	}
	want := []string{
		"git -C /srv/claude rev-parse HEAD",
		"docker compose ps -q claude-runner",
		"docker inspect --format {{.Config.Image}} container-1",
		"git -C /srv/claude fetch --depth=50 origin main",
		"git -C /srv/claude reset --hard " + requestSha,
		"docker build --target test .",
		"docker compose build claude-runner",
	}
	if strings.Join(recorded.lines, "\n") != strings.Join(want, "\n") {
		t.Fatalf("claude 의 CI 명령이 다르다:\n%s", strings.Join(recorded.lines, "\n"))
	}
}

func TestClaudeDeliveryReplacesSingleContainerAndChecksHealthOn15200(t *testing.T) {
	recorded := &recordedCommands{runningImage: "claude-runner:" + requestSha, imageListing: strings.Join([]string{
		"2026-11-27 16:00:00 +0900 KST\t" + requestSha,
		"2026-11-27 15:00:00 +0900 KST\taaaaaaa5", "2026-11-27 14:00:00 +0900 KST\taaaaaaa4", "2026-11-27 13:00:00 +0900 KST\taaaaaaa3",
		"2026-11-27 12:00:00 +0900 KST\taaaaaaa2", "2026-11-27 11:00:00 +0900 KST\taaaaaaa1",
	}, "\n")}
	agent := newClaudeAgent(t, recorded)
	checked := []string{}
	agent.waitHealthy = func(healthURL string, _ time.Duration) error { checked = append(checked, healthURL); return nil }
	recorder := postService(agent, "/agent/cd", "claude")
	if recorder.Code != 200 {
		t.Fatalf("CD 성공이어야 한다: %d %s", recorder.Code, recorder.Body.String())
	}
	if recorded.lines[0] != "docker compose up -d --no-build --no-deps claude-runner" || !strings.Contains(strings.Join(recorded.environments[0], " "), "IMAGE_TAG="+requestSha) {
		t.Fatalf("단일 컨테이너 교체가 아니다: %s %v", recorded.lines[0], recorded.environments[0])
	}
	if len(checked) != 1 || checked[0] != claudeHealthURL || !strings.HasSuffix(checked[0], ":15200/health") {
		t.Fatalf("헬스는 15200 의 /health 를 본다: %v", checked)
	}
	if recorded.has("upstream") || recorded.has("nginx") || recorded.has("claude-runner-blue") {
		t.Fatalf("두 자리 방식의 명령이 섞였다:\n%s", strings.Join(recorded.lines, "\n"))
	}
	if !recorded.has("docker rmi claude-runner:aaaaaaa1") {
		t.Fatalf("성공 뒤 오래된 판 이미지를 지운다(최근 5개):\n%s", strings.Join(recorded.lines, "\n"))
	}
}

func TestClaudeUnhealthyWithoutTokenFailsAndKeepsImages(t *testing.T) {
	recorded := &recordedCommands{runningImage: "claude-runner:" + requestSha}
	agent := newClaudeAgent(t, recorded)
	agent.waitHealthy = func(string, time.Duration) error { return errors.New("health 미도달: status 503") }
	recorder := postService(agent, "/agent/cd", "claude")
	if recorder.Code != 500 || !strings.Contains(recorder.Body.String(), `"code":"cd_health"`) || !strings.Contains(recorder.Body.String(), "503") {
		t.Fatalf("토큰이 없어 503 이면 cd_health 로 실패한다: %d %s", recorder.Code, recorder.Body.String())
	}
	if recorded.has("docker rmi") {
		t.Fatal("헬스 실패인데 이전 이미지를 지웠다")
	}
}

func TestHealthCheckTreats503AsNotHealthy(t *testing.T) {
	unhealthy := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusServiceUnavailable) // 실행 자리: 토큰이 없으면 503
	}))
	defer unhealthy.Close()
	if err := waitHealthy(unhealthy.URL+"/health", 100*time.Millisecond); err == nil || !strings.Contains(err.Error(), "503") {
		t.Fatalf("503 을 성공으로 쳤다: %v", err)
	}
}

func TestClaudeFailureOutputMasksSubscriptionToken(t *testing.T) {
	token := "sk-" + "ant-oat01-" + strings.Repeat("Zq81", 2) + strings.Repeat("aB3", 25)
	failing := &scriptedCommands{
		outputs:  map[string]string{"--target test": "loading env\nCLAUDE_CODE_OAUTH_TOKEN=" + token + "\nFAILED tests/test_runs.py"},
		failures: map[string]bool{"--target test": true},
	}
	agent := newClaudeAgent(t, &recordedCommands{})
	agent.run = failing.run
	var recorder *httptest.ResponseRecorder
	logged := captureStandardOutput(t, func() {
		mux := http.NewServeMux()
		agent.Register(mux)
		request := httptest.NewRequest("POST", "/agent/ci", strings.NewReader(`{"service":"claude","commit_sha":"`+requestSha+`","request_id":"gh-claude-2"}`))
		request.Header.Set(shared.SecretHeader, "test-value")
		request.Header.Set(shared.FailureOutputHeader, shared.FailureOutputVersion)
		recorder = httptest.NewRecorder()
		mux.ServeHTTP(recorder, request)
	})
	if recorder.Code != 500 || !strings.Contains(recorder.Body.String(), `"code":"ci_test"`) {
		t.Fatalf("테스트 실패는 ci_test: %d %s", recorder.Code, recorder.Body.String())
	}
	for where, text := range map[string]string{"실패 봉투(detail · output)": recorder.Body.String(), "agent 로그": logged} {
		if strings.Contains(text, "Zq81") || strings.Contains(text, "aB3aB3aB3") {
			t.Fatalf("%s 에 구독 토큰이 남았다:\n%s", where, text)
		}
	}
	if !strings.Contains(recorder.Body.String(), "CLAUDE_CODE_OAUTH_TOKEN=[가림:") {
		t.Fatalf("토큰 줄은 이름을 남기고 값을 가린다: %s", recorder.Body.String())
	}
}
