// Package master — 배포 접수·서비스 allowlist·에이전트 라우팅. (.9:15000)
package master

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/junhyeong9812/study-note-deploy-system-ci-cd/internal/shared"
)

type DeployRequest struct {
	Service   string `json:"service"`
	CommitSha string `json:"commit_sha"`
	RequestID string `json:"request_id"`
}

type deployRecord struct {
	Service   string `json:"service"`
	CommitSha string `json:"commit_sha"`
	RequestID string `json:"request_id"`
	Outcome   string `json:"outcome"`
	Detail    string `json:"detail,omitempty"`
	At        string `json:"at"`
	TookMs    int64  `json:"took_ms"`
}

type Master struct {
	secret  string
	logger  *shared.Logger
	agents  map[string]string // service → agent base URL (env — repo에 IP 금지)
	mutex   sync.Mutex
	history []deployRecord // 최근 배포 이력 (메모리 — 롤백 참고용 태그 기록)
}

func New(logger *shared.Logger) *Master {
	agents := map[string]string{}
	// AGENT_URL_FRONT=http://<host>:15001 형식 — 서비스 allowlist는 이 고정 열거가 정본
	for _, service := range []string{"front", "backend", "llm"} {
		if agentURL := os.Getenv("AGENT_URL_" + strings.ToUpper(service)); agentURL != "" {
			agents[service] = agentURL
		}
	}
	return &Master{secret: os.Getenv("DEPLOY_SECRET"), logger: logger, agents: agents}
}

func (master *Master) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /deploy", master.handleDeploy)
	mux.HandleFunc("GET /status", master.handleStatus)
}

func (master *Master) handleDeploy(writer http.ResponseWriter, request *http.Request) {
	if !shared.SecretMatches(request, master.secret) {
		shared.WriteFail(writer, http.StatusUnauthorized, "unauthorized", "")
		return
	}
	body, err := io.ReadAll(io.LimitReader(request.Body, 4096))
	if err != nil {
		shared.WriteFail(writer, http.StatusUnprocessableEntity, "invalid_request", "body read")
		return
	}
	var deploy DeployRequest
	if err := json.Unmarshal(body, &deploy); err != nil || deploy.Service == "" || deploy.CommitSha == "" {
		shared.WriteFail(writer, http.StatusUnprocessableEntity, "invalid_request", "need service·commit_sha")
		return
	}
	requestID := shared.AcceptOrIssue(deploy.RequestID)
	agentURL, allowed := master.agents[deploy.Service]
	if !allowed {
		master.logger.Log(requestID, fmt.Sprintf("deploy rejected: unknown service %q", deploy.Service), "warning")
		shared.WriteFail(writer, http.StatusUnprocessableEntity, "unknown_service", deploy.Service)
		return
	}
	master.logger.Log(requestID, fmt.Sprintf("deploy accepted %s sha=%s", deploy.Service, deploy.CommitSha), "info")

	go master.dispatch(requestID, deploy, agentURL) // 접수 즉시 202 — Actions를 기다리게 하지 않는다
	shared.WriteAccepted(writer, map[string]any{"request_id": requestID, "service": deploy.Service})
}

func (master *Master) dispatch(requestID string, deploy DeployRequest, agentURL string) {
	startedAt := time.Now()
	payload, _ := json.Marshal(map[string]string{
		"service": deploy.Service, "commit_sha": deploy.CommitSha, "request_id": requestID,
	})
	client := &http.Client{Timeout: 5 * time.Minute} // pull에 수 분 걸릴 수 있다
	agentRequest, _ := http.NewRequest(http.MethodPost, agentURL+"/agent/deploy", bytes.NewReader(payload))
	agentRequest.Header.Set("Content-Type", "application/json")
	agentRequest.Header.Set(shared.SecretHeader, master.secret)

	outcome, detail := "ok", ""
	response, err := client.Do(agentRequest)
	if err != nil {
		outcome, detail = "agent_unreachable", err.Error()
	} else {
		defer response.Body.Close()
		responseBody, _ := io.ReadAll(io.LimitReader(response.Body, 8192))
		if response.StatusCode != http.StatusOK {
			outcome = "agent_failed"
		}
		detail = strings.TrimSpace(string(responseBody))
	}
	tookMs := time.Since(startedAt).Milliseconds()
	level := "info"
	if outcome != "ok" {
		level = "error"
	}
	master.logger.Log(requestID,
		fmt.Sprintf("deploy %s %s sha=%s %dms %s", outcome, deploy.Service, deploy.CommitSha, tookMs, truncate(detail, 200)), level)

	master.mutex.Lock()
	defer master.mutex.Unlock()
	master.history = append(master.history, deployRecord{
		Service: deploy.Service, CommitSha: deploy.CommitSha, RequestID: requestID,
		Outcome: outcome, Detail: truncate(detail, 300),
		At: startedAt.UTC().Format(time.RFC3339), TookMs: tookMs,
	})
	if len(master.history) > 50 {
		master.history = master.history[len(master.history)-50:]
	}
}

func (master *Master) handleStatus(writer http.ResponseWriter, request *http.Request) {
	if !shared.SecretMatches(request, master.secret) {
		shared.WriteFail(writer, http.StatusUnauthorized, "unauthorized", "")
		return
	}
	master.mutex.Lock()
	defer master.mutex.Unlock()
	shared.WriteOK(writer, map[string]any{"services": keys(master.agents), "history": master.history})
}

func keys(input map[string]string) []string {
	result := make([]string, 0, len(input))
	for key := range input {
		result = append(result, key)
	}
	return result
}

func truncate(input string, limit int) string {
	if len(input) <= limit {
		return input
	}
	return input[:limit]
}
