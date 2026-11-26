package master

// git 조회 중계 — 배포 상세의 커밋 목록·diff 를 그 서비스의 agent 에서 받아 봉투 그대로 돌려준다(읽기 전용).
// 계약: docs/cross-cutting/plans/2026-11-22/admin-skeleton/requirement-spec.md §12 (12.7 이 12.2~12.3 보다 우선).
//
//   GET /deploys/git/commits?service=&from=&to= → agent GET /agent/git/commits
//   GET /deploys/git/commit?service=&sha=       → agent GET /agent/git/commit
//
// 순서: 비밀값(401 unauthorized) → 입력 검사(422 invalid_request — 허용 목록 밖 · agent 설정 없음 · SHA 꼴 아님 ·
// 칸 없음 · 칸 중복; 이때 agent 를 부르지 않는다) → agent 호출(GET, X-Deploy-Secret, 검사한 쿼리 그대로).
// agent 의 응답은 상태·봉투 그대로 전달한다. 중계 자체의 실패:
//   502 agent_unreachable · 504 agent_timeout(Master.gitRelayTimeout) · 502 unexpected_response(봉투 아님 · 4MB 초과).
//   agent 의 401 만은 그대로 전달하지 않고 502 agent_unauthorized(명세 12.8).

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/junhyeong9812/study-note-deploy-system-ci-cd/internal/shared"
)

const (
	gitRelayTimeout       = 4 * time.Second // agent 의 3초 + 여유 — Master.gitRelayTimeout 의 기본값
	gitRelayResponseLimit = 4 << 20         // 이 두 중계에 한한 agent 응답 읽기 상한(배포 호출은 16KB 그대로)

	StageAgentTimeout       = "agent_timeout"
	StageUnexpectedResponse = "unexpected_response"
	StageAgentUnauthorized  = "agent_unauthorized" // agent 가 401 — master 의 DEPLOY_SECRET 이 agent 와 다르다
)

// handleGitCommits — GET /deploys/git/commits.
func (master *Master) handleGitCommits(writer http.ResponseWriter, request *http.Request) {
	master.relayGitRead(writer, request, "/agent/git/commits", []string{"from", "to"}, gitCommitsRequiredFields)
}

// handleGitCommit — GET /deploys/git/commit.
func (master *Master) handleGitCommit(writer http.ResponseWriter, request *http.Request) {
	master.relayGitRead(writer, request, "/agent/git/commit", []string{"sha"}, gitCommitRequiredFields)
}

// relayGitRead — 비밀값 → 입력 검사(틀리면 agent 를 부르지 않는다) → 그 서비스의 agent 를 GET 으로 → 상태·봉투 그대로.
// 배포 호출(callAgent — POST)과 따로 둔다: 기한과 응답의 검사가 다르다.
func (master *Master) relayGitRead(writer http.ResponseWriter, request *http.Request, agentPath string, shaFields []string,
	requiredFields map[string]jsonKind) {
	if !shared.SecretMatches(request, master.secret) {
		shared.WriteFail(writer, http.StatusUnauthorized, "unauthorized", "")
		return
	}
	query := request.URL.Query()
	service, present := singleQueryValue(query, "service")
	agentURL := master.agents[service] // 허용 목록 밖이거나 agent 설정이 없는 서비스는 빈 값
	if !present || agentURL == "" {
		shared.WriteFail(writer, http.StatusUnprocessableEntity, "invalid_request", "")
		return
	}
	forwarded := url.Values{"service": {service}}
	for _, field := range shaFields {
		value, present := singleQueryValue(query, field)
		if !present || !shared.ValidCommitSha(value) {
			shared.WriteFail(writer, http.StatusUnprocessableEntity, "invalid_request", "")
			return
		}
		forwarded.Set(field, value)
	}

	requestID := shared.AcceptOrIssue(request.Header.Get("X-Request-Id"))
	agentRequest, err := http.NewRequest(http.MethodGet, agentURL+agentPath+"?"+forwarded.Encode(), nil)
	if err != nil { // AGENT_URL_<SVC> 가 URL 꼴이 아니다
		master.relayFailed(writer, requestID, service, agentPath, http.StatusBadGateway, StageAgentUnreachable)
		return
	}
	agentRequest.Header.Set(shared.SecretHeader, master.secret)
	agentRequest.Header.Set("X-Request-Id", requestID)
	response, err := (&http.Client{Timeout: master.gitRelayTimeout}).Do(agentRequest)
	if err != nil {
		if isTimeout(err) {
			master.relayFailed(writer, requestID, service, agentPath, http.StatusGatewayTimeout, StageAgentTimeout)
		} else {
			master.relayFailed(writer, requestID, service, agentPath, http.StatusBadGateway, StageAgentUnreachable)
		}
		return
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, gitRelayResponseLimit+1))
	if err != nil {
		if isTimeout(err) {
			master.relayFailed(writer, requestID, service, agentPath, http.StatusGatewayTimeout, StageAgentTimeout)
		} else {
			master.relayFailed(writer, requestID, service, agentPath, http.StatusBadGateway, StageUnexpectedResponse)
		}
		return
	}
	if len(responseBody) > gitRelayResponseLimit || !isAgentEnvelope(response.StatusCode, responseBody, requiredFields) {
		master.relayFailed(writer, requestID, service, agentPath, http.StatusBadGateway, StageUnexpectedResponse)
		return
	}
	if response.StatusCode == http.StatusUnauthorized { // master 와 agent 의 비밀값 불일치(설정 실수) — 호출한 쪽의 401 과 구분한다
		master.relayFailed(writer, requestID, service, agentPath, http.StatusBadGateway, StageAgentUnauthorized)
		return
	}
	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	writer.WriteHeader(response.StatusCode)
	_, _ = writer.Write(responseBody)
}

func (master *Master) relayFailed(writer http.ResponseWriter, requestID, service, agentPath string, status int, code string) {
	master.logger.Log(requestID, fmt.Sprintf("git relay failed %s %s: %s", service, agentPath, code), "warning")
	shared.WriteFail(writer, status, code, "")
}

// jsonKind — 성공 봉투의 data 에서 필수 칸이 가져야 할 JSON 형.
type jsonKind int

const (
	jsonString jsonKind = iota
	jsonBoolean
	jsonArray
)

// 경로별 성공 data 의 필수 칸(명세 12.9) — 없거나 형이 다르면 agent 의 답으로 보지 않는다.
var (
	gitCommitsRequiredFields = map[string]jsonKind{"commits": jsonArray, "relation": jsonString, "truncated": jsonBoolean, "range_complete": jsonBoolean}
	gitCommitRequiredFields  = map[string]jsonKind{"sha": jsonString, "files": jsonArray, "files_truncated": jsonBoolean}
)

// isAgentEnvelope — 성공은 200 + success:true + data 가 필수 칸을 갖춘 객체, 실패는 4xx·5xx + success:false + 코드.
// 그 밖은 agent 의 봉투로 보지 않는다.
func isAgentEnvelope(status int, body []byte, requiredFields map[string]jsonKind) bool {
	var envelope struct {
		Success *bool          `json:"success"`
		Data    map[string]any `json:"data"`
		Error   struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil || envelope.Success == nil {
		return false // data 가 객체가 아니어도(배열 등) 여기서 걸린다
	}
	if !*envelope.Success {
		return status >= http.StatusBadRequest && envelope.Error.Code != ""
	}
	if status != http.StatusOK || envelope.Data == nil {
		return false
	}
	for field, kind := range requiredFields {
		value, present := envelope.Data[field]
		switch kind {
		case jsonString:
			_, present = value.(string)
		case jsonBoolean:
			_, present = value.(bool)
		case jsonArray:
			_, present = value.([]any)
		}
		if !present {
			return false
		}
	}
	return true
}

func isTimeout(err error) bool {
	var networkError net.Error
	return errors.As(err, &networkError) && networkError.Timeout()
}

// singleQueryValue — 칸이 정확히 하나이고 비어 있지 않을 때만 받는다(없음·빈 값·중복은 거절).
func singleQueryValue(query url.Values, key string) (string, bool) {
	values := query[key]
	if len(values) != 1 || values[0] == "" {
		return "", false
	}
	return values[0], true
}
