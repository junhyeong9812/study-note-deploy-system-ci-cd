package agent

// 최종 JSON 2MB 는 실제 직렬화 길이로 — 기준: requirement-spec.md §12.9. 여러 개가 빠지는 경계 사례를 표로 고정하고,
// 상세의 "뒤 파일부터 patch 비우기" 를 500줄 상한·git 출력 상한과 겹치지 않는 사례로 고정한다.

import (
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

func TestGitCommitListStaysUnderTwoMegabytesWhenManyCommitsAreDropped(t *testing.T) {
	for _, shape := range []struct {
		name          string
		subjectLength func(index int) int
		bodyLength    func(index int) int
	}{
		{"본문 8192 '<'", func(int) int { return 10 }, func(int) int { return 8192 }},
		{"제목 500 '<' + 본문 8000 '<'", func(int) int { return 500 }, func(int) int { return 8000 }},
		{"본문 6000 '<'", func(int) int { return 10 }, func(int) int { return 6000 }},
		{"길이가 커밋마다 다름", func(index int) int { return 50 + index*37%450 }, func(index int) int { return 5000 + index*271%3192 }},
	} {
		repository := newTestRepository(t)
		base := repository.commitMessage("base")
		var shas []string
		for index := 1; index <= 100; index++ {
			subject := strings.Repeat("<", shape.subjectLength(index)-len(strconv.Itoa(index))) + strconv.Itoa(index)
			shas = append(shas, repository.commitMessageFile(subject+"\n\n"+strings.Repeat("<", shape.bodyLength(index))))
		}
		agent := newGitReadAgent(t, map[string]string{"llm": repository.directory})
		recorder := gitGet(agent, commitsTarget("llm", base, shas[99]))
		if recorder.Body.Len() > 2<<20 {
			t.Fatalf("%s: 응답 본문이 2MB 를 넘는다: %d바이트", shape.name, recorder.Body.Len())
		}
		result := requireGitSuccess[GitCommitRange](t, recorder)
		if !result.Truncated || len(result.Commits) > 60 || len(result.Commits) == 0 {
			t.Fatalf("%s: 40개 이상 빠지고 truncated:true 여야 한다: %d개 %v", shape.name, len(result.Commits), result.Truncated)
		}
		for index, commit := range result.Commits {
			if commit.Sha != shas[99-index] || !strings.HasSuffix(commit.Subject, strconv.Itoa(100-index)) {
				t.Fatalf("%s: B 쪽부터 연속이어야 한다(%d번째)", shape.name, index)
			}
		}
	}
}

func TestGitCommitStaysUnderTwoMegabytesWhenManyFilesAreDropped(t *testing.T) {
	for _, depth := range []int{13, 10} { // 제어 문자 경로 — path+previous_path 가 JSON 에서 파일당 ~40KB / ~31KB
		repository := newTestRepository(t)
		var parts []string
		for level := 0; level < depth; level++ {
			parts = append(parts, strings.Repeat("\x01", 250))
		}
		oldTop, newTop := parts[0], strings.Repeat("\x02", 250)
		var expected []string
		for index := 0; index < 100; index++ {
			name := "file" + strconv.Itoa(1000+index) + ".txt"
			repository.writeFile(filepath.Join(append(append([]string{}, parts...), name)...), "content "+strconv.Itoa(index)+"\nline two\nline three\n")
			expected = append(expected, filepath.Join(append(append([]string{newTop}, parts[1:]...), name)...))
		}
		repository.commitMessage("base")
		repository.git("mv", oldTop, newTop)
		sha := repository.commitMessage("rename")
		sort.Strings(expected)
		agent := newGitReadAgent(t, map[string]string{"llm": repository.directory})
		recorder := gitGet(agent, commitTarget("llm", sha))
		if recorder.Body.Len() > 2<<20 {
			t.Fatalf("깊이 %d: 응답 본문이 2MB 를 넘는다: %d바이트", depth, recorder.Body.Len())
		}
		detail := requireGitSuccess[GitCommitDetail](t, recorder)
		if !detail.FilesTruncated || len(detail.Files) == 0 || len(detail.Files) > 90 {
			t.Fatalf("깊이 %d: 여러 파일이 빠지고 files_truncated:true 여야 한다: %d개 %v", depth, len(detail.Files), detail.FilesTruncated)
		}
		for index, file := range detail.Files {
			if file.Path != expected[index] {
				t.Fatalf("깊이 %d: 앞에서부터 연속으로 실려야 한다(%d번째 %s)", depth, index, filepath.Base(file.Path))
			}
		}
	}
}

// 400줄 × (\x01 119바이트 + 줄바꿈) 파일 8개: git 출력 ~380KB(2MB 미만) · 500줄 상한 밑 · JSON 은 파일당 ~286KB 로 합 ~2.3MB.
// 그래서 줄일 수 있는 것은 "뒤 파일부터 patch 비우기" 하나뿐이다.
func TestGitCommitEmptiesTrailingPatchesFirstToFitTwoMegabytes(t *testing.T) {
	repository := newTestRepository(t)
	repository.commitMessage("base")
	content := strings.Repeat(strings.Repeat("\x01", 119)+"\n", 400)
	var paths []string
	for index := 0; index < 8; index++ {
		path := "wide" + strconv.Itoa(index) + ".txt"
		paths = append(paths, path)
		repository.writeFile(path, content)
	}
	sha := repository.commitMessage("eight wide files")
	agent := newGitReadAgent(t, map[string]string{"llm": repository.directory})
	recorder := gitGet(agent, commitTarget("llm", sha))
	if recorder.Body.Len() > 2<<20 {
		t.Fatalf("응답 본문이 2MB 를 넘는다: %d바이트", recorder.Body.Len())
	}
	detail := requireGitSuccess[GitCommitDetail](t, recorder)
	if len(detail.Files) != 8 || detail.FilesTruncated {
		t.Fatalf("파일 여덟 개 모두 목록에 있고 files_truncated:false 여야 한다: %d %v", len(detail.Files), detail.FilesTruncated)
	}
	firstEmptied := -1
	for index, path := range paths {
		file := findFile(t, detail, path)
		emptied := file.Patch == nil || *file.Patch == ""
		if emptied {
			if !file.PatchTruncated {
				t.Fatalf("%s: patch 를 비웠으면 patch_truncated:true 여야 한다", path)
			}
			if firstEmptied < 0 {
				firstEmptied = index
			}
			continue
		}
		if firstEmptied >= 0 {
			t.Fatalf("%s: 뒤 파일부터 비워야 한다 — 앞 파일(%s)이 비었는데 이 파일은 patch 가 있다", path, paths[firstEmptied])
		}
		if file.PatchTruncated || len(patchLines(*file.Patch)) != 401 || file.Additions != 400 {
			t.Fatalf("%s: 앞쪽 파일의 patch 는 온전해야 한다(머리 1줄 + 400줄, patch_truncated:false): %d줄 %v", path, len(patchLines(*file.Patch)), file.PatchTruncated)
		}
	}
	if firstEmptied <= 0 {
		t.Fatalf("앞쪽 파일은 온전하고 뒤쪽 파일만 비어야 한다: 처음 빈 파일 위치 %d", firstEmptied)
	}
}
