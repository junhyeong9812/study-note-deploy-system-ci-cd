package agent

// patch 조각과 파일의 짝 · diff 인자 고정 — 기준: requirement-spec.md §12.8(짝을 확정하지 못하면 비우고 patch_truncated) · §12.9.
// git 감싸개가 --patch 출력에서 조각을 빼거나 끼워 "짝이 어긋난 출력" 을 만든다.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// patchRewritingHook — --patch 호출만 실제 git 을 돌린 뒤 awk 로 출력을 바꿔 내고 끝낸다(그 밖의 호출은 그대로 넘긴다).
func patchRewritingHook(awkProgram string) string {
	return "case \" $* \" in *\" --patch \"*) '" + "REALGIT" + "' \"$@\" > \"$state/patch.out\"; status=$?; " +
		"'" + "AWK" + "' '" + awkProgram + "' \"$state/patch.out\"; exit $status;; esac"
}

// installPatchRewritingGit — 돌려준 함수는 감싸개가 --patch 출력을 실제로 바꿨는지 확인한다(받침 확인).
func installPatchRewritingGit(t *testing.T, agent *Agent, awkProgram string) func() {
	t.Helper()
	hook := strings.ReplaceAll(strings.ReplaceAll(patchRewritingHook(awkProgram), "REALGIT", absoluteProgram(t, "git")), "AWK", absoluteProgram(t, "awk"))
	state := installHookedGit(t, agent, hook)
	return func() {
		if _, err := os.Stat(filepath.Join(state, "patch.out")); err != nil {
			t.Fatal("받침 확인: --patch 호출이 없어 출력을 바꾸지 못했다")
		}
	}
}

var pairingMarkers = map[string]string{"a.txt": "alpha-marker", "b.txt": "bravo-marker", "c.txt": "charlie-marker"}

func pairingRepository(t *testing.T) (*testRepository, string) {
	repository := newTestRepository(t)
	repository.commitMessage("base")
	for path, marker := range pairingMarkers {
		repository.writeFile(path, marker+"\n")
	}
	return repository, repository.commitMessage("three files")
}

func requireNoForeignPatch(t *testing.T, detail GitCommitDetail, intruder string) {
	t.Helper()
	if len(detail.Files) != 3 {
		t.Fatalf("파일 셋이 목록에 있어야 한다: %+v", detail.Files)
	}
	for _, file := range detail.Files {
		if file.Patch == nil || *file.Patch == "" {
			continue
		}
		for path, marker := range pairingMarkers {
			if path != file.Path && strings.Contains(*file.Patch, marker) {
				t.Fatalf("%s 의 patch 가 %s 의 내용을 담았다(짝 밀림): %q", file.Path, path, *file.Patch)
			}
		}
		if intruder != "" && strings.Contains(*file.Patch, intruder) {
			t.Fatalf("%s 의 patch 가 끼워 넣은 엉뚱한 조각을 담았다: %q", file.Path, *file.Patch)
		}
		if !strings.Contains(*file.Patch, pairingMarkers[file.Path]) {
			t.Fatalf("%s 의 patch 에 자기 내용이 없다: %q", file.Path, *file.Patch)
		}
	}
}

func TestGitCommitDoesNotShiftPatchesWhenASectionIsMissing(t *testing.T) {
	repository, sha := pairingRepository(t)
	agent := newGitReadAgent(t, map[string]string{"llm": repository.directory})
	rewritten := installPatchRewritingGit(t, agent, `/^diff --git /{section++} section!=1{print}`) // 첫 조각(a.txt)을 지운다
	detail := requireGitSuccess[GitCommitDetail](t, gitGet(agent, commitTarget("llm", sha)))
	rewritten()
	requireNoForeignPatch(t, detail, "")
	missing := findFile(t, detail, "a.txt")
	if (missing.Patch != nil && *missing.Patch != "") || !missing.PatchTruncated {
		t.Fatalf("짝을 확정하지 못한 파일(a.txt)은 patch 가 비고 patch_truncated:true 여야 한다: %+v", missing)
	}
}

func TestGitCommitDoesNotShiftPatchesWhenAForeignSectionIsInserted(t *testing.T) {
	repository, sha := pairingRepository(t)
	agent := newGitReadAgent(t, map[string]string{"llm": repository.directory})
	insert := `NR==1{print "diff --git a/bogus.txt b/bogus.txt"; print "new file mode 100644"; print "index 0000000..1111111"; ` +
		`print "--- /dev/null"; print "+++ b/bogus.txt"; print "@@ -0,0 +1 @@"; print "+intruder-marker"} {print}`
	rewritten := installPatchRewritingGit(t, agent, insert)
	detail := requireGitSuccess[GitCommitDetail](t, gitGet(agent, commitTarget("llm", sha)))
	rewritten()
	requireNoForeignPatch(t, detail, "intruder-marker")
	for _, file := range detail.Files {
		if (file.Patch == nil || *file.Patch == "") && !file.PatchTruncated {
			t.Fatalf("%s: patch 를 비웠으면 patch_truncated:true 여야 한다", file.Path)
		}
	}
}

func TestGitCommitDiffArgumentsFixSubmoduleAndDiffOptions(t *testing.T) {
	repository, sha := pairingRepository(t)
	agent := newGitReadAgent(t, map[string]string{"llm": repository.directory})
	state := installHookedGit(t, agent, "")
	requireGitSuccess[GitCommitDetail](t, gitGet(agent, commitTarget("llm", sha)))
	diffCalls := 0
	for _, arguments := range hookedInvocations(t, state) {
		subcommand := gitSubcommand(arguments)
		if subcommand != "show" && subcommand != "diff-tree" {
			continue
		}
		if containsArgument(arguments, "--no-patch") || containsArgument(arguments, "-s") {
			continue
		}
		diffCalls++
		for _, wanted := range []string{"--submodule=short", "--no-ext-diff", "--no-textconv", "-M", "--no-color", "--diff-merges=first-parent"} {
			if !containsArgument(arguments, wanted) {
				t.Fatalf("diff 를 만드는 git 호출에 %s 가 없다: %q", wanted, arguments)
			}
		}
	}
	if diffCalls == 0 {
		t.Fatal("diff 를 만드는 git 호출(show·diff-tree)이 없다")
	}
}
