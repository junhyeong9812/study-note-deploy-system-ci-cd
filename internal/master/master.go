// Package master — 배포 접수·서비스 allowlist·CI→CD 진행·이력. (.9:15000)
package master

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/junhyeong9812/study-note-deploy-system-ci-cd/internal/shared"
)

const (
	// 합계가 워크플로 조회 상한(25분) 안에 들어와야 한다: 작업 중 대기 3분 + CI 11.5분 + CD 9분 = 23.5분
	integrationCallTimeout = 11*time.Minute + 30*time.Second // agent CI 예산 10분 + 원복 1분 + 여유
	deliveryCallTimeout    = 9 * time.Minute                 // 앞선 정리 대기 4분 10초 + 기동·헬스·전환 4분 30초 + 여유
	busyRetryLimit         = 12                              // agent가 다른 작업 중일 때 다시 불러 보는 횟수 — 배포 한 건 전체(CI+CD) 합계
	detailLimit            = 2000

	StageAgentUnreachable = "agent_unreachable"
	StageAgentRejected    = "agent_rejected" // agent가 단계에 들어가기 전에 거절(설정 누락·작업 중 등)
	StageInterrupted      = "interrupted"
	StageStore            = "store_failed"
)

// agent가 알려 주는 실패 단계 — 이 밖의 코드는 agent_rejected로 묶는다(이력의 단계 집합을 닫아 둔다).
var agentStages = map[string]bool{"ci_fetch": true, "ci_test": true, "ci_build": true, "cd_up": true, "cd_health": true, "cd_switch": true}

type DeployRequest struct {
	Service   string `json:"service"`
	CommitSha string `json:"commit_sha"`
	RequestID string `json:"request_id"`
}

type Master struct {
	secret   string
	logger   *shared.Logger
	agents   map[string]string // service → agent base URL (env — repo에 IP 금지)
	store    deploymentStore
	mutex    sync.Mutex
	inFlight map[string]bool // 서비스별 single-flight — 진행 중이면 명시적 409
	// 끝났지만 DB에 기록하지 못한 건. 조회는 이 값을 우선하고, 기록될 때까지 주기적으로 다시 쓴다.
	unrecorded         map[string]Deployment
	busyRetryInterval  time.Duration
	storeRetryInterval time.Duration
}

// New — 이력 DB를 열고, 지난 실행에서 끝나지 못한 건을 실패로 닫는다. DB를 못 열면 뜨지 않는다(fail-closed).
func New(logger *shared.Logger) (*Master, error) {
	agents := map[string]string{}
	// AGENT_URL_FRONT=http://<host>:15001 형식 — 서비스 allowlist는 이 고정 열거가 정본
	for _, service := range []string{"front", "backend", "llm"} {
		if agentURL := os.Getenv("AGENT_URL_" + strings.ToUpper(service)); agentURL != "" {
			agents[service] = agentURL
		}
	}
	path := os.Getenv("DEPLOY_DB_PATH")
	if path == "" {
		path = "data/deploys.db"
	}
	store, err := openStore(path)
	if err != nil {
		return nil, fmt.Errorf("이력 DB 열기 실패(%s): %w", path, err)
	}
	master := &Master{secret: os.Getenv("DEPLOY_SECRET"), logger: logger, agents: agents,
		store: store, inFlight: map[string]bool{}, unrecorded: map[string]Deployment{},
		busyRetryInterval: 15 * time.Second, storeRetryInterval: 30 * time.Second}
	closed, err := store.FailUnfinished(StageInterrupted, "master가 진행 중에 재시작됨", now())
	if err != nil {
		return nil, fmt.Errorf("미완료 건 정리 실패: %w", err)
	}
	if closed > 0 {
		logger.Log("boot", fmt.Sprintf("미완료 배포 %d건을 failed(interrupted)로 닫음", closed), "warning")
	}
	return master, nil
}

func now() string { return time.Now().UTC().Format(time.RFC3339) }

func (master *Master) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /deploy", master.handleDeploy)
	mux.HandleFunc("GET /deploys/{requestID}", master.handleDeployment)
	mux.HandleFunc("GET /deploys", master.handleDeployments)
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
	if !shared.ValidCommitSha(deploy.CommitSha) { // 접수 전에 거절 — 로그 주입·비정상 202 차단
		shared.WriteFail(writer, http.StatusUnprocessableEntity, "invalid_sha", "")
		return
	}
	agentURL, allowed := master.agents[deploy.Service]
	if !allowed {
		master.logger.Log(requestID, fmt.Sprintf("deploy rejected: unknown service %q", deploy.Service), "warning")
		shared.WriteFail(writer, http.StatusUnprocessableEntity, "unknown_service", deploy.Service)
		return
	}

	master.mutex.Lock()
	defer master.mutex.Unlock()
	// 멱등 접수 — 같은 request_id의 재전송(응답 유실 뒤 재시도)은 새 배포를 만들지 않는다.
	existing, found, err := master.store.Find(requestID)
	if err != nil {
		master.logger.Log(requestID, "deploy rejected: store read "+err.Error(), "error")
		shared.WriteFail(writer, http.StatusInternalServerError, StageStore, "")
		return
	}
	if found {
		existing = master.actual(existing)
		if existing.Service != deploy.Service || existing.CommitSha != deploy.CommitSha {
			shared.WriteFail(writer, http.StatusConflict, "request_id_conflict", "")
			return
		}
		shared.WriteAccepted(writer, map[string]any{"request_id": requestID, "service": existing.Service,
			"state": existing.State, "duplicate": true})
		return
	}
	if master.inFlight[deploy.Service] { // 진행 중이면 명시적 409 — 무음 유실 금지
		master.logger.Log(requestID, "deploy rejected: in progress "+deploy.Service, "warning")
		shared.WriteFail(writer, http.StatusConflict, "deploy_in_progress", deploy.Service)
		return
	}
	deployment := Deployment{RequestID: requestID, Service: deploy.Service, CommitSha: deploy.CommitSha,
		State: StateAccepted, AcceptedAt: now()}
	if err := master.store.Insert(deployment); err != nil { // 기록 못 하는 배포는 받지 않는다
		master.logger.Log(requestID, "deploy rejected: store write "+err.Error(), "error")
		shared.WriteFail(writer, http.StatusInternalServerError, StageStore, "")
		return
	}
	master.inFlight[deploy.Service] = true
	master.logger.Log(requestID, fmt.Sprintf("deploy accepted %s sha=%s", deploy.Service, deploy.CommitSha), "info")

	go master.advance(deployment, agentURL) // 접수 즉시 202 — 결과는 GET /deploys/{request_id}
	shared.WriteAccepted(writer, map[string]any{"request_id": requestID, "service": deploy.Service, "state": StateAccepted})
}

// advance — CI가 성공한 뒤에만 CD를 호출한다. 단계마다 DB에 기록하고, 기록에 실패하면 다음 단계로 가지 않는다.
func (master *Master) advance(deployment Deployment, agentURL string) {
	defer func() {
		master.mutex.Lock()
		delete(master.inFlight, deployment.Service)
		master.mutex.Unlock()
	}()
	fail := func(stage, detail string) {
		deployment.State, deployment.FailedStage = StateFailed, stage
		deployment.Detail, deployment.FinishedAt = lastPart(detail, detailLimit), now()
		master.logger.Log(deployment.RequestID, fmt.Sprintf("deploy failed %s sha=%s stage=%s %s",
			deployment.Service, deployment.CommitSha, stage, lastPart(detail, 200)), "error")
		master.recordFinished(deployment)
	}
	enter := func(state string) bool {
		deployment.State = state
		if err := master.store.Update(deployment); err != nil {
			fail(StageStore, state+" 기록 실패: "+err.Error())
			return false
		}
		return true
	}

	if !enter(StateIntegrating) {
		return
	}
	busyRetries := busyRetryLimit // CI와 CD가 나눠 쓴다 — 단계마다 따로 주면 합계가 조회 상한을 넘는다
	startedAt := time.Now()
	stage, detail := master.callAgentWhenFree(agentURL+"/agent/ci", deployment, integrationCallTimeout, &busyRetries)
	deployment.IntegrationMs = time.Since(startedAt).Milliseconds()
	if stage != "" {
		fail(stage, detail)
		return
	}
	if !enter(StateDelivering) { // CD 시작을 기록하지 못하면 운영을 바꾸지 않는다
		return
	}
	startedAt = time.Now()
	stage, detail = master.callAgentWhenFree(agentURL+"/agent/cd", deployment, deliveryCallTimeout, &busyRetries)
	deployment.DeliveryMs = time.Since(startedAt).Milliseconds()
	if stage != "" {
		fail(stage, detail)
		return
	}
	deployment.State, deployment.FinishedAt = StateSucceeded, now()
	if err := master.store.Update(deployment); err != nil {
		// 운영은 이미 바뀌었지만 이력에 남기지 못했다 — 기록 없는 성공은 성공으로 치지 않는다
		fail(StageStore, fmt.Sprintf("운영은 %s 로 교체됐으나 성공 기록 실패: %s", deployment.CommitSha, err.Error()))
		return
	}
	master.logger.Log(deployment.RequestID, fmt.Sprintf("deploy succeeded %s sha=%s ci=%dms cd=%dms",
		deployment.Service, deployment.CommitSha, deployment.IntegrationMs, deployment.DeliveryMs), "info")
}

// recordFinished — 끝난 결과를 DB에 쓴다. 쓰지 못하면 메모리에 들고(조회가 진행 중으로 보이지 않게)
// 기록될 때까지 주기적으로 다시 쓴다. 그사이 master가 재시작하면 그 행은 failed(interrupted)로 닫힌다.
func (master *Master) recordFinished(deployment Deployment) {
	if err := master.store.Update(deployment); err == nil {
		return
	} else {
		master.logger.Log(deployment.RequestID, "deploy 결과 기록 실패 — 메모리에 보관하고 재시도: "+err.Error(), "error")
	}
	master.mutex.Lock()
	master.unrecorded[deployment.RequestID] = deployment
	master.mutex.Unlock()
	go func() {
		for {
			time.Sleep(master.storeRetryInterval)
			if err := master.store.Update(deployment); err == nil {
				master.mutex.Lock()
				delete(master.unrecorded, deployment.RequestID)
				master.mutex.Unlock()
				master.logger.Log(deployment.RequestID, "deploy 결과 기록 복구됨", "warning")
				return
			}
		}
	}()
}

// actual — DB에 아직 기록하지 못한 종단 결과가 있으면 그것을 돌려준다. 단건 조회·이력·재접수가 같은 값을 보게 한다.
// 호출하는 쪽이 master.mutex를 쥐고 있어야 한다.
func (master *Master) actual(deployment Deployment) Deployment {
	if pending, unrecorded := master.unrecorded[deployment.RequestID]; unrecorded {
		return pending
	}
	return deployment
}

func (master *Master) actualAll(deployments []Deployment) []Deployment {
	master.mutex.Lock()
	defer master.mutex.Unlock()
	for index := range deployments {
		deployments[index] = master.actual(deployments[index])
	}
	return deployments
}

// callAgentWhenFree — agent가 다른 작업 중(409 agent_busy)이면 간격을 두고 다시 부른다.
// master가 재시작된 직후에는 agent가 앞선 작업을 아직 끝내지 못했을 수 있다.
func (master *Master) callAgentWhenFree(url string, deployment Deployment, timeout time.Duration, busyRetries *int) (stage, detail string) {
	for {
		stage, detail = master.callAgent(url, deployment, timeout)
		if stage != "agent_busy" || *busyRetries <= 0 {
			break
		}
		*busyRetries--
		time.Sleep(master.busyRetryInterval)
	}
	if stage != "" && stage != StageAgentUnreachable && !agentStages[stage] {
		return StageAgentRejected, stage + ": " + detail
	}
	return stage, detail
}

// callAgent — 성공이면 stage가 빈 문자열. 실패면 agent가 알려 준 코드와 출력.
func (master *Master) callAgent(url string, deployment Deployment, timeout time.Duration) (stage, detail string) {
	payload, _ := json.Marshal(map[string]string{
		"service": deployment.Service, "commit_sha": deployment.CommitSha, "request_id": deployment.RequestID,
	})
	agentRequest, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader(payload))
	agentRequest.Header.Set("Content-Type", "application/json")
	agentRequest.Header.Set(shared.SecretHeader, master.secret)
	response, err := (&http.Client{Timeout: timeout}).Do(agentRequest)
	if err != nil {
		return StageAgentUnreachable, err.Error()
	}
	defer response.Body.Close()
	responseBody, _ := io.ReadAll(io.LimitReader(response.Body, 16384))
	var envelope struct {
		Success bool `json:"success"`
		Error   struct {
			Code   string `json:"code"`
			Detail string `json:"detail"`
		} `json:"error"`
	}
	parseErr := json.Unmarshal(responseBody, &envelope)
	if response.StatusCode == http.StatusOK && parseErr == nil && envelope.Success {
		return "", ""
	}
	if parseErr != nil || envelope.Error.Code == "" { // agent의 봉투가 아니다 — 200이어도 성공으로 치지 않는다
		return "unexpected_response", fmt.Sprintf("status %d: %s", response.StatusCode, strings.TrimSpace(string(responseBody)))
	}
	return envelope.Error.Code, envelope.Error.Detail
}

func (master *Master) handleDeployment(writer http.ResponseWriter, request *http.Request) {
	if !shared.SecretMatches(request, master.secret) {
		shared.WriteFail(writer, http.StatusUnauthorized, "unauthorized", "")
		return
	}
	deployment, found, err := master.store.Find(request.PathValue("requestID"))
	if err != nil {
		shared.WriteFail(writer, http.StatusInternalServerError, StageStore, "")
		return
	}
	if !found {
		shared.WriteFail(writer, http.StatusNotFound, "not_found", "")
		return
	}
	master.mutex.Lock()
	deployment = master.actual(deployment) // DB에는 아직 진행 중으로 남아 있을 수 있다
	master.mutex.Unlock()
	shared.WriteOK(writer, deployment)
}

func (master *Master) handleDeployments(writer http.ResponseWriter, request *http.Request) {
	if !shared.SecretMatches(request, master.secret) {
		shared.WriteFail(writer, http.StatusUnauthorized, "unauthorized", "")
		return
	}
	limit, err := strconv.Atoi(request.URL.Query().Get("limit"))
	if err != nil || limit < 1 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	deployments, err := master.store.List(request.URL.Query().Get("service"), limit)
	if err != nil {
		shared.WriteFail(writer, http.StatusInternalServerError, StageStore, "")
		return
	}
	shared.WriteOK(writer, map[string]any{"deployments": master.actualAll(deployments)})
}

func (master *Master) handleStatus(writer http.ResponseWriter, request *http.Request) {
	if !shared.SecretMatches(request, master.secret) {
		shared.WriteFail(writer, http.StatusUnauthorized, "unauthorized", "")
		return
	}
	history, err := master.store.List("", 50)
	if err != nil {
		shared.WriteFail(writer, http.StatusInternalServerError, StageStore, "")
		return
	}
	shared.WriteOK(writer, map[string]any{"services": keys(master.agents), "history": master.actualAll(history)})
}

func keys(input map[string]string) []string {
	result := make([]string, 0, len(input))
	for key := range input {
		result = append(result, key)
	}
	return result
}

// lastPart — 출력의 끝부분. 빌드·테스트 오류는 끝에 있다.
func lastPart(input string, limit int) string {
	if len(input) <= limit {
		return input
	}
	return input[len(input)-limit:]
}
