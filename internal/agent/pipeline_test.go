package agent

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/junhyeong9812/study-note-deploy-system-ci-cd/internal/shared"
)

const (
	headSha    = "1111111111111111111111111111111111111111" // 디렉토리 HEAD (앞선 CI가 남긴 커밋일 수 있다)
	requestSha = "2222222222222222222222222222222222222222"
	runningSha = "3333333333333333333333333333333333333333" // 실행 중인 컨테이너의 이미지 태그 = 운영 버전
)

// recordedCommands — 실제 git·docker 대신 명령을 기록하고, failOn에 걸리는 명령만 실패시킨다.
type recordedCommands struct {
	lines        []string
	environments [][]string
	failOn       string
	imageListing string
	runningImage string // docker inspect 가 돌려줄 "이름:태그" — 비면 실행 중인 컨테이너 없음
}

func (recorded *recordedCommands) run(_ context.Context, _ string, environment []string, arguments ...string) (string, error) {
	line := strings.Join(arguments, " ")
	recorded.lines = append(recorded.lines, line)
	recorded.environments = append(recorded.environments, environment)
	if strings.Contains(line, "rev-parse") {
		return headSha + "\n", nil
	}
	if strings.HasPrefix(line, "docker compose ps -q") {
		if recorded.runningImage == "" {
			return "", nil
		}
		return "container-1\n", nil
	}
	if strings.HasPrefix(line, "docker inspect") {
		return recorded.runningImage + "\n", nil
	}
	if strings.HasPrefix(line, "docker images") {
		return recorded.imageListing, nil
	}
	if recorded.failOn != "" && strings.Contains(line, recorded.failOn) {
		return "boom output", errors.New("exit status 1")
	}
	return "", nil
}

func (recorded *recordedCommands) has(fragment string) bool {
	for _, line := range recorded.lines {
		if strings.Contains(line, fragment) {
			return true
		}
	}
	return false
}

func newPipelineAgent(t *testing.T, recorded *recordedCommands) *Agent {
	t.Setenv("DEPLOY_SECRET", "test-value")
	t.Setenv("DEPLOY_DIR_LLM", "/srv/llm")
	t.Setenv("DEPLOY_SVC_LLM", "wrapper")
	t.Setenv("DEPLOY_TEST_CONTEXT_LLM", "wrapper")
	t.Setenv("DEPLOY_HEALTH_LLM", "http://127.0.0.1:1/health")
	agent := New(shared.NewLogger())
	agent.run = recorded.run
	agent.waitHealthy = func(string, time.Duration) error { return nil }
	return agent
}

func post(agent *Agent, path string) *httptest.ResponseRecorder {
	mux := http.NewServeMux()
	agent.Register(mux)
	req := httptest.NewRequest("POST", path, strings.NewReader(`{"service":"llm","commit_sha":"`+requestSha+`","request_id":"gh-1"}`))
	req.Header.Set(shared.SecretHeader, "test-value")
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, req)
	return recorder
}

func TestContinuousIntegrationRunsFetchTestBuildInOrderWithoutTouchingContainers(t *testing.T) {
	recorded := &recordedCommands{runningImage: "llm-wrapper:" + runningSha}
	recorder := post(newPipelineAgent(t, recorded), "/agent/ci")
	if recorder.Code != 200 {
		t.Fatalf("CI 성공이어야 하는데 %d %s", recorder.Code, recorder.Body.String())
	}
	want := []string{
		"git -C /srv/llm rev-parse HEAD",
		"docker compose ps -q wrapper",
		"docker inspect --format {{.Config.Image}} container-1",
		"git -C /srv/llm fetch --depth=50 origin main",
		"git -C /srv/llm reset --hard " + requestSha,
		"docker build --target test wrapper",
		"docker compose build wrapper",
	}
	if strings.Join(recorded.lines, "\n") != strings.Join(want, "\n") {
		t.Fatalf("명령 순서가 다르다:\n%s", strings.Join(recorded.lines, "\n"))
	}
	if recorded.has("compose up") {
		t.Fatal("CI가 컨테이너를 교체했다")
	}
	buildEnvironment := strings.Join(recorded.environments[len(recorded.environments)-1], " ")
	if !strings.Contains(buildEnvironment, "IMAGE_TAG="+requestSha) {
		t.Fatalf("이미지 빌드에 sha 태그가 전달되지 않았다: %q", buildEnvironment)
	}
}

func TestContinuousIntegrationTestFailureRestoresSourceAndSkipsBuild(t *testing.T) {
	// 앞선 배포가 CI만 통과하고 CD에서 실패한 상황: HEAD(1111…)는 운영(3333…)과 다르다
	recorded := &recordedCommands{failOn: "--target test", runningImage: "llm-wrapper:" + runningSha}
	recorder := post(newPipelineAgent(t, recorded), "/agent/ci")
	if recorder.Code != 500 || !strings.Contains(recorder.Body.String(), `"code":"ci_test"`) {
		t.Fatalf("테스트 실패는 500 ci_test여야 한다: %d %s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "boom output") {
		t.Fatalf("실패 출력이 응답에 없다: %s", recorder.Body.String())
	}
	if recorded.has("compose build") || recorded.has("compose up") {
		t.Fatal("테스트 실패 뒤에 빌드·교체가 실행됐다")
	}
	if last := recorded.lines[len(recorded.lines)-1]; last != "git -C /srv/llm reset --hard "+runningSha {
		t.Fatalf("소스를 운영 중인 버전으로 되돌리지 않았다: %s", last)
	}
}

func TestContinuousIntegrationBuildFailureRestoresSource(t *testing.T) {
	recorded := &recordedCommands{failOn: "compose build"}
	recorder := post(newPipelineAgent(t, recorded), "/agent/ci")
	if recorder.Code != 500 || !strings.Contains(recorder.Body.String(), `"code":"ci_build"`) {
		t.Fatalf("빌드 실패는 500 ci_build여야 한다: %d %s", recorder.Code, recorder.Body.String())
	}
	// 실행 중인 컨테이너가 없으면(운영 버전을 알 수 없으면) HEAD가 기준이다
	if last := recorded.lines[len(recorded.lines)-1]; last != "git -C /srv/llm reset --hard "+headSha {
		t.Fatalf("소스를 HEAD로 되돌리지 않았다: %s", last)
	}
}

func TestContinuousIntegrationFetchFailureLeavesSourceAlone(t *testing.T) {
	recorded := &recordedCommands{failOn: "fetch"}
	recorder := post(newPipelineAgent(t, recorded), "/agent/ci")
	if recorder.Code != 500 || !strings.Contains(recorder.Body.String(), `"code":"ci_fetch"`) {
		t.Fatalf("받기 실패는 500 ci_fetch여야 한다: %d %s", recorder.Code, recorder.Body.String())
	}
	if recorded.has("reset --hard") {
		t.Fatal("받기 실패인데 소스를 건드렸다")
	}
}

func TestContinuousDeliveryReplacesOnlyTargetWithoutBuilding(t *testing.T) {
	recorded := &recordedCommands{runningImage: "llm-wrapper:" + requestSha, imageListing: strings.Join([]string{
		"2026-11-19 16:00:00 +0900 KST\t" + requestSha,
		"2026-11-19 15:00:00 +0900 KST\taaaaaaa5",
		"2026-11-19 14:00:00 +0900 KST\taaaaaaa4",
		"2026-11-19 13:00:00 +0900 KST\taaaaaaa3",
		"2026-11-19 12:00:00 +0900 KST\taaaaaaa2",
		"2026-11-19 11:00:00 +0900 KST\taaaaaaa1",
		"2026-11-19 10:00:00 +0900 KST\tlocal",
	}, "\n")}
	recorder := post(newPipelineAgent(t, recorded), "/agent/cd")
	if recorder.Code != 200 {
		t.Fatalf("CD 성공이어야 하는데 %d %s", recorder.Code, recorder.Body.String())
	}
	if recorded.lines[0] != "docker compose up -d --no-build --no-deps wrapper" {
		t.Fatalf("교체 명령이 다르다: %s", recorded.lines[0])
	}
	if !strings.Contains(strings.Join(recorded.environments[0], " "), "IMAGE_TAG="+requestSha) {
		t.Fatal("교체에 sha 태그가 전달되지 않았다")
	}
	if recorded.has("git ") || recorded.has("compose build") || recorded.has("docker build") {
		t.Fatalf("CD가 소스를 받거나 빌드했다:\n%s", strings.Join(recorded.lines, "\n"))
	}
	// 이미지 이름은 설정이 아니라 실행 중인 컨테이너에서 읽는다 — 6번째로 오래된 것만 지운다
	if !recorded.has("docker rmi llm-wrapper:aaaaaaa1") || recorded.has("rmi llm-wrapper:aaaaaaa2") || recorded.has("rmi llm-wrapper:local") {
		t.Fatalf("이미지 정리가 다르다:\n%s", strings.Join(recorded.lines, "\n"))
	}
}

func TestContinuousDeliveryRefusesWithoutHealthCheck(t *testing.T) {
	recorded := &recordedCommands{}
	agent := newPipelineAgent(t, recorded)
	agent.healthURLs = map[string]string{}
	recorder := post(agent, "/agent/cd")
	if recorder.Code != 500 || !strings.Contains(recorder.Body.String(), `"code":"cd_health"`) {
		t.Fatalf("헬스 URL이 없으면 cd_health로 거절해야 한다: %d %s", recorder.Code, recorder.Body.String())
	}
	if recorded.has("compose up") {
		t.Fatal("헬스를 확인할 수 없는데 운영을 교체했다")
	}
}

func TestStageRefusesWhenComposeServiceIsNotConfigured(t *testing.T) {
	for _, path := range []string{"/agent/ci", "/agent/cd"} {
		recorded := &recordedCommands{}
		agent := newPipelineAgent(t, recorded)
		agent.composeServices = map[string]string{}
		recorder := post(agent, path)
		if recorder.Code != 422 || !strings.Contains(recorder.Body.String(), "service_not_configured") {
			t.Fatalf("%s: 대상 미설정은 422여야 한다: %d %s", path, recorder.Code, recorder.Body.String())
		}
		if len(recorded.lines) != 0 {
			t.Fatalf("%s: 대상 없이 명령을 실행했다: %v", path, recorded.lines)
		}
	}
}

func TestContinuousDeliveryUpFailureReportsStage(t *testing.T) {
	recorded := &recordedCommands{failOn: "compose up"}
	recorder := post(newPipelineAgent(t, recorded), "/agent/cd")
	if recorder.Code != 500 || !strings.Contains(recorder.Body.String(), `"code":"cd_up"`) {
		t.Fatalf("교체 실패는 500 cd_up이어야 한다: %d %s", recorder.Code, recorder.Body.String())
	}
}

func TestContinuousDeliveryUnhealthyReportsStage(t *testing.T) {
	recorded := &recordedCommands{}
	agent := newPipelineAgent(t, recorded)
	agent.waitHealthy = func(string, time.Duration) error { return errors.New("health 미도달: status 503") }
	recorder := post(agent, "/agent/cd")
	if recorder.Code != 500 || !strings.Contains(recorder.Body.String(), `"code":"cd_health"`) {
		t.Fatalf("헬스 실패는 500 cd_health여야 한다: %d %s", recorder.Code, recorder.Body.String())
	}
	if recorded.has("docker rmi") {
		t.Fatal("헬스 실패인데 이전 이미지를 지웠다")
	}
}

func TestStageRejectsWhileAnotherRuns(t *testing.T) {
	agent := newPipelineAgent(t, &recordedCommands{})
	agent.busy.Lock()
	defer agent.busy.Unlock()
	if recorder := post(agent, "/agent/ci"); recorder.Code != 409 {
		t.Fatalf("작업 중인데 %d", recorder.Code)
	}
}

func TestStaleImageTagsKeepsFiveIncludingDeployed(t *testing.T) {
	listing := strings.Join([]string{
		"2026-11-19 10:00:00 +0900 KST\taaaaaaa1",
		"2026-11-19 16:00:00 +0900 KST\taaaaaaa7",
		"2026-11-19 16:00:00 +0900 KST\tlocal",
		"2026-11-19 15:00:00 +0900 KST\taaaaaaa6",
		"2026-11-19 14:00:00 +0900 KST\taaaaaaa5",
		"2026-11-19 13:00:00 +0900 KST\taaaaaaa4",
		"2026-11-19 12:00:00 +0900 KST\taaaaaaa3",
		"2026-11-19 11:00:00 +0900 KST\taaaaaaa2",
		"2026-11-19 09:00:00 +0900 KST\t<none>",
	}, "\n")
	// 방금 되돌린 가장 오래된 버전(aaaaaaa1)은 남기고, 그것을 포함해 5개가 되도록 나머지를 지운다
	stale := staleImageTags(listing, "aaaaaaa1")
	if strings.Join(stale, ",") != "aaaaaaa3,aaaaaaa2" {
		t.Fatalf("지울 태그가 다르다: %v", stale)
	}
}
