package agent

// 기존 경로 무변경 — 수집기가 도는 중에도 /agent/ci · /agent/cd · /agent/git/* 가 같은 상태 · 같은 봉투로 답한다.
// 기준: requirement-spec.md(2026-11-24 resource-metrics) §2.3 "기존 동작 무변경" · §4. 단언은 existing_routes_test.go 와 같다.
// /health 는 cmd/server/main.go 의 경로라 이 패키지에서 부를 수 없다(main 의 배선은 서버 확인에서 본다).

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/junhyeong9812/study-note-deploy-system-ci-cd/internal/shared"
)

// keepTicking — 테스트가 끝날 때까지 수집기에 주기를 계속 보낸다(앞 수집이 돌면 건너뛰어진다).
func keepTicking(t *testing.T, harness *resourceHarness) {
	t.Helper()
	harness.start()
	stop := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		for {
			select {
			case harness.ticks <- time.Now():
			case <-stop:
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()
	t.Cleanup(func() {
		close(stop)
		<-stopped
	})
}

func TestExistingRoutesAnswerSameWhileResourceCollectorRuns(t *testing.T) {
	harness := newContainerHarness(t)
	harness.docker.set(func(docker *fakeDocker) { docker.stallStats = true }) // 수집이 도는 시간을 늘린다
	harness.collector.dockerDeadline = 20 * time.Millisecond
	keepTicking(t, harness)
	harness.waitStats("수집기가 돈다", func(stats ResourceCollectorStats) bool { return stats.Sent >= 1 })
	agent := harness.agent

	for _, path := range []string{"/agent/ci", "/agent/cd"} {
		if recorder := post(agent, path); recorder.Code != 200 || strings.Join(stageDataKeys(t, recorder), ",") != "output,took_ms" {
			t.Fatalf("%s 성공 답이 바뀌었다: %d %s", path, recorder.Code, recorder.Body.String())
		}

		agent.busy.Lock()
		if recorder := post(agent, path); recorder.Code != 409 || recorder.Body.String() != `{"error":{"code":"agent_busy"},"success":false}`+"\n" {
			t.Fatalf("%s 작업 중 답이 바뀌었다: %d %q", path, recorder.Code, recorder.Body.String())
		}
		agent.busy.Unlock()

		mux := http.NewServeMux()
		agent.Register(mux)
		unauthorized := httptest.NewRecorder()
		mux.ServeHTTP(unauthorized, httptest.NewRequest("POST", path, strings.NewReader(`{}`)))
		if unauthorized.Code != 401 || unauthorized.Body.String() != `{"error":{"code":"unauthorized"},"success":false}`+"\n" {
			t.Fatalf("%s 무인증 답이 바뀌었다: %d %q", path, unauthorized.Code, unauthorized.Body.String())
		}
		getRecorder := httptest.NewRecorder()
		getRequest := httptest.NewRequest("GET", path, nil)
		getRequest.Header.Set(shared.SecretHeader, "test-value")
		mux.ServeHTTP(getRecorder, getRequest)
		if getRecorder.Code != http.StatusMethodNotAllowed {
			t.Fatalf("GET %s 는 405 여야 한다: %d", path, getRecorder.Code)
		}
	}

	repository, shas := linearRepository(t, 3)
	agent.directories["llm"] = repository.directory
	commits := requireGitSuccess[GitCommitRange](t, gitGet(agent, commitsTarget("llm", shas[0], shas[2])))
	if len(commits.Commits) != 2 || commits.Relation != gitRelationAncestor {
		t.Fatalf("git 목록이 바뀌었다: %+v", commits)
	}
	detail := requireGitSuccess[GitCommitDetail](t, gitGet(agent, commitTarget("llm", shas[2])))
	if detail.Sha != shas[2] {
		t.Fatalf("git 상세가 바뀌었다: %+v", detail.GitCommit)
	}
	requireGitFailure(t, gitCall(agent, http.MethodGet, commitTarget("llm", shas[2]), ""), 401, "unauthorized")
	requireGitFailure(t, gitGet(agent, commitTarget("nope", shas[2])), 422, "invalid_request")

	// 수집기는 경로를 더하지 않는다
	mux := http.NewServeMux()
	agent.Register(mux)
	for _, path := range []string{"/agent/metrics", "/metrics"} {
		recorder := httptest.NewRecorder()
		mux.ServeHTTP(recorder, httptest.NewRequest("GET", path, nil))
		if recorder.Code != http.StatusNotFound {
			t.Fatalf("%s 는 없는 경로여야 한다: %d", path, recorder.Code)
		}
	}
	if stats := harness.collector.Stats(); stats.Sent < 1 || stats.Panics != 0 {
		t.Fatalf("경로를 부르는 동안 수집기가 돌았어야 한다: %+v", stats)
	}
}
