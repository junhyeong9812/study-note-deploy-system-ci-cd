// Package agent — compose pull/up 실행. 서비스→디렉토리 고정 매핑(allowlist) 밖은 실행 불가.
package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/junhyeong9812/study-note-deploy-system-ci-cd/internal/shared"
)

type Agent struct {
	secret string
	logger *shared.Logger
	// 서비스 → compose 디렉토리. env DEPLOY_DIR_<SVC> — 이 고정 열거가 실행 가능한 전부다.
	directories map[string]string
	// 서비스 → compose 서비스명. env DEPLOY_SVC_<SVC> — 대상만 build/up
	composeServices map[string]string
	// 서비스 → 배포 후 헬스 URL. env DEPLOY_HEALTH_<SVC> — compose 성공 ≠ 서비스 정상 (리뷰 F7)
	healthURLs map[string]string
	// 서비스 → 테스트 빌드 경로(배포 디렉토리 기준). env DEPLOY_TEST_CONTEXT_<SVC> — 없으면 "."
	testContexts map[string]string
	// 서비스 → 전환 방식. env DEPLOY_MODE_<SVC>=bluegreen 이면 두 자리 + nginx 전환, 아니면 단일 컨테이너 교체
	modes map[string]string
	// 서비스 → 자리 → 헬스 URL(127.0.0.1 포트). env DEPLOY_HEALTH_<SVC>_BLUE·_GREEN
	slotHealthURLs map[string]map[string]string
	// 서비스 → 자리 → 정리 시작 알림 URL(선택). env DEPLOY_DRAIN_<SVC>_BLUE·_GREEN
	slotDrainURLs map[string]map[string]string
	busy          sync.Mutex // 한 번에 하나의 작업만 (호스트 자원 보호)
	run           commandRunner
	waitHealthy   func(healthURL string, budget time.Duration) error
	probeSlot     func(healthURL string) (slot string, err error)
	notifyDrain   func(drainURL string) error

	probeInterval     time.Duration
	probeBudget       time.Duration
	drainPollInterval time.Duration
	drainLimit        time.Duration       // 구 버전에 남은 연결을 기다리는 상한
	drainSettle       time.Duration       // reload 뒤 nginx가 구 worker를 종료 중으로 바꿀 때까지 주는 시간
	drainWaitMargin   time.Duration       // 다음 CD가 앞선 정리를 기다릴 때 상한에 더하는 여유(자리 정지 시간)
	alerts            *shared.AlertSender // 두 자리가 남았을 수 있는 상황을 알림 서버에 알린다
	drainMutex        sync.Mutex
	draining          map[string]chan struct{} // 서비스 → 진행 중인 정리(끝나면 닫힌다)
	background        sync.WaitGroup

	// git 조회(git_read.go) — 테스트가 바꾸는 이음매. gitProgram 은 실행할 git(기본 "git"), gitTimeout 은 조회 하나의 기한.
	gitProgram   string
	gitTimeout   time.Duration
	gitReadSlots chan struct{}                          // 동시 조회 자리(gitReadConcurrency 개) — 비어 있지 않으면 기다리지 않고 429
	redact       func(raw string) shared.CapturedOutput // 실패 글의 가림 — 테스트가 바꾼다(가림의 실패)
}

func New(logger *shared.Logger) *Agent {
	directories := map[string]string{}
	composeServices := map[string]string{}
	healthURLs := map[string]string{}
	testContexts := map[string]string{}
	modes := map[string]string{}
	slotHealthURLs := map[string]map[string]string{}
	slotDrainURLs := map[string]map[string]string{}
	for _, service := range []string{"front", "backend", "llm", "alert", "observe", "admin"} {
		if directory := os.Getenv("DEPLOY_DIR_" + strings.ToUpper(service)); directory != "" {
			directories[service] = directory
		}
		if composeService := os.Getenv("DEPLOY_SVC_" + strings.ToUpper(service)); composeService != "" {
			composeServices[service] = composeService
		}
		if healthURL := os.Getenv("DEPLOY_HEALTH_" + strings.ToUpper(service)); healthURL != "" {
			healthURLs[service] = healthURL
		}
		if testContext := os.Getenv("DEPLOY_TEST_CONTEXT_" + strings.ToUpper(service)); testContext != "" {
			testContexts[service] = testContext
		}
		modes[service] = os.Getenv("DEPLOY_MODE_" + strings.ToUpper(service))
		slotHealthURLs[service] = map[string]string{}
		slotDrainURLs[service] = map[string]string{}
		for _, slot := range []string{slotBlue, slotGreen} {
			suffix := strings.ToUpper(service) + "_" + strings.ToUpper(slot)
			slotHealthURLs[service][slot] = os.Getenv("DEPLOY_HEALTH_" + suffix)
			slotDrainURLs[service][slot] = os.Getenv("DEPLOY_DRAIN_" + suffix)
		}
	}
	return &Agent{secret: os.Getenv("DEPLOY_SECRET"), logger: logger,
		directories: directories, composeServices: composeServices, healthURLs: healthURLs,
		testContexts: testContexts, run: runCommand, waitHealthy: waitHealthy,
		modes: modes, slotHealthURLs: slotHealthURLs, slotDrainURLs: slotDrainURLs,
		probeSlot: probeSlot, notifyDrain: notifyDrain,
		probeInterval: time.Second, probeBudget: 30 * time.Second,
		drainPollInterval: 2 * time.Second, drainLimit: 3 * time.Minute, drainWaitMargin: 70 * time.Second, drainSettle: 2 * time.Second,
		draining: map[string]chan struct{}{}, alerts: shared.NewAlertSender("ci-cd-agent", logger),
		gitProgram: "git", gitTimeout: gitReadTimeout, gitReadSlots: make(chan struct{}, gitReadConcurrency), redact: shared.CaptureOutput}
}

func (agent *Agent) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /agent/ci", agent.handleStage("ci", agent.integrate))
	mux.HandleFunc("POST /agent/cd", agent.handleStage("cd", agent.deliver))
	mux.HandleFunc("GET /agent/git/commits", agent.handleGitCommits)
	mux.HandleFunc("GET /agent/git/commit", agent.handleGitCommit)
}

// handleStage — CI·CD 공통 접수: 인증 → 서비스·sha 검증 → 한 번에 하나 → 실행 → 실패 단계를 코드로 응답.
func (agent *Agent) handleStage(name string, execute func(service, commitSha string) (string, error)) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		if !shared.SecretMatches(request, agent.secret) {
			shared.WriteFail(writer, http.StatusUnauthorized, "unauthorized", "")
			return
		}
		body, _ := io.ReadAll(io.LimitReader(request.Body, 4096))
		var payload struct {
			Service   string `json:"service"`
			CommitSha string `json:"commit_sha"`
			RequestID string `json:"request_id"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			shared.WriteFail(writer, http.StatusUnprocessableEntity, "invalid_request", "bad json")
			return
		}
		requestID := shared.AcceptOrIssue(payload.RequestID)
		if _, allowed := agent.directories[payload.Service]; !allowed {
			shared.WriteFail(writer, http.StatusUnprocessableEntity, "unknown_service", payload.Service)
			return
		}
		if agent.composeServices[payload.Service] == "" { // 대상이 없으면 compose 전체가 대상이 된다 — 거절
			shared.WriteFail(writer, http.StatusUnprocessableEntity, "service_not_configured", "DEPLOY_SVC_"+strings.ToUpper(payload.Service))
			return
		}
		if !shared.ValidCommitSha(payload.CommitSha) {
			shared.WriteFail(writer, http.StatusUnprocessableEntity, "invalid_sha", "")
			return
		}
		if !agent.busy.TryLock() {
			shared.WriteFail(writer, http.StatusConflict, "agent_busy", "")
			return
		}
		defer agent.busy.Unlock()

		startedAt := time.Now()
		agent.logger.Log(requestID, fmt.Sprintf("agent %s start %s sha=%s", name, payload.Service, payload.CommitSha), "info")
		output, err := execute(payload.Service, payload.CommitSha)
		tookMs := time.Since(startedAt).Milliseconds()
		if err != nil {
			stage, detail := name+"_failed", output
			var staged *stageError
			if errors.As(err, &staged) {
				stage, detail = staged.stage, staged.output
			}
			// 실패 글은 한 번 가리고, detail · 로그의 끝 · 청한 경우의 output 은 모두 그 가린 글에서 뗀다(청하지 않아도 가린다)
			masked, redacted := shared.RedactSafely(agent.redact, detail)
			detailTail, logTail := shared.DetailRedactionFailed, shared.DetailRedactionFailed
			if redacted {
				detailTail, logTail = shared.CutTail(masked.Text, outputTailLimit), shared.CutTail(masked.Text, 200)
			}
			agent.logger.Log(requestID, fmt.Sprintf("agent %s failed %s stage=%s %dms: %s", name, payload.Service, stage, tookMs, logTail), "error")
			// 청한 master 에게만 output 을 싣는다 — 옛 master(16KB 까지 읽음)에게는 칸이 지금과 같은 봉투
			if redacted && request.Header.Get(shared.FailureOutputHeader) == shared.FailureOutputVersion {
				shared.WriteFailWithOutput(writer, http.StatusInternalServerError, stage, detailTail, masked)
				return
			}
			shared.WriteFail(writer, http.StatusInternalServerError, stage, detailTail)
			return
		}
		agent.logger.Log(requestID, fmt.Sprintf("agent %s ok %s sha=%s %dms", name, payload.Service, payload.CommitSha, tookMs), "info")
		shared.WriteOK(writer, map[string]any{"took_ms": tookMs, "output": tail(output, outputTailLimit)})
	}
}
