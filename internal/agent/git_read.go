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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

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
	Relation      string      `json:"relation"`       // gitRelationAncestor | gitRelationNotAncestor | gitRelationUnknown
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

const (
	gitCommitFormat      = "--format=%H%x00%an%x00%aI%x00%s%x00%b" // 이메일(%ae)은 읽지도 않는다
	gitCommitFieldCount  = 5                                       // 위 형식의 칸 수 — -z 로 레코드마다 NUL 로 끝맺는다
	gitLineCutMarker     = " …(잘림)"                                // patch 한 줄을 자른 자리
	gitEnvelopeAllowance = 64                                      // 봉투({"data":…,"success":true}\n)가 data 에 더하는 바이트의 넉넉한 몫
	gitRelationUnknown   = "unknown"                               // 이력이 얕은 경계에서 끊겨 알 수 없음 — 목록은 빈 배열, range_complete:false
)

// 조회 실패의 갈래 — 응답 코드는 이것으로만 정한다. git 의 오류 글자는 읽지 않는다(지역 설정을 탄다 — 명세 12.5).
var (
	errGitTimeout          = errors.New(gitCodeTimeout)
	errCommitNotFound      = errors.New(gitCodeCommitNotFound)
	errParentUnavailable   = errors.New(gitCodeParentUnavailable)
	errUnexpectedGitOutput = errors.New("git 출력 형식이 예상과 다름")
	errShallowListUnread   = errors.New("shallow 목록 읽기 실패")
)

// git 이 확정해 준 전체 SHA(SHA-1 40자 · SHA-256 64자).
var fullCommitSha = regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})$`)

// gitCommandError — git 이 0 이 아닌 코드로 끝났거나 뜨지 못했다(-1). 로그에는 명령 종류와 종료 코드만 남긴다.
type gitCommandError struct {
	subcommand string
	exitCode   int
}

func (failure *gitCommandError) Error() string {
	return fmt.Sprintf("git %s exit %d", failure.subcommand, failure.exitCode)
}

// handleGitCommits — GET /agent/git/commits.
func (agent *Agent) handleGitCommits(writer http.ResponseWriter, request *http.Request) {
	agent.serveGitRead(writer, request, "commits", []string{"from", "to"}, readCommitRange)
}

// handleGitCommit — GET /agent/git/commit.
func (agent *Agent) handleGitCommit(writer http.ResponseWriter, request *http.Request) {
	agent.serveGitRead(writer, request, "commit", []string{"sha"}, readCommitDetail)
}

// serveGitRead — 두 조회의 공통 접수: 인증 → 입력 전체 검사 → 동시 조회 자리(기다리지 않는다) → git.
// 틀린 입력으로는 git 을 띄우지 않는다. busy 잠금은 잡지 않는다(배포를 막지 않는다).
func (agent *Agent) serveGitRead(writer http.ResponseWriter, request *http.Request, name string, shaFields []string,
	read func(reader *gitReader, shas []string) (any, error)) {
	if !shared.SecretMatches(request, agent.secret) {
		shared.WriteFail(writer, http.StatusUnauthorized, "unauthorized", "")
		return
	}
	query := request.URL.Query()
	service, present := singleQueryValue(query, "service")
	directory := agent.directories[service] // 허용 목록의 키로만 디렉터리를 고른다 — 입력 글자는 경로에 들어가지 않는다
	if !present || directory == "" {
		shared.WriteFail(writer, http.StatusUnprocessableEntity, gitCodeInvalidRequest, "")
		return
	}
	shas := make([]string, len(shaFields))
	for index, field := range shaFields {
		value, present := singleQueryValue(query, field)
		if !present || !shared.ValidCommitSha(value) {
			shared.WriteFail(writer, http.StatusUnprocessableEntity, gitCodeInvalidRequest, "")
			return
		}
		shas[index] = value
	}
	select {
	case agent.gitReadSlots <- struct{}{}:
		defer func() { <-agent.gitReadSlots }()
	default:
		shared.WriteFail(writer, http.StatusTooManyRequests, gitCodeTooManyRequests, "")
		return
	}

	requestID := shared.AcceptOrIssue(request.Header.Get("X-Request-Id"))
	ctx, cancel := context.WithTimeout(request.Context(), agent.gitTimeout)
	defer cancel()
	startedAt := time.Now()
	data, err := read(&gitReader{ctx: ctx, program: agent.gitProgram, directory: directory}, shas)
	tookMs := time.Since(startedAt).Milliseconds()
	switch {
	case err == nil:
		agent.logger.Log(requestID, fmt.Sprintf("agent git %s ok %s %dms", name, service, tookMs), "info")
		shared.WriteOK(writer, data)
	case errors.Is(err, errCommitNotFound):
		shared.WriteFail(writer, http.StatusNotFound, gitCodeCommitNotFound, "")
	case errors.Is(err, errParentUnavailable):
		shared.WriteFail(writer, http.StatusNotFound, gitCodeParentUnavailable, "")
	case errors.Is(err, errGitTimeout):
		agent.logger.Log(requestID, fmt.Sprintf("agent git %s timeout %s %dms", name, service, tookMs), "warning")
		shared.WriteFail(writer, http.StatusGatewayTimeout, gitCodeTimeout, "")
	default: // detail 은 내지 않는다 — 경로·환경이 샐 수 있다. 로그에도 명령 종류·종료 코드만.
		agent.logger.Log(requestID, fmt.Sprintf("agent git %s failed %s %dms: %v", name, service, tookMs, err), "error")
		shared.WriteFail(writer, http.StatusInternalServerError, gitCodeFailed, "")
	}
}

// singleQueryValue — 칸이 정확히 하나이고 비어 있지 않을 때만 받는다(없음·빈 값·중복은 거절).
func singleQueryValue(query map[string][]string, key string) (string, bool) {
	values := query[key]
	if len(values) != 1 || values[0] == "" {
		return "", false
	}
	return values[0], true
}

// readCommitRange — A 다음부터 B 까지(A 제외·B 포함), git 의 순회 순서(--topo-order, B 쪽이 먼저).
func readCommitRange(reader *gitReader, shas []string) (any, error) {
	boundaries, err := reader.shallowBoundaries()
	if err != nil {
		return nil, err
	}
	from, err := reader.resolveCommit(shas[0])
	if err != nil {
		return nil, err
	}
	to, err := reader.resolveCommit(shas[1])
	if err != nil {
		return nil, err
	}
	result := GitCommitRange{Commits: []GitCommit{}, RangeComplete: true}
	result.Relation, err = reader.relation(from, to, boundaries)
	if err != nil {
		return nil, err
	}
	if result.Relation == gitRelationUnknown {
		result.RangeComplete = false
	}
	if result.Relation != gitRelationAncestor { // 목록을 만들지 않는다(어드민이 GitHub 비교로 넘긴다)
		return result, nil
	}
	output, overflowed, err := reader.run("log", "--topo-order", "--no-color", "--no-show-signature", "-z", gitCommitFormat,
		"--max-count="+strconv.Itoa(gitCommitListLimit+1), "--end-of-options", from+".."+to)
	if err != nil {
		return nil, err
	}
	commits, err := parseCommits(output, overflowed)
	if err != nil {
		return nil, err
	}
	if len(commits) > gitCommitListLimit {
		commits = commits[:gitCommitListLimit]
		result.Truncated = true
	}
	result.Truncated = result.Truncated || overflowed
	for _, commit := range commits {
		if boundaries[commit.Sha] { // 그 너머(부모)는 로컬에 없다 — 범위가 잘렸을 수 있다
			result.RangeComplete = false
		}
	}
	result.Commits = commits
	limitRangeSize(&result)
	return result, nil
}

// relation — 명세 12.8 의 판정 순서. 얕은 이력에서는 A 객체가 남아 있어도 B 에서 A 까지의 이력이 경계에서 끊길 수 있어
// merge-base 의 "조상 아님"만으로는 되돌림·갈라짐이라 말할 수 없다.
//  1. A 가 B 의 조상 → ancestor
//  2. B 의 조상 순회가 얕은 경계에 닿지 않는다(이력이 온전하다) → not_ancestor
//  3. B 가 A 의 조상(확실한 되돌림) → not_ancestor
//  4. 그 밖 → unknown
func (reader *gitReader) relation(from, to string, boundaries map[string]bool) (string, error) {
	ancestor, err := reader.isAncestor(from, to)
	if err != nil || ancestor {
		return gitRelationAncestor, err
	}
	reached, err := reader.reachesShallowBoundary(to, boundaries)
	if err != nil {
		return "", err
	}
	if !reached {
		return gitRelationNotAncestor, nil
	}
	rolledBack, err := reader.isAncestor(to, from)
	if err != nil {
		return "", err
	}
	if rolledBack {
		return gitRelationNotAncestor, nil
	}
	return gitRelationUnknown, nil
}

// reachesShallowBoundary — sha 의 조상(자기 포함) 가운데 얕은 경계 커밋이 있는가. 얕지 않은 저장소면 닿을 경계가 없다.
// 순회 출력이 상한에 걸리면 끝까지 보지 못한 것이므로 "닿았다"로 본다(조상 아님을 단정하지 않는 쪽).
func (reader *gitReader) reachesShallowBoundary(sha string, boundaries map[string]bool) (bool, error) {
	if len(boundaries) == 0 {
		return false, nil
	}
	output, overflowed, err := reader.run("rev-list", "--end-of-options", sha)
	if err != nil {
		return false, err
	}
	if overflowed {
		return true, nil
	}
	for _, ancestor := range strings.Fields(string(output)) {
		if boundaries[ancestor] {
			return true, nil
		}
	}
	return false, nil
}

// limitRangeSize — 커밋 목록의 최종 JSON(봉투 포함)이 gitResponseByteLimit 를 넘으면 뒤(오래된) 커밋부터 빼고 truncated.
func limitRangeSize(result *GitCommitRange) {
	encoded, err := json.Marshal(result)
	if err != nil {
		return
	}
	size := len(encoded) + gitEnvelopeAllowance
	for len(result.Commits) > 0 && size > gitResponseByteLimit {
		last := len(result.Commits) - 1
		encodedCommit, _ := json.Marshal(result.Commits[last])
		size -= len(encodedCommit) + len(",")
		result.Commits = result.Commits[:last]
		result.Truncated = true
	}
}

// readCommitDetail — 커밋 하나와 그 첫째 부모와의 차이. 얕은 경계 커밋은 부모가 없어 틀린 diff 가 되므로 답하지 않는다.
func readCommitDetail(reader *gitReader, shas []string) (any, error) {
	boundaries, err := reader.shallowBoundaries()
	if err != nil {
		return nil, err
	}
	sha, err := reader.resolveCommit(shas[0])
	if err != nil {
		return nil, err
	}
	if boundaries[sha] {
		return nil, errParentUnavailable
	}
	output, overflowed, err := reader.run("log", "--no-color", "--no-show-signature", "-z", gitCommitFormat,
		"--max-count=1", "--end-of-options", sha)
	if err != nil {
		return nil, err
	}
	commits, err := parseCommits(output, overflowed)
	if err != nil {
		return nil, err
	}
	if len(commits) != 1 || commits[0].Sha != sha {
		return nil, errUnexpectedGitOutput
	}
	files, filesTruncated, err := reader.fileChanges(sha)
	if err != nil {
		return nil, err
	}
	detail := GitCommitDetail{GitCommit: commits[0], Files: files, FilesTruncated: filesTruncated}
	limitResponseSize(&detail)
	return detail, nil
}

// gitReader — 조회 하나의 git 실행 묶음(같은 기한·같은 저장소).
type gitReader struct {
	ctx       context.Context
	program   string
	directory string
}

// gitEnvironment — 부모의 GIT_* 는 물려주지 않는다(다른 저장소·추적 파일·설정 주입 차단). 전역 설정(safe.directory)은 HOME 으로 그대로 읽는다.
func gitEnvironment() []string {
	environment := []string{}
	for _, entry := range os.Environ() {
		if strings.HasPrefix(entry, "GIT_") || strings.HasPrefix(entry, "LC_ALL=") {
			continue
		}
		environment = append(environment, entry)
	}
	return append(environment, "LC_ALL=C", "GIT_OPTIONAL_LOCKS=0", "GIT_NO_LAZY_FETCH=1",
		"GIT_NO_REPLACE_OBJECTS=1", "GIT_TERMINAL_PROMPT=0")
}

// run — git 을 인자 배열로(셸 없음) 돌리고 stdout 을 읽으면서 gitOutputByteLimit 을 건다. stderr 는 버린다(응답·로그에 넣지 않는다).
// 상한을 넘으면 더 읽지 않고 프로세스를 끝낸 뒤 overflowed=true — 오류가 아니다, 호출한 쪽이 온전한 레코드까지만 쓴다.
// 기한이 지나면 CommandContext 가 자식을 죽이고 Wait 가 끝날 때까지 기다린다(좀비를 남기지 않는다).
func (reader *gitReader) run(subcommand string, arguments ...string) (output []byte, overflowed bool, err error) {
	command := exec.CommandContext(reader.ctx, reader.program, append([]string{"--no-pager", "-c", "core.quotePath=true", subcommand}, arguments...)...)
	command.Dir = reader.directory
	command.Env = gitEnvironment()
	command.WaitDelay = time.Second // 자식이 물려준 파이프를 누가 쥐고 있어도 Wait 가 끝나게
	stdout, err := command.StdoutPipe()
	if err != nil {
		return nil, false, &gitCommandError{subcommand, -1}
	}
	if err := command.Start(); err != nil {
		if reader.ctx.Err() != nil {
			return nil, false, errGitTimeout
		}
		return nil, false, &gitCommandError{subcommand, -1}
	}
	output, readErr := io.ReadAll(io.LimitReader(stdout, gitOutputByteLimit+1))
	if len(output) > gitOutputByteLimit {
		output, overflowed = output[:gitOutputByteLimit], true
		_ = command.Process.Kill() // 남은 출력은 버린다 — 이미 상한
	}
	waitErr := command.Wait()
	if overflowed {
		return output, true, nil
	}
	if waitErr != nil && reader.ctx.Err() != nil {
		return nil, false, errGitTimeout
	}
	if waitErr != nil || readErr != nil {
		exitCode := -1
		var exitError *exec.ExitError
		if errors.As(waitErr, &exitError) {
			exitCode = exitError.ExitCode()
		}
		return nil, false, &gitCommandError{subcommand, exitCode}
	}
	return output, false, nil
}

// shallowBoundaries — 얕은 저장소의 경계 커밋(부모가 로컬에 없는 커밋). 얕지 않으면 빈 집합.
// 조회마다 가장 먼저 부른다 — 디렉터리가 저장소가 아니면 여기서 git_failed 로 끝나, 뒤의 "커밋 없음(404)"과 섞이지 않는다.
func (reader *gitReader) shallowBoundaries() (map[string]bool, error) {
	output, _, err := reader.run("rev-parse", "--git-path", "shallow")
	if err != nil {
		return nil, err
	}
	path := strings.TrimSuffix(string(output), "\n")
	if !filepath.IsAbs(path) {
		path = filepath.Join(reader.directory, path)
	}
	content, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]bool{}, nil
	}
	if err != nil {
		return nil, errShallowListUnread
	}
	boundaries := map[string]bool{}
	for _, sha := range strings.Fields(string(content)) {
		boundaries[sha] = true
	}
	return boundaries, nil
}

// resolveCommit — 형식 검사를 지난 SHA 가 이 저장소의 커밋인지 확인하고 전체 SHA 로 확정한다. 뒤의 명령은 이 값만 쓴다.
// 없음(얕은 이력 밖 포함)·커밋 아님·약식 SHA 가 모호함은 모두 0 이 아닌 종료 코드 → commit_not_found.
func (reader *gitReader) resolveCommit(sha string) (string, error) {
	output, _, err := reader.run("rev-parse", "--verify", "--quiet", "--end-of-options", sha+"^{commit}")
	var commandError *gitCommandError
	if errors.As(err, &commandError) && commandError.exitCode > 0 {
		return "", errCommitNotFound
	}
	if err != nil {
		return "", err
	}
	full := strings.TrimSpace(string(output))
	if !fullCommitSha.MatchString(full) {
		return "", errUnexpectedGitOutput
	}
	return full, nil
}

// isAncestor — merge-base --is-ancestor 의 종료 코드: 0 조상 · 1 조상 아님 · 그 밖 실패.
func (reader *gitReader) isAncestor(from, to string) (bool, error) {
	_, _, err := reader.run("merge-base", "--is-ancestor", "--end-of-options", from, to)
	var commandError *gitCommandError
	switch {
	case err == nil:
		return true, nil
	case errors.As(err, &commandError) && commandError.exitCode == 1:
		return false, nil
	default:
		return false, err
	}
}

// parseCommits — gitCommitFormat 과 -z 의 출력(레코드마다 칸 5개, 칸과 레코드 모두 NUL 로 끝난다).
// 출력이 상한에서 잘렸으면 온전한 레코드까지만 쓴다. 잘리지 않았는데 칸 수가 맞지 않으면 형식 오류다.
func parseCommits(output []byte, overflowed bool) ([]GitCommit, error) {
	fields := strings.Split(string(output), "\x00")
	completeFields := len(fields) - 1 // 마지막 조각은 NUL 로 끝맺지 않은 나머지
	if !overflowed && (completeFields%gitCommitFieldCount != 0 || fields[completeFields] != "") {
		return nil, errUnexpectedGitOutput
	}
	commits := []GitCommit{}
	for start := 0; start+gitCommitFieldCount <= completeFields; start += gitCommitFieldCount {
		record := fields[start : start+gitCommitFieldCount]
		authoredAt, err := time.Parse(time.RFC3339, record[2])
		if !fullCommitSha.MatchString(record[0]) || err != nil {
			return nil, errUnexpectedGitOutput
		}
		subject, _ := cutBytes(record[3], gitSubjectByteLimit)
		body, bodyTruncated := cutBytes(record[4], gitBodyByteLimit)
		commits = append(commits, GitCommit{Sha: record[0], AuthorName: record[1],
			AuthoredAt: authoredAt.UTC().Format(time.RFC3339), Subject: subject, Body: body, BodyTruncated: bodyTruncated})
	}
	return commits, nil
}

// diffArguments — 파일 목록·줄 수·patch 를 같은 옵션으로 만든다(세 출력의 파일 순서가 같아야 짝이 맞는다).
// 병합 커밋은 첫째 부모와의 차이, 이름 바꾸기 탐지는 -M 고정, 외부 diff·textconv·색 없음.
func diffArguments(sha string, outputOptions ...string) []string {
	arguments := []string{"--format=", "--no-color", "--no-ext-diff", "--no-textconv", "--no-show-signature",
		"-M", "--diff-merges=first-parent", "--src-prefix=a/", "--dst-prefix=b/"}
	arguments = append(arguments, outputOptions...)
	return append(arguments, "--end-of-options", sha)
}

type rawFileEntry struct {
	status       string
	path         string
	previousPath *string
	typeChanged  bool // 파일↔심볼릭 링크 — patch 는 같은 머리줄의 두 조각으로 나온다
}

type fileLineCount struct {
	additions, deletions int
	binary               bool
}

// patchSection — patch 출력의 파일 한 조각: `diff --git ` 머리줄과 첫 `@@` 부터의 내용.
type patchSection struct {
	header string
	hunks  string
}

// fileChanges — 파일 경로·상태는 --raw -z 에서, 줄 수는 --numstat -z 에서, patch 는 --patch 에서 읽는다.
// 경로는 patch 머리줄에서 파싱하지 않는다(탭·줄바꿈·따옴표가 든 경로). 파일 목록(raw·numstat)과 patch 는 따로 지킨다:
// 목록을 온전히 읽었으면 patch 출력이 상한에 걸려도 목록은 싣고, patch 를 온전히 읽지 못했거나 짝을 확정하지 못한 파일만
// patch 를 비우고 patch_truncated. files_truncated 는 목록 자체가 잘렸을 때(100개 초과 · 목록 출력 상한)만.
func (reader *gitReader) fileChanges(sha string) ([]GitFileChange, bool, error) {
	rawOutput, rawOverflowed, err := reader.run("show", diffArguments(sha, "-z", "--raw")...)
	if err != nil {
		return nil, false, err
	}
	entries, err := parseRawEntries(rawOutput, rawOverflowed)
	if err != nil {
		return nil, false, err
	}
	numstatOutput, numstatOverflowed, err := reader.run("show", diffArguments(sha, "-z", "--numstat")...)
	if err != nil {
		return nil, false, err
	}
	counts, err := parseNumstat(numstatOutput, numstatOverflowed)
	if err != nil {
		return nil, false, err
	}
	listOverflowed := rawOverflowed || numstatOverflowed
	if !listOverflowed && len(entries) != len(counts) {
		return nil, false, errUnexpectedGitOutput
	}
	fileCount := min(len(entries), len(counts))
	filesTruncated := listOverflowed // 목록이 상한에서 잘렸으면 온전히 읽은 파일까지만 싣는다
	if fileCount > gitFileLimit {
		fileCount, filesTruncated = gitFileLimit, true
	}
	patchOutput, patchOverflowed, err := reader.run("show", diffArguments(sha, "--patch")...)
	if err != nil {
		return nil, false, err
	}
	sections := splitPatchSections(patchOutput, patchOverflowed)

	files := make([]GitFileChange, 0, fileCount)
	next := 0 // 아직 짝짓지 않은 첫 조각
	for index := 0; index < fileCount; index++ {
		entry, count := entries[index], counts[index]
		file := GitFileChange{Path: entry.path, PreviousPath: entry.previousPath, Status: entry.status,
			Additions: count.additions, Deletions: count.deletions, Binary: count.binary}
		hunks, consumed := pairPatch(entry, sections[next:])
		next += consumed
		switch {
		case count.binary:
		case consumed == 0: // 온전히 읽지 못했거나 짝을 확정하지 못했다 — 순서로 밀어 맞추지 않는다
			emptied := ""
			file.Patch, file.PatchTruncated = &emptied, true
		default:
			patch, patchTruncated := limitPatch(hunks)
			file.Patch, file.PatchTruncated = &patch, patchTruncated
		}
		files = append(files, file)
	}
	return files, filesTruncated, nil
}

// pairPatch — 파일의 머리줄(경로로 만든 `diff --git a/… b/…`)과 같은 조각을 앞에서부터 짝짓는다.
// 형 바뀜은 같은 머리줄의 조각 둘을 이어 붙인다. 앞 조각의 머리줄이 다르면 짝이 없는 것으로 보고 조각을 쓰지 않는다(consumed=0).
func pairPatch(entry rawFileEntry, sections []patchSection) (hunks string, consumed int) {
	wanted := 1
	if entry.typeChanged {
		wanted = 2
	}
	if len(sections) < wanted {
		return "", 0
	}
	previousPath := entry.path
	if entry.previousPath != nil {
		previousPath = *entry.previousPath
	}
	header := "diff --git " + quoteGitPath("a/"+previousPath) + " " + quoteGitPath("b/"+entry.path)
	var builder strings.Builder
	for _, section := range sections[:wanted] {
		if section.header != header {
			return "", 0
		}
		builder.WriteString(section.hunks)
	}
	return builder.String(), wanted
}

// quoteGitPath — git 이 patch 머리줄에 경로를 쓰는 방식(core.quotePath=true 의 C 따옴표)을 그대로 흉내 낸다.
// 큰따옴표·역슬래시·제어 문자·0x7f·0x80 이상이 있으면 전체를 따옴표로 감싸고 바이트를 이스케이프한다.
func quoteGitPath(path string) string {
	named := map[byte]string{'\a': `\a`, '\b': `\b`, '\t': `\t`, '\n': `\n`, '\v': `\v`, '\f': `\f`, '\r': `\r`, '"': `\"`, '\\': `\\`}
	needsQuote := false
	for index := 0; index < len(path); index++ {
		character := path[index]
		if character < 0x20 || character == '"' || character == '\\' || character >= 0x7f {
			needsQuote = true
			break
		}
	}
	if !needsQuote {
		return path
	}
	var builder strings.Builder
	builder.WriteByte('"')
	for index := 0; index < len(path); index++ {
		character := path[index]
		if escaped, found := named[character]; found {
			builder.WriteString(escaped)
		} else if character < 0x20 || character >= 0x7f {
			builder.WriteString(fmt.Sprintf("\\%03o", character))
		} else {
			builder.WriteByte(character)
		}
	}
	builder.WriteByte('"')
	return builder.String()
}

// parseRawEntries — `:모드 모드 객체 객체 상태 NUL 경로 NUL [새 경로 NUL]` 의 나열.
func parseRawEntries(output []byte, overflowed bool) ([]rawFileEntry, error) {
	tokens := strings.Split(string(output), "\x00")
	completeTokens := len(tokens) - 1
	entries := []rawFileEntry{}
	for index := 0; index < completeTokens; {
		header := strings.Fields(tokens[index])
		if len(header) != 5 || !strings.HasPrefix(header[0], ":") {
			return nil, errUnexpectedGitOutput
		}
		statusLetter := header[4][:1]
		pathCount := 1
		if statusLetter == "R" || statusLetter == "C" {
			pathCount = 2
		}
		if index+pathCount >= completeTokens { // 경로가 다 오지 않았다
			if overflowed {
				break
			}
			return nil, errUnexpectedGitOutput
		}
		entry := rawFileEntry{path: tokens[index+pathCount], status: gitFileStatusModified, typeChanged: statusLetter == "T"}
		switch statusLetter {
		case "A", "C":
			entry.status = gitFileStatusAdded
		case "D":
			entry.status = gitFileStatusDeleted
		case "R":
			previousPath := tokens[index+1]
			entry.status, entry.previousPath = gitFileStatusRenamed, &previousPath
		}
		entries = append(entries, entry)
		index += 1 + pathCount
	}
	if !overflowed && tokens[completeTokens] != "" {
		return nil, errUnexpectedGitOutput
	}
	return entries, nil
}

// parseNumstat — `더한 TAB 뺀 TAB 경로 NUL`, 이름 바꾸기는 `더한 TAB 뺀 TAB NUL 옛 경로 NUL 새 경로 NUL`. 바이너리는 "-".
func parseNumstat(output []byte, overflowed bool) ([]fileLineCount, error) {
	tokens := strings.Split(string(output), "\x00")
	completeTokens := len(tokens) - 1
	counts := []fileLineCount{}
	for index := 0; index < completeTokens; {
		fields := strings.SplitN(tokens[index], "\t", 3)
		if len(fields) != 3 {
			return nil, errUnexpectedGitOutput
		}
		consumed := 1
		if fields[2] == "" { // 경로 둘이 뒤에 따로 온다
			consumed = 3
		}
		if index+consumed > completeTokens {
			if overflowed {
				break
			}
			return nil, errUnexpectedGitOutput
		}
		count := fileLineCount{binary: fields[0] == "-" && fields[1] == "-"}
		if !count.binary {
			additions, additionsErr := strconv.Atoi(fields[0])
			deletions, deletionsErr := strconv.Atoi(fields[1])
			if additionsErr != nil || deletionsErr != nil {
				return nil, errUnexpectedGitOutput
			}
			count.additions, count.deletions = additions, deletions
		}
		counts = append(counts, count)
		index += consumed
	}
	if !overflowed && tokens[completeTokens] != "" {
		return nil, errUnexpectedGitOutput
	}
	return counts, nil
}

// splitPatchSections — patch 출력을 조각마다(`diff --git ` 줄에서 시작) 나누고, 머리줄과 첫 `@@` 부터의 내용을 남긴다.
// 내용 줄은 언제나 ' '·'+'·'-'·'\' 로 시작하므로 `diff --git ` 으로 시작하는 줄은 조각의 머리뿐이다.
// 출력이 상한에서 잘렸으면 마지막 조각은 온전하다고 볼 수 없어 뺀다.
func splitPatchSections(output []byte, overflowed bool) []patchSection {
	sections := []patchSection{}
	var current *patchSection
	var hunks strings.Builder
	inHunks := false
	flush := func() {
		if current != nil {
			current.hunks = hunks.String()
			sections = append(sections, *current)
		}
	}
	for _, line := range strings.SplitAfter(string(output), "\n") {
		if strings.HasPrefix(line, "diff --git ") {
			flush()
			current, inHunks = &patchSection{header: strings.TrimSuffix(line, "\n")}, false
			hunks.Reset()
			continue
		}
		if current == nil {
			continue
		}
		if !inHunks && !strings.HasPrefix(line, "@@") {
			continue
		}
		inHunks = true
		hunks.WriteString(line)
	}
	flush()
	if overflowed && len(sections) > 0 {
		sections = sections[:len(sections)-1]
	}
	return sections
}

// limitPatch — patch 를 gitPatchLineLimit 줄까지, 한 줄은 gitPatchLineByteLimit 바이트까지(넘는 줄은 잘라 표시). 잘랐으면 true.
func limitPatch(patch string) (string, bool) {
	var builder strings.Builder
	truncated := false
	lineCount := 0
	for _, line := range strings.SplitAfter(patch, "\n") {
		if line == "" {
			continue
		}
		if lineCount == gitPatchLineLimit {
			truncated = true
			break
		}
		content, ending := strings.CutSuffix(line, "\n")
		if shortened, cut := cutBytes(content, gitPatchLineByteLimit); cut {
			content, truncated = shortened+gitLineCutMarker, true
		}
		builder.WriteString(content)
		if ending {
			builder.WriteString("\n")
		}
		lineCount++
	}
	return builder.String(), truncated
}

// limitResponseSize — 최종 JSON(봉투 포함)이 gitResponseByteLimit 를 넘으면 뒤 파일부터 patch 를 비우고 patch_truncated.
// patch 를 다 비우고도 넘으면(경로만으로 큰 경우) 뒤 파일부터 빼고 files_truncated.
func limitResponseSize(detail *GitCommitDetail) {
	encoded, err := json.Marshal(detail)
	if err != nil {
		return
	}
	size := len(encoded) + gitEnvelopeAllowance
	for index := len(detail.Files) - 1; index >= 0 && size > gitResponseByteLimit; index-- {
		file := &detail.Files[index]
		if file.Patch == nil || *file.Patch == "" {
			continue
		}
		encodedPatch, _ := json.Marshal(*file.Patch)
		size -= len(encodedPatch) - len(`""`)
		emptied := ""
		file.Patch, file.PatchTruncated = &emptied, true
	}
	for len(detail.Files) > 0 && size > gitResponseByteLimit {
		last := len(detail.Files) - 1
		encodedFile, _ := json.Marshal(detail.Files[last])
		size -= len(encodedFile) + len(",")
		detail.Files = detail.Files[:last]
		detail.FilesTruncated = true
	}
}

// cutBytes — limit 바이트 이하로 자른다(UTF-8 글자 중간에서 자르지 않는다). 잘랐으면 true.
func cutBytes(value string, limit int) (string, bool) {
	if len(value) <= limit {
		return value, false
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(value[cut]) {
		cut--
	}
	return value[:cut], true
}
