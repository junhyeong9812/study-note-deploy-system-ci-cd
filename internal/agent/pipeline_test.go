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
	previousSha = "1111111111111111111111111111111111111111"
	requestSha  = "2222222222222222222222222222222222222222"
)

// recordedCommands — 실제 git·docker 대신 명령을 기록하고, failOn에 걸리는 명령만 실패시킨다.
type recordedCommands struct {
	lines        []string
	environments [][]string
	failOn       string
	imageListing string
}

func (recorded *recordedCommands) run(_ context.Context, _ string, environment []string, arguments ...string) (string, error) {
	line := strings.Join(arguments, " ")
	recorded.lines = append(recorded.lines, line)
	recorded.environments = append(recorded.environments, environment)
	if strings.Contains(line, "rev-parse") {
		return previousSha + "\n", nil
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
	t.Setenv("DEPLOY_IMAGE_LLM", "llm-wrapper")
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
	recorded := &recordedCommands{}
	recorder := post(newPipelineAgent(t, recorded), "/agent/ci")
	if recorder.Code != 200 {
		t.Fatalf("CI 성공이어야 하는데 %d %s", recorder.Code, recorder.Body.String())
	}
	want := []string{
		"git -C /srv/llm rev-parse HEAD",
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
	buildEnvironment := strings.Join(recorded.environments[4], " ")
	if !strings.Contains(buildEnvironment, "IMAGE_TAG="+requestSha) {
		t.Fatalf("이미지 빌드에 sha 태그가 전달되지 않았다: %q", buildEnvironment)
	}
}

func TestContinuousIntegrationTestFailureRestoresSourceAndSkipsBuild(t *testing.T) {
	recorded := &recordedCommands{failOn: "--target test"}
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
	if last := recorded.lines[len(recorded.lines)-1]; last != "git -C /srv/llm reset --hard "+previousSha {
		t.Fatalf("소스를 직전 커밋으로 되돌리지 않았다: %s", last)
	}
}

func TestContinuousIntegrationBuildFailureRestoresSource(t *testing.T) {
	recorded := &recordedCommands{failOn: "compose build"}
	recorder := post(newPipelineAgent(t, recorded), "/agent/ci")
	if recorder.Code != 500 || !strings.Contains(recorder.Body.String(), `"code":"ci_build"`) {
		t.Fatalf("빌드 실패는 500 ci_build여야 한다: %d %s", recorder.Code, recorder.Body.String())
	}
	if last := recorded.lines[len(recorded.lines)-1]; last != "git -C /srv/llm reset --hard "+previousSha {
		t.Fatalf("소스를 직전 커밋으로 되돌리지 않았다: %s", last)
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
	recorded := &recordedCommands{}
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
	if !recorded.has("docker tag llm-wrapper:" + requestSha + " llm-wrapper:current") {
		t.Fatal("배포한 이미지에 current 꼬리표를 붙이지 않았다")
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
	if recorded.has("docker tag") {
		t.Fatal("헬스 실패인데 current 꼬리표를 옮겼다")
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

func TestStaleImageTagsKeepsNewestFiveAndDeployed(t *testing.T) {
	listing := strings.Join([]string{
		"2026-11-19 10:00:00 +0900 KST\taaaaaaa1",
		"2026-11-19 16:00:00 +0900 KST\taaaaaaa7",
		"2026-11-19 16:00:00 +0900 KST\tcurrent",
		"2026-11-19 15:00:00 +0900 KST\taaaaaaa6",
		"2026-11-19 14:00:00 +0900 KST\taaaaaaa5",
		"2026-11-19 13:00:00 +0900 KST\taaaaaaa4",
		"2026-11-19 12:00:00 +0900 KST\taaaaaaa3",
		"2026-11-19 11:00:00 +0900 KST\taaaaaaa2",
		"2026-11-19 09:00:00 +0900 KST\t<none>",
	}, "\n")
	stale := staleImageTags(listing, "aaaaaaa1") // 방금 되돌린 오래된 버전은 지우지 않는다
	if strings.Join(stale, ",") != "aaaaaaa2" {
		t.Fatalf("지울 태그가 다르다: %v", stale)
	}
}
