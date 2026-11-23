package agent

// 출력 상한·레코드 단위 자르기 — 기준: requirement-spec.md §12.8(앞 절보다 우선).
// 500줄 상한과 겹치지 않는 사례로 고정한다: 형 바뀜 patch 의 짝, 파일 목록과 patch 의 분리, 두 응답의 최종 JSON 2MB.

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// commitMessageFile — 메시지를 파일로 넘겨 글자 그대로(--cleanup=verbatim) 커밋한다.
func (repository *testRepository) commitMessageFile(message string) string {
	repository.t.Helper()
	path := filepath.Join(repository.t.TempDir(), "message")
	if err := os.WriteFile(path, []byte(message), 0o644); err != nil {
		repository.t.Fatal(err)
	}
	repository.git("add", "-A")
	return repository.record("commit", "-q", "--allow-empty", "--cleanup=verbatim", "-F", path)
}

// requireOwnPatch — patch 가 있으면 자기 표식만 담고 남의 표식은 담지 않는다.
func requireOwnPatch(t *testing.T, file GitFileChange, own string, others ...string) {
	t.Helper()
	if file.Patch == nil {
		return
	}
	if own != "" && *file.Patch != "" && !strings.Contains(*file.Patch, own) {
		t.Fatalf("%s: patch 에 자기 내용(%q)이 없다: %.200q", file.Path, own, *file.Patch)
	}
	for _, other := range others {
		if strings.Contains(*file.Patch, other) {
			t.Fatalf("%s: patch 에 다른 파일의 내용(%q)이 들어 있다(밀림): %.200q", file.Path, other, *file.Patch)
		}
	}
}

// ===== 형 바뀜(파일 ↔ 심볼릭 링크) =====

func TestGitCommitShowsTypeChangeAsOneModifiedFileWithBothPatchPieces(t *testing.T) {
	repository := newTestRepository(t)
	repository.writeFile("a.txt", "hello\n")
	repository.writeFile("b.txt", "bravo-old\n")
	repository.commitMessage("base")
	repository.removeFile("a.txt")
	if err := os.Symlink("target", filepath.Join(repository.directory, "a.txt")); err != nil {
		t.Fatal(err)
	}
	repository.writeFile("b.txt", "bravo-new\n") // 형 바뀜 파일 뒤의 다른 수정 파일
	sha := repository.commitMessage("file to symlink")
	agent := newGitReadAgent(t, map[string]string{"llm": repository.directory})
	detail := requireGitSuccess[GitCommitDetail](t, gitGet(agent, commitTarget("llm", sha)))
	if len(detail.Files) != 2 || detail.FilesTruncated {
		t.Fatalf("파일 2개(a.txt·b.txt)여야 한다: %+v", detail.Files)
	}
	changed := findFile(t, detail, "a.txt")
	if changed.Status != "modified" || changed.Patch == nil || changed.PatchTruncated {
		t.Fatalf("형 바뀜은 modified 한 파일 + 두 조각 patch 여야 한다: %+v", changed)
	}
	if !strings.Contains(*changed.Patch, "-hello") || !strings.Contains(*changed.Patch, "+target") {
		t.Fatalf("형 바뀜 patch 에 두 조각(-hello · +target)이 모두 있어야 한다: %q", *changed.Patch)
	}
	requireOwnPatch(t, changed, "hello", "bravo")
	other := findFile(t, detail, "b.txt")
	if other.Status != "modified" || other.Patch == nil || other.PatchTruncated ||
		!strings.Contains(*other.Patch, "-bravo-old") || !strings.Contains(*other.Patch, "+bravo-new") {
		t.Fatalf("형 바뀜 뒤의 파일이 자기 patch 를 가져야 한다(밀림 없음): %+v", other)
	}
	requireOwnPatch(t, other, "bravo", "hello", "target")
}

// ===== 파일 목록은 patch 와 따로 =====

func TestGitCommitKeepsFileListWhenPatchOutputOverflows(t *testing.T) {
	wideLine := strings.Repeat("q", 6000) + "\n"
	bigContent := strings.Repeat(wideLine, 400) // 400줄 × 6000바이트 ≈ 2.4MB — 500줄 상한에는 안 걸리고 출력 상한(2MB)에만 걸린다
	for name, layout := range map[string]struct{ before, big, after string }{
		"큰 파일이 가운데": {"a_small.txt", "m_big.txt", "z_small.txt"},
		"큰 파일이 앞":   {"", "a_big.txt", "m_small.txt"},
	} {
		repository := newTestRepository(t)
		repository.commitMessage("base")
		if layout.before != "" {
			repository.writeFile(layout.before, "alpha-marker\n")
		}
		repository.writeFile(layout.big, bigContent)
		repository.writeFile(layout.after, "omega-marker\n")
		if layout.before == "" {
			repository.writeFile("z_small.txt", "zulu-marker\n")
		}
		sha := repository.commitMessage("big patch")
		agent := newGitReadAgent(t, map[string]string{"llm": repository.directory})
		detail := requireGitSuccess[GitCommitDetail](t, gitGet(agent, commitTarget("llm", sha)))
		if len(detail.Files) != 3 || detail.FilesTruncated {
			t.Fatalf("%s: 파일 목록은 patch 가 넘쳐도 셋 다 실려야 한다(files_truncated:false): %d %v", name, len(detail.Files), detail.FilesTruncated)
		}
		big := findFile(t, detail, layout.big)
		if !big.PatchTruncated || big.Status != "added" || big.Additions != 400 {
			t.Fatalf("%s: 큰 파일은 patch_truncated:true 이고 목록 칸(added +400)은 사실이어야 한다: %+v", name, big)
		}
		requireOwnPatch(t, big, "", "alpha-marker", "omega-marker", "zulu-marker")
		if layout.before != "" {
			small := findFile(t, detail, layout.before)
			if small.Patch == nil || small.PatchTruncated || !strings.Contains(*small.Patch, "+alpha-marker") {
				t.Fatalf("%s: 큰 파일 앞의 파일은 온전히 읽었으니 자기 patch 를 가져야 한다: %+v", name, small)
			}
			requireOwnPatch(t, small, "alpha-marker", "qqqq", "omega-marker")
		}
		for _, path := range []string{layout.after, "z_small.txt"} {
			if path == layout.after || layout.before == "" {
				after := findFile(t, detail, path)
				if after.Status != "added" || after.Additions != 1 {
					t.Fatalf("%s: %s 의 목록 칸이 사실이 아니다: %+v", name, path, after)
				}
				if !after.PatchTruncated {
					t.Fatalf("%s: 큰 파일 뒤에서 patch 를 온전히 읽지 못한 %s 는 patch_truncated:true 여야 한다: %+v", name, path, after)
				}
				requireOwnPatch(t, after, "", "alpha-marker", "qqqq")
			}
		}
	}
}

// 파일 목록 출력 자체가 상한(2MB)을 넘는 커밋 — 파일이 100개를 넘어야만 만들 수 있다(경로 하나가 4KB 아래라 100개로는 ~400KB).
// 그래서 100개 상한과 겹친다: 여기서 고정하는 것은 "목록 출력이 넘쳐도 실패(500)가 아니라 files_truncated 로 답한다"이다.
func TestGitCommitAnswersFilesTruncatedWhenFileListOutputOverflows(t *testing.T) {
	repository := newTestRepository(t)
	repository.commitMessage("base")
	var directory []string
	for depth := 0; depth < 13; depth++ {
		directory = append(directory, strings.Repeat(string(rune('a'+depth)), 250))
	}
	deep := filepath.Join(directory...)
	for index := 0; index < 700; index++ { // 700 × ~3.5KB ≈ 2.4MB 의 경로 — 목록 출력이 2MB 를 넘는다
		repository.writeFile(filepath.Join(deep, strconv.Itoa(100000+index)+strings.Repeat("n", 200)+".txt"), "x\n")
	}
	sha := repository.commitMessage("many long paths")
	agent := newGitReadAgent(t, map[string]string{"llm": repository.directory})
	recorder := gitGet(agent, commitTarget("llm", sha))
	detail := requireGitSuccess[GitCommitDetail](t, recorder)
	if len(detail.Files) != 100 || !detail.FilesTruncated || recorder.Body.Len() > 2<<20 {
		t.Fatalf("목록이 넘치면 100개 + files_truncated:true, 2MB 이하여야 한다: %d %v %d바이트", len(detail.Files), detail.FilesTruncated, recorder.Body.Len())
	}
	for _, file := range detail.Files {
		if !strings.HasPrefix(file.Path, deep+"/") || file.Status != "added" || file.Additions != 1 {
			t.Fatalf("실린 파일의 칸이 온전하지 않다: %.80q %+v", file.Path, file.Status)
		}
	}
}

// ===== 최종 JSON 2MB — 커밋 목록 =====

func TestGitCommitListKeepsWholeResponseUnderTwoMegabytes(t *testing.T) {
	repository := newTestRepository(t)
	base := repository.commitMessage("base")
	body := strings.Repeat("<", 8192) // 본문 상한(8KB) 안 — 그러나 JSON 에서 6배(<)로 늘어 100개면 ~4.9MB
	var shas []string
	for index := 1; index <= 100; index++ {
		shas = append(shas, repository.commitMessageFile("wide "+strconv.Itoa(index)+"\n\n"+body))
	}
	agent := newGitReadAgent(t, map[string]string{"llm": repository.directory})
	recorder := gitGet(agent, commitsTarget("llm", base, shas[99]))
	if recorder.Body.Len() > 2<<20 {
		t.Fatalf("커밋 목록의 최종 JSON 도 2MB 이하여야 한다: %d바이트", recorder.Body.Len())
	}
	result := requireGitSuccess[GitCommitRange](t, recorder)
	if !result.Truncated || len(result.Commits) == 0 || len(result.Commits) >= 100 {
		t.Fatalf("크기 때문에 뺐으면 truncated:true 여야 한다: %d개 truncated=%v", len(result.Commits), result.Truncated)
	}
	for index, commit := range result.Commits {
		if commit.Sha != shas[99-index] {
			t.Fatalf("B 쪽부터 연속이어야 한다(%d번째): %s", index, commit.Subject)
		}
		if commit.Subject != "wide "+strconv.Itoa(100-index) || strings.TrimRight(commit.Body, "\n") != body || commit.BodyTruncated {
			t.Fatalf("실린 레코드가 온전하지 않다(%d번째): subject=%q body %d바이트 truncated=%v", index, commit.Subject, len(commit.Body), commit.BodyTruncated)
		}
	}
}

func TestGitCommitsCutsAtWholeRecordWhenLogOutputOverflows(t *testing.T) {
	repository := newTestRepository(t)
	base := repository.commitMessage("base")
	body := strings.Repeat("a", 30<<10) // 본문은 응답에서만 8KB 로 자른다 — log 출력은 30KB × 80개 ≈ 2.4MB 로 상한(2MB)을 넘는다
	var shas []string
	for index := 1; index <= 80; index++ {
		shas = append(shas, repository.commitMessageFile("long "+strconv.Itoa(index)+"\n\n"+body))
	}
	agent := newGitReadAgent(t, map[string]string{"llm": repository.directory})
	result := requireGitSuccess[GitCommitRange](t, gitGet(agent, commitsTarget("llm", base, shas[79])))
	if !result.Truncated || len(result.Commits) == 0 || len(result.Commits) >= 80 {
		t.Fatalf("log 출력이 넘치면 온전한 레코드까지만 + truncated:true 여야 한다: %d개 truncated=%v", len(result.Commits), result.Truncated)
	}
	for index, commit := range result.Commits {
		if commit.Sha != shas[79-index] || commit.Subject != "long "+strconv.Itoa(80-index) || commit.AuthorName != gitTestAuthorName ||
			commit.AuthoredAt != repository.authoredAt[commit.Sha] {
			t.Fatalf("B 쪽부터 연속인 온전한 레코드여야 한다(%d번째): subject=%q", index, commit.Subject)
		}
		if !commit.BodyTruncated || len(commit.Body) == 0 || len(commit.Body) > 8192 || !strings.HasPrefix(body, commit.Body) {
			t.Fatalf("본문은 8KB 앞부분 + body_truncated:true 여야 한다(%d번째): %d바이트 %v", index, len(commit.Body), commit.BodyTruncated)
		}
	}
}

// ===== 최종 JSON 2MB — 상세(patch 를 다 비워도 넘는 경우) =====

func TestGitCommitDropsTrailingFilesWhenPathsAloneExceedTwoMegabytes(t *testing.T) {
	repository := newTestRepository(t)
	component := func(character byte) string { return strings.Repeat(string([]byte{character}), 250) }
	var oldParts []string
	for depth := 0; depth < 13; depth++ {
		oldParts = append(oldParts, component(0x01))
	}
	newTop := component(0x02)
	oldDirectory := filepath.Join(oldParts...)
	newDirectory := filepath.Join(append([]string{newTop}, oldParts[1:]...)...) // 맨 위 한 조각만 바뀐다
	for index := 0; index < 100; index++ {
		repository.writeFile(filepath.Join(oldDirectory, "file"+strconv.Itoa(1000+index)+".txt"), "same content "+strconv.Itoa(index)+"\nline two\nline three\n")
	}
	repository.commitMessage("base")
	repository.git("mv", oldParts[0], newTop) // 맨 위 디렉터리 이름 바꾸기 → 100개가 renamed(경로 + previous_path)
	sha := repository.commitMessage("rename long control-character paths")
	// 제어 문자 경로는 JSON 에서 6배 — 파일 하나의 path+previous_path 가 ~40KB, 100개면 ~4MB(patch 없이도 2MB 초과)
	agent := newGitReadAgent(t, map[string]string{"llm": repository.directory})
	recorder := gitGet(agent, commitTarget("llm", sha))
	if recorder.Body.Len() > 2<<20 {
		t.Fatalf("patch 를 다 비워도 넘으면 파일을 빼서 2MB 이하로 맞춰야 한다: %d바이트", recorder.Body.Len())
	}
	detail := requireGitSuccess[GitCommitDetail](t, recorder)
	if !detail.FilesTruncated || len(detail.Files) == 0 || len(detail.Files) >= 100 {
		t.Fatalf("뒤 파일부터 빼고 files_truncated:true 여야 한다: %d개 %v", len(detail.Files), detail.FilesTruncated)
	}
	for _, file := range detail.Files {
		base := filepath.Base(file.Path)
		if file.Status != "renamed" || file.Path != newDirectory+"/"+base || file.PreviousPath == nil || *file.PreviousPath != oldDirectory+"/"+base {
			t.Fatalf("실린 파일이 온전하지 않다: status=%s base=%s path 일치=%v previous 일치=%v", file.Status, base,
				file.Path == newDirectory+"/"+base, file.PreviousPath != nil && *file.PreviousPath == oldDirectory+"/"+base)
		}
	}
}
