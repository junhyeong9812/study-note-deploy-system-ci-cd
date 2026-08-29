// Package agent — compose pull/up 실행. 서비스→디렉토리 고정 매핑(allowlist) 밖은 실행 불가.
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"regexp"
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
	// 서비스 → compose 서비스명. env DEPLOY_SVC_<SVC> — 대상만 pull/up (실측: 전체 pull은
	// ollama 등 무관 이미지를 GB 단위로 받다 타임아웃 + 무관 컨테이너 재생성 위험)
	composeServices map[string]string
	busy        sync.Mutex // 한 번에 하나의 배포만 (호스트 자원 보호)
}

var validTag = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`) // 셸 주입 차단

func New(logger *shared.Logger) *Agent {
	directories := map[string]string{}
	composeServices := map[string]string{}
	for _, service := range []string{"front", "backend", "llm"} {
		if directory := os.Getenv("DEPLOY_DIR_" + strings.ToUpper(service)); directory != "" {
			directories[service] = directory
		}
		if composeService := os.Getenv("DEPLOY_SVC_" + strings.ToUpper(service)); composeService != "" {
			composeServices[service] = composeService
		}
	}
	return &Agent{secret: os.Getenv("DEPLOY_SECRET"), logger: logger,
		directories: directories, composeServices: composeServices}
}

func (agent *Agent) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /agent/deploy", agent.handleDeploy)
}

func (agent *Agent) handleDeploy(writer http.ResponseWriter, request *http.Request) {
	if !shared.SecretMatches(request, agent.secret) {
		shared.WriteFail(writer, http.StatusUnauthorized, "unauthorized", "")
		return
	}
	body, _ := io.ReadAll(io.LimitReader(request.Body, 4096))
	var payload struct {
		Service   string `json:"service"`
		ImageTag  string `json:"image_tag"`
		RequestID string `json:"request_id"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		shared.WriteFail(writer, http.StatusUnprocessableEntity, "invalid_request", "bad json")
		return
	}
	requestID := shared.AcceptOrIssue(payload.RequestID)
	directory, allowed := agent.directories[payload.Service]
	if !allowed {
		shared.WriteFail(writer, http.StatusUnprocessableEntity, "unknown_service", payload.Service)
		return
	}
	if !validTag.MatchString(payload.ImageTag) {
		shared.WriteFail(writer, http.StatusUnprocessableEntity, "invalid_tag", "")
		return
	}
	if !agent.busy.TryLock() {
		shared.WriteFail(writer, http.StatusConflict, "deploy_in_progress", "")
		return
	}
	defer agent.busy.Unlock()

	startedAt := time.Now()
	agent.logger.Log(requestID, fmt.Sprintf("agent deploy start %s tag=%s", payload.Service, payload.ImageTag), "info")
	composeService := agent.composeServices[payload.Service] // ""이면 전체 (비권장 — env로 지정)
	output, err := agent.composeDeploy(directory, payload.ImageTag, composeService)
	tookMs := time.Since(startedAt).Milliseconds()
	if err != nil {
		agent.logger.Log(requestID, fmt.Sprintf("agent deploy failed %s: %s", payload.Service, truncate(output, 200)), "error")
		shared.WriteFail(writer, http.StatusInternalServerError, "deploy_failed", truncate(output, 500))
		return
	}
	agent.logger.Log(requestID, fmt.Sprintf("agent deploy ok %s tag=%s %dms", payload.Service, payload.ImageTag, tookMs), "info")
	shared.WriteOK(writer, map[string]any{"took_ms": tookMs, "output": truncate(output, 500)})
}

// composeDeploy — allowlist 디렉토리에서만, 고정 인자만. TAG는 compose가 ${TAG}로 소비.
// 순서: ① git 동기화(디렉토리가 clone이면 — compose·설정 변경 전달, .env는 미추적이라 보존)
//       ② 대상 서비스만 pull ③ 대상 서비스만 up (무관 이미지 갱신·재생성 차단)
// (실측 2026-08-29: 이미지만 배포하면 compose 변경이 호스트에 영원히 안 닿는다)
func (agent *Agent) composeDeploy(directory, imageTag, composeService string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	commands := [][]string{}
	if _, err := os.Stat(directory + "/.git"); err == nil {
		commands = append(commands,
			[]string{"git", "-C", directory, "fetch", "--depth=1", "origin", "main"},
			[]string{"git", "-C", directory, "reset", "--hard", "origin/main"},
		)
	}
	pullArguments := []string{"docker", "compose", "pull"}
	upArguments := []string{"docker", "compose", "up", "-d"}
	if composeService != "" {
		pullArguments = append(pullArguments, composeService)
		upArguments = append(upArguments, composeService)
	}
	commands = append(commands, pullArguments, upArguments)
	var combined strings.Builder
	for _, arguments := range commands {
		command := exec.CommandContext(ctx, arguments[0], arguments[1:]...)
		command.Dir = directory
		command.Env = append(os.Environ(), "TAG="+imageTag)
		output, err := command.CombinedOutput()
		combined.Write(output)
		if err != nil {
			return combined.String(), err
		}
	}
	return combined.String(), nil
}

func truncate(input string, limit int) string {
	if len(input) <= limit {
		return input
	}
	return input[:limit]
}
