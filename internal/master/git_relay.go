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

import (
	"net/http"
	"time"

	"github.com/junhyeong9812/study-note-deploy-system-ci-cd/internal/shared"
)

const (
	gitRelayTimeout       = 4 * time.Second // agent 의 3초 + 여유 — Master.gitRelayTimeout 의 기본값
	gitRelayResponseLimit = 4 << 20         // 이 두 중계에 한한 agent 응답 읽기 상한(배포 호출은 16KB 그대로)

	StageAgentTimeout       = "agent_timeout"
	StageUnexpectedResponse = "unexpected_response"
)

// handleGitCommits — GET /deploys/git/commits.
func (master *Master) handleGitCommits(writer http.ResponseWriter, request *http.Request) {
	shared.WriteFail(writer, http.StatusNotImplemented, "not_implemented", "") // TODO(task 05-a 구현 워커)
}

// handleGitCommit — GET /deploys/git/commit.
func (master *Master) handleGitCommit(writer http.ResponseWriter, request *http.Request) {
	shared.WriteFail(writer, http.StatusNotImplemented, "not_implemented", "") // TODO(task 05-a 구현 워커)
}
