package agent

// git 조회 — 배포 상세의 커밋 목록·커밋 하나의 diff 를 서버의 저장소에서 읽는다(읽기 전용).
// 계약: docs/cross-cutting/plans/2026-11-22/admin-skeleton/requirement-spec.md §12 (12.7 이 12.2~12.3 보다 우선).
//
//   GET /agent/git/commits?service=&from=<A>&to=<B> → GitCommitRange
//   GET /agent/git/commit?service=&sha=            → GitCommitDetail
//
// 인증은 X-Deploy-Secret(본문 검사보다 먼저). 오류는 공통 봉투(shared.WriteFail)로:
//   401 unauthorized · 422 invalid_request · 404 commit_not_found · 404 parent_unavailable ·
//   429 too_many_requests · 504 git_timeout · 500 git_failed (detail 없음).
// 조회는 busy 잠금을 잡지 않는다. 동시 조회는 두 경로를 합쳐 gitReadConcurrency 개까지, 넘으면 기다리지 않고 429.

import (
	"net/http"
	"time"

	"github.com/junhyeong9812/study-note-deploy-system-ci-cd/internal/shared"
)

const (
	gitReadTimeout           = 3 * time.Second // 조회 하나의 기한(agent) — Agent.gitTimeout 의 기본값
	gitReadConcurrency       = 2               // 동시 조회 상한 — 넘으면 즉시 429
	gitCommitListLimit       = 100             // 커밋 목록 — 넘으면 B 쪽 100개 + truncated
	gitFileLimit             = 100             // 커밋 하나의 파일 — 넘으면 files_truncated
	gitPatchLineLimit        = 500             // 파일 하나의 patch 줄 수 — 넘으면 patch_truncated
	gitPatchLineByteLimit    = 2000            // patch 한 줄의 바이트 — 넘는 줄은 잘라 표시
	gitSubjectByteLimit      = 500             // 제목 바이트
	gitBodyByteLimit         = 8 << 10         // 본문 바이트 — 넘으면 body_truncated
	gitOutputByteLimit       = 2 << 20         // git 명령 하나의 stdout — 넘으면 온전한 마지막 레코드까지
	gitResponseByteLimit     = 2 << 20         // 최종 JSON — 넘으면 뒤 파일부터 patch 를 비우고 patch_truncated
	gitRelationAncestor      = "ancestor"      // A 가 B 의 조상 — 목록은 A..B
	gitRelationNotAncestor   = "not_ancestor"  // 되돌린 배포 등 — 목록은 빈 배열
	gitFileStatusAdded       = "added"
	gitFileStatusModified    = "modified"
	gitFileStatusDeleted     = "deleted"
	gitFileStatusRenamed     = "renamed"
	gitCodeInvalidRequest    = "invalid_request"
	gitCodeCommitNotFound    = "commit_not_found"
	gitCodeParentUnavailable = "parent_unavailable"
	gitCodeTooManyRequests   = "too_many_requests"
	gitCodeTimeout           = "git_timeout"
	gitCodeFailed            = "git_failed"
)

// GitCommit — 커밋 하나의 요약. 작성자 이메일은 담지 않는다(이름만).
type GitCommit struct {
	Sha           string `json:"sha"`         // 40자 전체 SHA
	AuthorName    string `json:"author_name"` // 이메일 없음
	AuthoredAt    string `json:"authored_at"` // ISO-8601 UTC (RFC3339, "Z")
	Subject       string `json:"subject"`     // gitSubjectByteLimit 이하
	Body          string `json:"body"`        // gitBodyByteLimit 이하
	BodyTruncated bool   `json:"body_truncated"`
}

// GitCommitRange — GET /agent/git/commits 의 data.
type GitCommitRange struct {
	Commits       []GitCommit `json:"commits"`        // B 쪽이 먼저(--topo-order). 빈 경우에도 null 이 아닌 [].
	Truncated     bool        `json:"truncated"`      // 100개를 넘었거나 출력 상한에 걸림
	RangeComplete bool        `json:"range_complete"` // 목록에 얕은 경계 커밋이 들어 있지 않음
	Relation      string      `json:"relation"`       // gitRelationAncestor | gitRelationNotAncestor
}

// GitFileChange — 커밋의 파일 하나(첫째 부모와의 차이).
type GitFileChange struct {
	Path           string  `json:"path"`          // -z 출력에서 읽은 경로(탭·줄바꿈·따옴표 그대로)
	PreviousPath   *string `json:"previous_path"` // renamed 일 때만, 그 밖은 null
	Status         string  `json:"status"`        // added | modified | deleted | renamed
	Additions      int     `json:"additions"`     // 실제 더한 줄 수(patch 를 잘라도 줄지 않는다). 바이너리는 0
	Deletions      int     `json:"deletions"`
	Binary         bool    `json:"binary"`
	Patch          *string `json:"patch"` // GitHub 의 patch 와 같은 꼴 — 첫 줄이 "@@" 구간 머리. 바이너리는 null
	PatchTruncated bool    `json:"patch_truncated"`
}

// GitCommitDetail — GET /agent/git/commit 의 data.
type GitCommitDetail struct {
	GitCommit
	Files          []GitFileChange `json:"files"` // 빈 경우에도 []
	FilesTruncated bool            `json:"files_truncated"`
}

// handleGitCommits — GET /agent/git/commits.
func (agent *Agent) handleGitCommits(writer http.ResponseWriter, request *http.Request) {
	shared.WriteFail(writer, http.StatusNotImplemented, "not_implemented", "") // TODO(task 05-a 구현 워커)
}

// handleGitCommit — GET /agent/git/commit.
func (agent *Agent) handleGitCommit(writer http.ResponseWriter, request *http.Request) {
	shared.WriteFail(writer, http.StatusNotImplemented, "not_implemented", "") // TODO(task 05-a 구현 워커)
}
