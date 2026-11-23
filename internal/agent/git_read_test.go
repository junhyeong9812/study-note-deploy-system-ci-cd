package agent

// git 조회 계약 테스트 — 기준: docs/cross-cutting/plans/2026-11-22/admin-skeleton/requirement-spec.md §12(12.7 우선).
// 구현을 보지 않고 명세에서 썼다(blind). 받침은 git_repository_test.go.

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// linearRepository — c1…cN 일렬 커밋. shas[0] = c1.
func linearRepository(t *testing.T, count int) (*testRepository, []string) {
	t.Helper()
	repository := newTestRepository(t)
	var shas []string
	for index := 1; index <= count; index++ {
		repository.writeFile("history.txt", "c"+strconv.Itoa(index)+"\n")
		shas = append(shas, repository.commitMessage("c"+strconv.Itoa(index)))
	}
	return repository, shas
}

func rawData(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var envelope struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("봉투가 아니다: %v %.300s", err, body)
	}
	return envelope.Data
}

func patchLines(patch string) []string {
	return strings.Split(strings.TrimSuffix(patch, "\n"), "\n")
}

// ===== GET /agent/git/commits =====

func TestGitCommitsListsRangeExcludingFromIncludingToNewestFirst(t *testing.T) {
	repository, shas := linearRepository(t, 8)
	agent := newGitReadAgent(t, map[string]string{"llm": repository.directory})
	result := requireGitSuccess[GitCommitRange](t, gitGet(agent, commitsTarget("llm", shas[2], shas[6])))
	want := []string{shas[6], shas[5], shas[4], shas[3]} // c7, c6, c5, c4 — c3(from) 제외, c7(to) 포함
	if strings.Join(commitShas(result.Commits), ",") != strings.Join(want, ",") {
		t.Fatalf("범위·순서가 다르다:\n got %v\nwant %v", commitShas(result.Commits), want)
	}
	if result.Relation != "ancestor" || result.Truncated || !result.RangeComplete {
		t.Fatalf("relation=ancestor·truncated=false·range_complete=true 여야 한다: %+v", result)
	}
	for index, commit := range result.Commits {
		wantSubject := "c" + strconv.Itoa(7-index)
		if commit.Subject != wantSubject || commit.AuthorName != gitTestAuthorName ||
			commit.AuthoredAt != repository.authoredAt[commit.Sha] || commit.Body != "" || commit.BodyTruncated {
			t.Fatalf("커밋 칸이 다르다(want subject %s, authored_at %s): %+v", wantSubject, repository.authoredAt[commit.Sha], commit)
		}
	}
}

func TestGitCommitsAuthoredAtIsUtc(t *testing.T) {
	repository, shas := linearRepository(t, 2)
	agent := newGitReadAgent(t, map[string]string{"llm": repository.directory})
	result := requireGitSuccess[GitCommitRange](t, gitGet(agent, commitsTarget("llm", shas[0], shas[1])))
	// 두 번째 커밋 = 2026-01-02T12:01:00+09:00 → UTC 03:01:00Z
	if len(result.Commits) != 1 || result.Commits[0].AuthoredAt != "2026-01-02T03:01:00Z" {
		t.Fatalf("authored_at 은 ISO-8601 UTC(Z) 여야 한다: %+v", result.Commits)
	}
}

func TestGitCommitsAcceptsShortShasAndAnswersFullShas(t *testing.T) {
	repository, shas := linearRepository(t, 5)
	agent := newGitReadAgent(t, map[string]string{"llm": repository.directory})
	result := requireGitSuccess[GitCommitRange](t, gitGet(agent, commitsTarget("llm", shas[1][:7], shas[3][:7])))
	if strings.Join(commitShas(result.Commits), ",") != shas[3]+","+shas[2] {
		t.Fatalf("약식 SHA(7자) 범위가 다르거나 40자로 확정되지 않았다: %v", commitShas(result.Commits))
	}
}

func TestGitCommitsKeepsHundredNearestToWhenMoreExist(t *testing.T) {
	repository := newTestRepository(t)
	base := repository.commitMessage("base")
	var shas []string
	for index := 1; index <= 105; index++ {
		shas = append(shas, repository.commitMessage("n"+strconv.Itoa(index)))
	}
	agent := newGitReadAgent(t, map[string]string{"llm": repository.directory})
	result := requireGitSuccess[GitCommitRange](t, gitGet(agent, commitsTarget("llm", base, shas[104])))
	if len(result.Commits) != 100 || !result.Truncated {
		t.Fatalf("100개 + truncated:true 여야 한다: %d개 truncated=%v", len(result.Commits), result.Truncated)
	}
	if result.Commits[0].Sha != shas[104] || result.Commits[99].Sha != shas[5] {
		t.Fatalf("B(to) 쪽 100개여야 한다: 처음 %s(want n105) 끝 %s(want n6)", result.Commits[0].Subject, result.Commits[99].Subject)
	}
	if result.Relation != "ancestor" || !result.RangeComplete {
		t.Fatalf("얕지 않은 저장소의 잘림은 range_complete 와 무관하다: %+v", result.Relation)
	}

	exact := requireGitSuccess[GitCommitRange](t, gitGet(agent, commitsTarget("llm", shas[4], shas[104])))
	if len(exact.Commits) != 100 || exact.Truncated {
		t.Fatalf("정확히 100개면 truncated:false 여야 한다: %d개 truncated=%v", len(exact.Commits), exact.Truncated)
	}
}

func TestGitCommitsReportsNotAncestorForRolledBackOrDivergedDeploy(t *testing.T) {
	repository, shas := linearRepository(t, 7)
	repository.git("checkout", "-q", "-b", "side", shas[1])
	repository.writeFile("side.txt", "x\n")
	diverged := repository.commitMessage("x1")
	repository.git("checkout", "-q", "main")
	agent := newGitReadAgent(t, map[string]string{"llm": repository.directory})
	for name, pair := range map[string][2]string{
		"되돌린 배포(A=c7, B=c3)": {shas[6], shas[2]},
		"갈라진 이력(A=x1, B=c7)": {diverged, shas[6]},
	} {
		recorder := gitGet(agent, commitsTarget("llm", pair[0], pair[1]))
		result := requireGitSuccess[GitCommitRange](t, recorder)
		if result.Relation != "not_ancestor" || result.Truncated {
			t.Fatalf("%s: relation=not_ancestor 여야 한다: %+v", name, result)
		}
		commits, isList := rawData(t, recorder.Body.Bytes())["commits"].([]any)
		if !isList || len(commits) != 0 {
			t.Fatalf("%s: commits 는 빈 배열([])이어야 한다(null 아님): %.300s", name, recorder.Body.String())
		}
	}
}

func TestGitCommitsSameShaIsEmptyAncestorRange(t *testing.T) {
	repository, shas := linearRepository(t, 3)
	agent := newGitReadAgent(t, map[string]string{"llm": repository.directory})
	recorder := gitGet(agent, commitsTarget("llm", shas[1], shas[1]))
	result := requireGitSuccess[GitCommitRange](t, recorder)
	commits, isList := rawData(t, recorder.Body.Bytes())["commits"].([]any)
	if result.Relation != "ancestor" || !isList || len(commits) != 0 || result.Truncated || !result.RangeComplete {
		t.Fatalf("A==B 는 빈 목록·ancestor 여야 한다: %.300s", recorder.Body.String())
	}
}

func TestGitCommitsMarksRangeIncompleteWhenShallowBoundaryIsListed(t *testing.T) {
	_, shallow, shas := shallowMergeHistory(t)
	agent := newGitReadAgent(t, map[string]string{"llm": shallow.directory})

	result := requireGitSuccess[GitCommitRange](t, gitGet(agent, commitsTarget("llm", shas["m18"], shas["m22"])))
	listed := strings.Join(commitShas(result.Commits), ",")
	if result.Relation != "ancestor" || result.RangeComplete || !strings.Contains(listed, shas["s13"]) {
		t.Fatalf("얕은 경계(s13)가 목록에 들면 range_complete:false 여야 한다: relation=%s complete=%v", result.Relation, result.RangeComplete)
	}
	if len(result.Commits) == 0 || result.Commits[0].Sha != shas["m22"] {
		t.Fatalf("B 쪽이 먼저여야 한다: %v", commitShas(result.Commits))
	}
	if strings.Contains(listed, shas["m18"]) {
		t.Fatal("A(m18)는 목록에서 빠져야 한다")
	}

	inside := requireGitSuccess[GitCommitRange](t, gitGet(agent, commitsTarget("llm", shas["m19"], shas["m20"])))
	if strings.Join(commitShas(inside.Commits), ",") != shas["m20"] || !inside.RangeComplete {
		t.Fatalf("경계를 지나지 않는 범위는 range_complete:true 여야 한다: %+v", inside)
	}
}

func TestGitCommitsAnswersNotFoundForShaOutsideLocalHistory(t *testing.T) {
	_, shallow, shas := shallowMergeHistory(t)
	agent := newGitReadAgent(t, map[string]string{"llm": shallow.directory})
	requireGitFailure(t, gitGet(agent, commitsTarget("llm", shas["m10"], shas["m22"])), 404, "commit_not_found") // 얕은 이력 밖
	requireGitFailure(t, gitGet(agent, commitsTarget("llm", shas["m20"], gitMissingSha)), 404, "commit_not_found")
	requireGitFailure(t, gitGet(agent, commitsTarget("llm", shas["m10"][:7], shas["m22"])), 404, "commit_not_found")
}

func TestGitCommitsAnswersNotFoundForNonCommitObjects(t *testing.T) {
	repository, shas := linearRepository(t, 3)
	tree := repository.git("rev-parse", shas[2]+"^{tree}")
	blob := repository.git("rev-parse", shas[2]+":history.txt")
	agent := newGitReadAgent(t, map[string]string{"llm": repository.directory})
	for name, pair := range map[string][2]string{
		"from=트리": {tree, shas[2]}, "to=트리": {shas[0], tree},
		"from=blob": {blob, shas[2]}, "to=blob": {shas[0], blob},
	} {
		recorder := gitGet(agent, commitsTarget("llm", pair[0], pair[1]))
		if recorder.Code != 404 || !strings.Contains(recorder.Body.String(), `"commit_not_found"`) {
			t.Fatalf("%s: 커밋이 아닌 객체는 404 commit_not_found 여야 한다: %d %.300s", name, recorder.Code, recorder.Body.String())
		}
	}
}

func TestGitReadNeverExposesAuthorEmail(t *testing.T) {
	repository, shas := linearRepository(t, 3)
	agent := newGitReadAgent(t, map[string]string{"llm": repository.directory})
	for _, target := range []string{commitsTarget("llm", shas[0], shas[2]), commitTarget("llm", shas[2])} {
		recorder := gitGet(agent, target)
		if recorder.Code != 200 {
			t.Fatalf("%s: 200 이어야 한다: %d %.300s", target, recorder.Code, recorder.Body.String())
		}
		body := recorder.Body.String()
		if strings.Contains(body, gitTestEmailDomain) || strings.Contains(body, "secret-author") || strings.Contains(body, "secret-committer") {
			t.Fatalf("%s: 이메일이 응답에 있다: %.500s", target, body)
		}
	}
}

func TestGitCommitsLimitsSubjectAndBody(t *testing.T) {
	repository := newTestRepository(t)
	base := repository.commitMessage("base")
	longSubject := strings.Repeat("x", 600)
	longBody := strings.Repeat("b", 10000)
	long := repository.commitMessage(longSubject + "\n\n" + longBody)
	normal := repository.commitMessage("normal subject\n\nLine one\nLine two")
	agent := newGitReadAgent(t, map[string]string{"llm": repository.directory})
	result := requireGitSuccess[GitCommitRange](t, gitGet(agent, commitsTarget("llm", base, normal)))
	if len(result.Commits) != 2 || result.Commits[0].Sha != normal || result.Commits[1].Sha != long {
		t.Fatalf("목록이 다르다: %v", commitShas(result.Commits))
	}
	normalCommit, longCommit := result.Commits[0], result.Commits[1]
	if normalCommit.Subject != "normal subject" || strings.TrimRight(normalCommit.Body, "\n") != "Line one\nLine two" || normalCommit.BodyTruncated {
		t.Fatalf("보통 커밋의 제목·본문이 글자 그대로가 아니다: %q %q truncated=%v", normalCommit.Subject, normalCommit.Body, normalCommit.BodyTruncated)
	}
	if len(longCommit.Subject) == 0 || len(longCommit.Subject) > 500 || !strings.HasPrefix(longSubject, longCommit.Subject) {
		t.Fatalf("제목은 500바이트 이하의 앞부분이어야 한다: %d바이트", len(longCommit.Subject))
	}
	if len(longCommit.Body) == 0 || len(longCommit.Body) > 8192 || !strings.HasPrefix(longBody, longCommit.Body) || !longCommit.BodyTruncated {
		t.Fatalf("본문은 8KB 이하로 자르고 body_truncated:true 여야 한다: %d바이트 truncated=%v", len(longCommit.Body), longCommit.BodyTruncated)
	}
}

// ===== GET /agent/git/commit =====

func TestGitCommitDescribesAddedModifiedDeletedAndRenamedFiles(t *testing.T) {
	repository := newTestRepository(t)
	repository.writeFile("keep.txt", "one\ntwo\n")
	repository.writeFile("gone.txt", "a\nb\n")
	repository.writeFile("move.txt", "moving line one\nmoving line two\nmoving line three\n")
	repository.commitMessage("base")
	repository.writeFile("new.txt", "n1\nn2\nn3\n")
	repository.writeFile("keep.txt", "one\nTWO\n")
	repository.removeFile("gone.txt")
	repository.git("mv", "move.txt", "moved.txt")
	sha := repository.commitMessage("change four files\n\nwhy it changed")
	agent := newGitReadAgent(t, map[string]string{"llm": repository.directory})
	detail := requireGitSuccess[GitCommitDetail](t, gitGet(agent, commitTarget("llm", sha)))

	if detail.Sha != sha || detail.Subject != "change four files" || strings.TrimRight(detail.Body, "\n") != "why it changed" ||
		detail.AuthorName != gitTestAuthorName || detail.AuthoredAt != repository.authoredAt[sha] {
		t.Fatalf("커밋 요약이 다르다: %+v", detail.GitCommit)
	}
	if len(detail.Files) != 4 || detail.FilesTruncated {
		t.Fatalf("파일 4개·files_truncated:false 여야 한다: %d %v", len(detail.Files), detail.FilesTruncated)
	}
	type expectation struct {
		status               string
		previousPath         string
		additions, deletions int
	}
	for path, want := range map[string]expectation{
		"new.txt":   {"added", "", 3, 0},
		"keep.txt":  {"modified", "", 1, 1},
		"gone.txt":  {"deleted", "", 0, 2},
		"moved.txt": {"renamed", "move.txt", 0, 0},
	} {
		file := findFile(t, detail, path)
		previous := ""
		if file.PreviousPath != nil {
			previous = *file.PreviousPath
		}
		if file.Status != want.status || previous != want.previousPath || (want.previousPath == "") != (file.PreviousPath == nil) ||
			file.Additions != want.additions || file.Deletions != want.deletions || file.Binary || file.PatchTruncated {
			t.Fatalf("%s: want %+v, got status=%s previous=%v +%d -%d binary=%v truncated=%v",
				path, want, file.Status, file.PreviousPath, file.Additions, file.Deletions, file.Binary, file.PatchTruncated)
		}
	}
	added := findFile(t, detail, "new.txt")
	modified := findFile(t, detail, "keep.txt")
	if added.Patch == nil || modified.Patch == nil {
		t.Fatal("글자 파일의 patch 는 null 이 아니어야 한다")
	}
	// patch 는 GitHub 의 patch 와 같은 꼴 — 첫 줄이 구간 머리(@@), 파일 머리줄(diff --git·---·+++)은 없다(어드민 diffLines 가 그대로 읽는다)
	for path, patch := range map[string]string{"new.txt": *added.Patch, "keep.txt": *modified.Patch} {
		lines := patchLines(patch)
		if !strings.HasPrefix(lines[0], "@@ ") {
			t.Fatalf("%s: patch 첫 줄은 @@ 구간 머리여야 한다: %q", path, patch)
		}
		for _, line := range lines {
			if strings.HasPrefix(line, "diff --git") || strings.HasPrefix(line, "+++ ") || strings.HasPrefix(line, "--- ") || strings.HasPrefix(line, "index ") {
				t.Fatalf("%s: patch 에 파일 머리줄이 들어 있다: %q", path, patch)
			}
		}
	}
	if !strings.Contains(*added.Patch, "+n1\n+n2\n+n3") || !strings.Contains(*modified.Patch, "-two\n+TWO") {
		t.Fatalf("patch 내용이 다르다:\n%s\n%s", *added.Patch, *modified.Patch)
	}
}

func TestGitCommitMarksBinaryFilesWithoutPatch(t *testing.T) {
	repository := newTestRepository(t)
	repository.commitMessage("base")
	repository.writeFile("image.bin", string([]byte{0x89, 'P', 'N', 'G', 0, 0, 0, 13, 0, 1, 2, 3, 255, 254, 0}))
	sha := repository.commitMessage("add binary")
	agent := newGitReadAgent(t, map[string]string{"llm": repository.directory})
	recorder := gitGet(agent, commitTarget("llm", sha))
	detail := requireGitSuccess[GitCommitDetail](t, recorder)
	file := findFile(t, detail, "image.bin")
	if !file.Binary || file.Patch != nil || file.Status != "added" {
		t.Fatalf("바이너리는 binary:true·patch:null 이어야 한다: %+v", file)
	}
	files := rawData(t, recorder.Body.Bytes())["files"].([]any)
	rawFile := files[0].(map[string]any)
	if patch, present := rawFile["patch"]; !present || patch != nil {
		t.Fatalf("patch 칸이 null 로 있어야 한다: %v", rawFile)
	}
}

func TestGitCommitOfMergeShowsDifferenceFromFirstParent(t *testing.T) {
	repository := newTestRepository(t)
	repository.writeFile("a.txt", "a\n")
	base := repository.commitMessage("base")
	repository.git("checkout", "-q", "-b", "side", base)
	repository.writeFile("side.txt", "side\n")
	repository.commitMessage("side work")
	repository.git("checkout", "-q", "main")
	repository.writeFile("a.txt", "a2\n")
	repository.commitMessage("main work")
	merge := repository.record("merge", "-q", "--no-ff", "-m", "merge side", "side")
	agent := newGitReadAgent(t, map[string]string{"llm": repository.directory})
	detail := requireGitSuccess[GitCommitDetail](t, gitGet(agent, commitTarget("llm", merge)))
	if len(detail.Files) != 1 || detail.Files[0].Path != "side.txt" || detail.Files[0].Status != "added" || detail.Files[0].Additions != 1 {
		t.Fatalf("병합 커밋은 첫째 부모와의 차이(side.txt 추가)만이어야 한다: %+v", detail.Files)
	}
}

func TestGitCommitReadsPathsWithTabsQuotesSpacesAndKorean(t *testing.T) {
	paths := []string{"탭\t이름.txt", "공백 이름.txt", "따옴표\"이름.txt", "한글.txt", "줄\n바꿈.txt", "역슬래시\\이름.txt", "디렉터리/안 쪽.txt"}
	repository := newTestRepository(t)
	repository.commitMessage("base")
	for index, path := range paths {
		repository.writeFile(path, "content "+strconv.Itoa(index)+"\nsecond line\nthird line\n")
	}
	added := repository.commitMessage("add odd paths")
	renamedTo := "새\t\"이름\" 바뀐.txt"
	repository.git("mv", "공백 이름.txt", renamedTo)
	renamed := repository.commitMessage("rename odd path")
	agent := newGitReadAgent(t, map[string]string{"llm": repository.directory})

	detail := requireGitSuccess[GitCommitDetail](t, gitGet(agent, commitTarget("llm", added)))
	if len(detail.Files) != len(paths) {
		t.Fatalf("파일 %d개여야 한다: %d", len(paths), len(detail.Files))
	}
	for _, path := range paths {
		if file := findFile(t, detail, path); file.Status != "added" || file.Additions != 3 {
			t.Fatalf("%q: added +3 이어야 한다: %+v", path, file)
		}
	}
	renameDetail := requireGitSuccess[GitCommitDetail](t, gitGet(agent, commitTarget("llm", renamed)))
	file := findFile(t, renameDetail, renamedTo)
	if len(renameDetail.Files) != 1 || file.Status != "renamed" || file.PreviousPath == nil || *file.PreviousPath != "공백 이름.txt" {
		t.Fatalf("이름 바꾸기의 경로·previous_path 가 글자 그대로가 아니다: %+v", renameDetail.Files)
	}
}

func TestGitCommitKeepsHundredFilesAndMarksTruncated(t *testing.T) {
	repository := newTestRepository(t)
	repository.commitMessage("base")
	for index := 0; index < 105; index++ {
		repository.writeFile("files/f"+strconv.Itoa(1000+index)+".txt", "line\n")
	}
	many := repository.commitMessage("105 files")
	repository.writeFile("files/f1000.txt", "line\nmore\n")
	repository.commitMessage("one more")
	agent := newGitReadAgent(t, map[string]string{"llm": repository.directory})
	detail := requireGitSuccess[GitCommitDetail](t, gitGet(agent, commitTarget("llm", many)))
	if len(detail.Files) != 100 || !detail.FilesTruncated {
		t.Fatalf("파일 100개 + files_truncated:true 여야 한다: %d %v", len(detail.Files), detail.FilesTruncated)
	}
}

func TestGitCommitWithExactlyHundredFilesIsNotTruncated(t *testing.T) {
	repository := newTestRepository(t)
	repository.commitMessage("base")
	for index := 0; index < 100; index++ {
		repository.writeFile("files/f"+strconv.Itoa(1000+index)+".txt", "line\n")
	}
	sha := repository.commitMessage("100 files")
	agent := newGitReadAgent(t, map[string]string{"llm": repository.directory})
	detail := requireGitSuccess[GitCommitDetail](t, gitGet(agent, commitTarget("llm", sha)))
	if len(detail.Files) != 100 || detail.FilesTruncated {
		t.Fatalf("정확히 100개면 files_truncated:false 여야 한다: %d %v", len(detail.Files), detail.FilesTruncated)
	}
}

func TestGitCommitLimitsPatchToFiveHundredLines(t *testing.T) {
	repository := newTestRepository(t)
	repository.commitMessage("base")
	var content strings.Builder
	for index := 0; index < 600; index++ {
		content.WriteString("row " + strconv.Itoa(index) + "\n")
	}
	repository.writeFile("long.txt", content.String())
	repository.writeFile("short.txt", "only\n")
	sha := repository.commitMessage("600 lines")
	agent := newGitReadAgent(t, map[string]string{"llm": repository.directory})
	detail := requireGitSuccess[GitCommitDetail](t, gitGet(agent, commitTarget("llm", sha)))
	long := findFile(t, detail, "long.txt")
	if long.Patch == nil || !long.PatchTruncated || len(patchLines(*long.Patch)) > 500 || !strings.HasPrefix(*long.Patch, "@@ ") {
		t.Fatalf("patch 는 500줄 이하 + patch_truncated:true 여야 한다: truncated=%v", long.PatchTruncated)
	}
	if long.Additions != 600 {
		t.Fatalf("additions 는 patch 를 잘라도 실제 줄 수(600)여야 한다: %d", long.Additions)
	}
	if short := findFile(t, detail, "short.txt"); short.PatchTruncated || short.Patch == nil {
		t.Fatalf("짧은 파일은 자르지 않는다: %+v", short)
	}
}

func TestGitCommitShortensVeryLongLine(t *testing.T) {
	repository := newTestRepository(t)
	repository.commitMessage("base")
	repository.writeFile("minified.js", strings.Repeat("z", 10000)+"\n")
	sha := repository.commitMessage("one very long line")
	agent := newGitReadAgent(t, map[string]string{"llm": repository.directory})
	detail := requireGitSuccess[GitCommitDetail](t, gitGet(agent, commitTarget("llm", sha)))
	file := findFile(t, detail, "minified.js")
	if file.Patch == nil || !strings.Contains(*file.Patch, "+zzzz") {
		t.Fatalf("긴 줄의 앞부분은 보여야 한다: %+v", file)
	}
	for _, line := range patchLines(*file.Patch) {
		if len(line) > 2000+64 { // 2000바이트 + 잘림 표시 몫
			t.Fatalf("patch 한 줄이 2000바이트를 넘는다: %d바이트", len(line))
		}
	}
}

func TestGitCommitKeepsWholeResponseUnderTwoMegabytes(t *testing.T) {
	repository := newTestRepository(t)
	repository.commitMessage("base")
	// 제어 문자 \x01 은 JSON 에서 6바이트(\u0001)로 늘어난다(git 은 NUL 이 없으면 글자 파일로 본다)
	// — git 출력 상한(2MB) 안이어도 최종 JSON 은 2MB 를 넘게 만든다.
	line := strings.Repeat("\x01", 119) + "\n"
	for index := 0; index < 40; index++ {
		repository.writeFile("big/file"+strconv.Itoa(100+index)+".txt", strings.Repeat(line, 600)) // 40 × 600줄 × 120바이트 ≈ 2.9MB
	}
	sha := repository.commitMessage("huge diff")
	agent := newGitReadAgent(t, map[string]string{"llm": repository.directory})
	recorder := gitGet(agent, commitTarget("llm", sha))
	if recorder.Body.Len() > 2<<20 {
		t.Fatalf("최종 JSON 은 2MB 이하여야 한다: %d바이트", recorder.Body.Len())
	}
	detail := requireGitSuccess[GitCommitDetail](t, recorder)
	anyTruncation := detail.FilesTruncated
	for _, file := range detail.Files {
		if file.PatchTruncated {
			anyTruncation = true
		}
		if file.Additions != 600 || file.Status != "added" {
			t.Fatalf("%s: 실린 파일의 칸은 사실이어야 한다(added +600): %+v", file.Path, file)
		}
	}
	if !anyTruncation || len(detail.Files) == 0 {
		t.Fatalf("잘랐으면 알려야 한다(files_truncated 또는 patch_truncated): 파일 %d개", len(detail.Files))
	}
}

func TestGitCommitAnswersParentUnavailableForShallowBoundary(t *testing.T) {
	_, shallow, shas := shallowMergeHistory(t)
	agent := newGitReadAgent(t, map[string]string{"llm": shallow.directory})
	requireGitFailure(t, gitGet(agent, commitTarget("llm", shas["m18"])), 404, "parent_unavailable")
	requireGitFailure(t, gitGet(agent, commitTarget("llm", shas["s13"])), 404, "parent_unavailable")
	detail := requireGitSuccess[GitCommitDetail](t, gitGet(agent, commitTarget("llm", shas["m22"])))
	if len(detail.Files) != 1 || detail.Files[0].Path != "main.txt" || detail.Files[0].Status != "modified" {
		t.Fatalf("경계가 아닌 커밋은 정상 diff 여야 한다: %+v", detail.Files)
	}
	requireGitFailure(t, gitGet(agent, commitTarget("llm", shas["m10"])), 404, "commit_not_found") // 이력 밖
}

func TestGitCommitOfRootCommitListsAddedFiles(t *testing.T) {
	repository := newTestRepository(t)
	repository.writeFile("README.md", "hello\nworld\n")
	root := repository.commitMessage("root")
	agent := newGitReadAgent(t, map[string]string{"llm": repository.directory})
	detail := requireGitSuccess[GitCommitDetail](t, gitGet(agent, commitTarget("llm", root)))
	if len(detail.Files) != 1 || detail.Files[0].Status != "added" || detail.Files[0].Additions != 2 {
		t.Fatalf("진짜 첫 커밋(얕은 경계 아님)은 추가로 보여야 한다: %+v", detail.Files)
	}
}

func TestGitCommitAnswersNotFoundAndAcceptsShortSha(t *testing.T) {
	repository, shas := linearRepository(t, 2)
	tree := repository.git("rev-parse", shas[1]+"^{tree}")
	blob := repository.git("rev-parse", shas[1]+":history.txt")
	agent := newGitReadAgent(t, map[string]string{"llm": repository.directory})
	for _, sha := range []string{gitMissingSha, gitMissingSha[:7], tree, blob} {
		requireGitFailure(t, gitGet(agent, commitTarget("llm", sha)), 404, "commit_not_found")
	}
	detail := requireGitSuccess[GitCommitDetail](t, gitGet(agent, commitTarget("llm", shas[1][:7])))
	if detail.Sha != shas[1] {
		t.Fatalf("약식 SHA 는 40자로 확정돼야 한다: %q", detail.Sha)
	}
}

func TestGitReadResponsesCarryEveryContractField(t *testing.T) {
	repository, shas := linearRepository(t, 2)
	agent := newGitReadAgent(t, map[string]string{"llm": repository.directory})
	commits := rawData(t, gitGet(agent, commitsTarget("llm", shas[0], shas[1])).Body.Bytes())
	for _, key := range []string{"commits", "truncated", "range_complete", "relation"} {
		if _, present := commits[key]; !present {
			t.Fatalf("commits 응답에 %s 칸이 없다: %v", key, commits)
		}
	}
	commitList, _ := commits["commits"].([]any)
	if len(commitList) != 1 {
		t.Fatalf("커밋 1개여야 한다: %v", commits)
	}
	for _, key := range []string{"sha", "author_name", "authored_at", "subject", "body", "body_truncated"} {
		if _, present := commitList[0].(map[string]any)[key]; !present {
			t.Fatalf("커밋에 %s 칸이 없다: %v", key, commitList[0])
		}
	}
	detail := rawData(t, gitGet(agent, commitTarget("llm", shas[1])).Body.Bytes())
	for _, key := range []string{"sha", "author_name", "authored_at", "subject", "body", "body_truncated", "files", "files_truncated"} {
		if _, present := detail[key]; !present {
			t.Fatalf("commit 응답에 %s 칸이 없다: %v", key, detail)
		}
	}
	files, _ := detail["files"].([]any)
	if len(files) != 1 {
		t.Fatalf("파일 1개여야 한다: %v", detail)
	}
	file := files[0].(map[string]any)
	for _, key := range []string{"path", "previous_path", "status", "additions", "deletions", "binary", "patch", "patch_truncated"} {
		if _, present := file[key]; !present {
			t.Fatalf("파일에 %s 칸이 없다: %v", key, file)
		}
	}
	if file["previous_path"] != nil {
		t.Fatalf("이름을 바꾸지 않은 파일의 previous_path 는 null 이어야 한다: %v", file)
	}
}

// ===== 공통: 인증 · 입력 검사 · 메서드 =====

func TestGitReadRequiresSecretBeforeCheckingInput(t *testing.T) {
	repository, shas := linearRepository(t, 2)
	agent := newGitReadAgent(t, map[string]string{"llm": repository.directory})
	invocations := installRecordingGit(t, agent)
	targets := []string{
		commitsTarget("llm", shas[0], shas[1]), commitTarget("llm", shas[1]),
		commitsTarget("other", "HEAD", "a..b"), "/agent/git/commit?sha=--output=x", "/agent/git/commits",
	}
	for _, target := range targets {
		for _, secret := range []string{"", "wrong-value"} {
			recorder := gitCall(agent, http.MethodGet, target, secret)
			requireGitFailure(t, recorder, 401, "unauthorized")
		}
	}
	if recorded := invocations(); len(recorded) != 0 {
		t.Fatalf("인증 전에 git 을 불렀다: %v", recorded)
	}
}

func TestGitReadRejectsInvalidInputWithoutRunningGit(t *testing.T) {
	repository, shas := linearRepository(t, 2)
	from, to := shas[0], shas[1]
	agent := newGitReadAgent(t, map[string]string{"llm": repository.directory})
	invocations := installRecordingGit(t, agent)
	outputPath := filepath.Join(t.TempDir(), "x")
	optionLike := "--output=" + outputPath
	cases := map[string]string{
		"commits 서비스 허용 목록 밖":      "/agent/git/commits?" + gitQuery("service", "other", "from", from, "to", to),
		"commits 서비스 설정 없음(front)": "/agent/git/commits?" + gitQuery("service", "front", "from", from, "to", to),
		"commits 서비스 경로 글자":        "/agent/git/commits?" + gitQuery("service", "../llm", "from", from, "to", to),
		"commits from 옵션 꼴":        "/agent/git/commits?" + gitQuery("service", "llm", "from", optionLike, "to", to),
		"commits to 옵션 꼴":          "/agent/git/commits?" + gitQuery("service", "llm", "from", from, "to", optionLike),
		"commits from=HEAD":        "/agent/git/commits?" + gitQuery("service", "llm", "from", "HEAD", "to", to),
		"commits to=a..b 범위":       "/agent/git/commits?" + gitQuery("service", "llm", "from", from, "to", from[:7]+".."+to[:7]),
		"commits 대문자 hex":          "/agent/git/commits?" + gitQuery("service", "llm", "from", strings.ToUpper(from), "to", to),
		"commits 6자":               "/agent/git/commits?" + gitQuery("service", "llm", "from", from[:6], "to", to),
		"commits 빈 from":           "/agent/git/commits?" + gitQuery("service", "llm", "from", "", "to", to),
		"commits from 없음":          "/agent/git/commits?" + gitQuery("service", "llm", "to", to),
		"commits to 없음":            "/agent/git/commits?" + gitQuery("service", "llm", "from", from),
		"commits service 없음":       "/agent/git/commits?" + gitQuery("from", from, "to", to),
		"commits from 중복":          "/agent/git/commits?" + gitQuery("service", "llm", "from", from, "from", from, "to", to),
		"commits service 중복":       "/agent/git/commits?" + gitQuery("service", "llm", "service", "llm", "from", from, "to", to),
		"commits 빈 쿼리":             "/agent/git/commits",
		"commit 서비스 허용 목록 밖":       "/agent/git/commit?" + gitQuery("service", "other", "sha", to),
		"commit 옵션 꼴":              "/agent/git/commit?" + gitQuery("service", "llm", "sha", optionLike),
		"commit sha=HEAD":          "/agent/git/commit?" + gitQuery("service", "llm", "sha", "HEAD"),
		"commit sha=HEAD~1":        "/agent/git/commit?" + gitQuery("service", "llm", "sha", "HEAD~1"),
		"commit sha 범위":            "/agent/git/commit?" + gitQuery("service", "llm", "sha", from[:7]+".."+to[:7]),
		"commit 대문자 hex":           "/agent/git/commit?" + gitQuery("service", "llm", "sha", strings.ToUpper(to)),
		"commit sha^{tree} 꼴":      "/agent/git/commit?" + gitQuery("service", "llm", "sha", to+"^{tree}"),
		"commit 빈 sha":             "/agent/git/commit?" + gitQuery("service", "llm", "sha", ""),
		"commit sha 없음":            "/agent/git/commit?" + gitQuery("service", "llm"),
		"commit sha 중복":            "/agent/git/commit?" + gitQuery("service", "llm", "sha", to, "sha", to),
		"commit service 없음":        "/agent/git/commit?" + gitQuery("sha", to),
	}
	for name, target := range cases {
		recorder := gitGet(agent, target)
		if recorder.Code != 422 || !strings.Contains(recorder.Body.String(), `"invalid_request"`) {
			t.Fatalf("%s: 422 invalid_request 여야 한다: %d %.300s", name, recorder.Code, recorder.Body.String())
		}
	}
	if recorded := invocations(); len(recorded) != 0 {
		t.Fatalf("틀린 입력인데 git 을 불렀다(입력이 git 에 넘어갔을 수 있다): %v", recorded)
	}
	if _, err := os.Stat(outputPath); !os.IsNotExist(err) {
		t.Fatalf("옵션처럼 생긴 입력으로 파일이 생겼다: %s", outputPath)
	}
}

func TestGitReadRejectsNonGetMethods(t *testing.T) {
	repository, shas := linearRepository(t, 2)
	agent := newGitReadAgent(t, map[string]string{"llm": repository.directory})
	for _, target := range []string{commitsTarget("llm", shas[0], shas[1]), commitTarget("llm", shas[1])} {
		for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
			if recorder := gitCall(agent, method, target, gitTestSecret); recorder.Code != http.StatusMethodNotAllowed {
				t.Fatalf("%s %s: 405 여야 한다: %d", method, target, recorder.Code)
			}
		}
	}
}

func TestGitReadAcceptsAdminService(t *testing.T) {
	repository, shas := linearRepository(t, 3)
	agent := newGitReadAgent(t, map[string]string{"admin": repository.directory})
	if agent.directories["admin"] != repository.directory {
		t.Fatalf("DEPLOY_DIR_ADMIN 을 허용 목록으로 읽지 않았다: %v", agent.directories)
	}
	result := requireGitSuccess[GitCommitRange](t, gitGet(agent, commitsTarget("admin", shas[0], shas[2])))
	if len(result.Commits) != 2 {
		t.Fatalf("admin 저장소의 범위를 읽지 못했다: %+v", result)
	}
	requireGitSuccess[GitCommitDetail](t, gitGet(agent, commitTarget("admin", shas[2])))
}

// ===== 읽기만 =====

// readQueries — 정상·오류 갈래를 고루 지나는 조회 묶음.
func readQueries(shas map[string]string, sourceOnly string) []string {
	return []string{
		commitsTarget("llm", shas["m18"], shas["m22"]),
		commitsTarget("llm", shas["m19"], shas["m20"]),
		commitsTarget("llm", shas["m22"], shas["m19"]),
		commitsTarget("llm", sourceOnly, shas["m22"]),
		commitTarget("llm", shas["m22"]),
		commitTarget("llm", shas["m21"]),
		commitTarget("llm", shas["m18"]),
		commitTarget("llm", sourceOnly),
		commitTarget("llm", shas["m20"][:7]),
	}
}

func TestGitReadLeavesRepositoryUnchanged(t *testing.T) {
	_, shallow, shas := shallowMergeHistory(t)
	agent := newGitReadAgent(t, map[string]string{"llm": shallow.directory})
	before := snapshotTree(t, shallow.directory)
	for round := 0; round < 3; round++ {
		for _, target := range readQueries(shas, shas["m10"]) {
			if recorder := gitGet(agent, target); recorder.Code >= 500 {
				t.Fatalf("%s: %d %.300s", target, recorder.Code, recorder.Body.String())
			}
		}
	}
	requireSameSnapshot(t, before, snapshotTree(t, shallow.directory))
}

func TestGitReadRunsOnlyReadCommandsWithFixedEnvironment(t *testing.T) {
	_, shallow, shas := shallowMergeHistory(t)
	agent := newGitReadAgent(t, map[string]string{"llm": shallow.directory})
	invocations := installRecordingGit(t, agent)
	for _, target := range readQueries(shas, shas["m10"]) {
		if recorder := gitGet(agent, target); recorder.Code >= 500 {
			t.Fatalf("%s: %d %.300s", target, recorder.Code, recorder.Body.String())
		}
	}
	recorded := invocations()
	if len(recorded) == 0 {
		t.Fatal("agent.gitProgram 으로 git 을 부르지 않았다(이음매 계약)")
	}
	allowed := map[string]bool{"rev-parse": true, "cat-file": true, "log": true, "show": true, "diff-tree": true, "merge-base": true, "rev-list": true}
	requiredEnvironment := map[string]string{"LC_ALL": "C", "GIT_OPTIONAL_LOCKS": "0", "GIT_NO_LAZY_FETCH": "1",
		"GIT_NO_REPLACE_OBJECTS": "1", "GIT_TERMINAL_PROMPT": "0"}
	for _, invocation := range recorded {
		subcommand := gitSubcommand(invocation.arguments)
		if !allowed[subcommand] {
			t.Fatalf("읽기 명령이 아닌 git 을 불렀다(%q): %q", subcommand, invocation.arguments)
		}
		if !containsArgument(invocation.arguments, "--no-pager") && !containsArgument(invocation.arguments, "-P") {
			t.Fatalf("--no-pager 없이 불렀다: %q", invocation.arguments)
		}
		for name, value := range requiredEnvironment {
			if invocation.environment[name] != value {
				t.Fatalf("%s=%s 로 돌아야 한다(got %q): %q", name, value, invocation.environment[name], invocation.arguments)
			}
		}
		// SHA 는 옵션 끝 표시 뒤에만 온다
		endOfOptions := indexOfArgument(invocation.arguments, "--end-of-options")
		for index, argument := range invocation.arguments {
			if containsAnySha(argument, shas) && (endOfOptions < 0 || index < endOfOptions) {
				t.Fatalf("SHA 가 --end-of-options 앞에 있다: %q", invocation.arguments)
			}
		}
	}
}

func containsArgument(arguments []string, wanted string) bool {
	return indexOfArgument(arguments, wanted) >= 0
}

func indexOfArgument(arguments []string, wanted string) int {
	for index, argument := range arguments {
		if argument == wanted {
			return index
		}
	}
	return -1
}

func containsAnySha(argument string, shas map[string]string) bool {
	for _, sha := range shas {
		if strings.Contains(argument, sha[:7]) {
			return true
		}
	}
	return false
}

func TestGitReadIgnoresParentGitEnvironment(t *testing.T) {
	_, shallow, shas := shallowMergeHistory(t)
	other, _ := linearRepository(t, 2) // 새면 결과가 달라지는 다른 저장소
	agent := newGitReadAgent(t, map[string]string{"llm": shallow.directory})
	targets := readQueries(shas, shas["m10"])
	baseline := map[string]string{}
	for _, target := range targets {
		recorder := gitGet(agent, target)
		if recorder.Code >= 500 {
			t.Fatalf("기준 조회가 실패했다 — 비교할 결과가 없다: %s %d %.300s", target, recorder.Code, recorder.Body.String())
		}
		baseline[target] = recorder.Body.String()
	}

	markerDirectory := t.TempDir()
	marker := filepath.Join(markerDirectory, "ran")
	markerScript := filepath.Join(t.TempDir(), "marker.sh")
	if err := os.WriteFile(markerScript, []byte("#!/bin/sh\necho ran >> '"+marker+"'\ncat >/dev/null\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	traceFile := filepath.Join(markerDirectory, "trace")
	trace2File := filepath.Join(markerDirectory, "trace2")
	t.Setenv("GIT_DIR", filepath.Join(other.directory, ".git"))
	t.Setenv("GIT_WORK_TREE", other.directory)
	t.Setenv("GIT_OBJECT_DIRECTORY", filepath.Join(other.directory, ".git", "objects"))
	t.Setenv("GIT_TRACE", traceFile)
	t.Setenv("GIT_TRACE2", trace2File)
	t.Setenv("GIT_CONFIG_COUNT", "2")
	t.Setenv("GIT_CONFIG_KEY_0", "diff.noprefix")
	t.Setenv("GIT_CONFIG_VALUE_0", "true")
	t.Setenv("GIT_CONFIG_KEY_1", "diff.external")
	t.Setenv("GIT_CONFIG_VALUE_1", markerScript)
	t.Setenv("GIT_CONFIG_PARAMETERS", "'diff.mnemonicprefix'='true'")
	t.Setenv("GIT_EXTERNAL_DIFF", markerScript)
	t.Setenv("GIT_PAGER", markerScript)
	t.Setenv("PAGER", markerScript)

	for _, target := range targets {
		if got := gitGet(agent, target).Body.String(); got != baseline[target] {
			t.Fatalf("%s: 부모의 GIT_* 에 따라 결과가 달라졌다\nbefore %.300s\nafter  %.300s", target, baseline[target], got)
		}
	}
	for _, path := range []string{traceFile, trace2File, marker} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("부모 환경이 git 에 새었다 — %s 가 생겼다", path)
		}
	}
}

// ===== 배포를 막지 않는다 =====

func TestGitReadAnswersWhileDeployLockIsHeld(t *testing.T) {
	repository, shas := linearRepository(t, 3)
	agent := newGitReadAgent(t, map[string]string{"llm": repository.directory})
	agent.busy.Lock()
	defer agent.busy.Unlock()
	for _, target := range []string{commitsTarget("llm", shas[0], shas[2]), commitTarget("llm", shas[2])} {
		answered := make(chan int, 1)
		go func() { answered <- gitGet(agent, target).Code }()
		select {
		case code := <-answered:
			if code != 200 {
				t.Fatalf("%s: 배포 잠금이 잡혀 있어도 200 이어야 한다: %d", target, code)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: 배포 잠금을 기다리고 있다(조회가 busy 를 잡으면 안 된다)", target)
		}
	}
}

func TestDeployIsNotBusyWhileGitReadRuns(t *testing.T) {
	repository, shas := linearRepository(t, 3)
	agent := newGitReadAgent(t, map[string]string{"llm": repository.directory})
	agent.composeServices["llm"] = "wrapper"
	agent.run = (&recordedCommands{}).run // CI 는 가짜 실행기로 — 여기서 보는 것은 잠금뿐
	agent.gitTimeout = 2 * time.Second
	pidFile := installHangingGit(t, agent)

	finished := make(chan int, 1)
	go func() { finished <- gitGet(agent, commitsTarget("llm", shas[0], shas[2])).Code }()
	waitForPids(t, pidFile, 1)

	if recorder := post(agent, "/agent/ci"); recorder.Code != 200 {
		t.Fatalf("조회가 도는 중에 CI 가 막혔다(agent_busy 이면 조회가 잠금을 잡은 것): %d %s", recorder.Code, recorder.Body.String())
	}
	select {
	case code := <-finished:
		if code != 504 {
			t.Fatalf("멈춘 조회는 504 여야 한다: %d", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("조회가 기한 안에 끝나지 않았다")
	}
}

func TestGitReadRefusesThirdConcurrentReadImmediately(t *testing.T) {
	repository, shas := linearRepository(t, 3)
	agent := newGitReadAgent(t, map[string]string{"llm": repository.directory})
	agent.gitTimeout = 2 * time.Second
	pidFile := installHangingGit(t, agent)

	var running sync.WaitGroup
	codes := make(chan int, 2)
	for _, target := range []string{commitsTarget("llm", shas[0], shas[2]), commitTarget("llm", shas[2])} {
		running.Add(1)
		go func(target string) {
			defer running.Done()
			codes <- gitGet(agent, target).Code
		}(target)
	}
	waitForPids(t, pidFile, 2)

	startedAt := time.Now()
	third := gitGet(agent, commitTarget("llm", shas[1]))
	if elapsed := time.Since(startedAt); elapsed > time.Second {
		t.Fatalf("3번째 조회가 기다렸다(%v) — 즉시 429 여야 한다", elapsed)
	}
	requireGitFailure(t, third, 429, "too_many_requests")
	if pids := readPids(pidFile); len(pids) != 2 {
		t.Fatalf("거절한 조회가 git 을 불렀다: %v", pids)
	}

	running.Wait()
	close(codes)
	for code := range codes {
		if code != 504 {
			t.Fatalf("멈춘 조회는 504 여야 한다: %d", code)
		}
	}
	agent.gitProgram = "git" // 자리가 풀렸는지 — 실제 git 으로 다시 부른다
	requireGitSuccess[GitCommitDetail](t, gitGet(agent, commitTarget("llm", shas[1])))
}

func TestGitReadTimesOutAndKillsGitProcess(t *testing.T) {
	repository, shas := linearRepository(t, 2)
	agent := newGitReadAgent(t, map[string]string{"llm": repository.directory})
	agent.gitTimeout = 300 * time.Millisecond
	pidFile := installHangingGit(t, agent)
	for _, target := range []string{commitsTarget("llm", shas[0], shas[1]), commitTarget("llm", shas[1])} {
		startedAt := time.Now()
		recorder := gitGet(agent, target)
		elapsed := time.Since(startedAt)
		requireGitFailure(t, recorder, 504, "git_timeout")
		if strings.Contains(recorder.Body.String(), `"detail"`) {
			t.Fatalf("git_timeout 에 detail 을 내지 않는다: %s", recorder.Body.String())
		}
		if elapsed > 3*time.Second {
			t.Fatalf("기한(300ms)이 지나고도 %v 걸렸다", elapsed)
		}
	}
	pids := readPids(pidFile)
	if len(pids) == 0 {
		t.Fatal("git 을 부르지 않았다")
	}
	deadline := time.Now().Add(2 * time.Second)
	for _, pid := range pids {
		for !processGone(pid) {
			if time.Now().After(deadline) {
				t.Fatalf("기한이 지난 git 자식 프로세스(%d)가 남아 있다", pid)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
}
