package agent

// 조회 하나는 변하지 않은 이력 위에서만 답한다 — 기준: requirement-spec.md §12.9(앞 절보다 우선).
// 얕은 경계 목록을 git 명령들 앞뒤로 비교해, 다르면 한 번 처음부터 다시 하고, 다시 해도 다르면 503 repository_changing.
// git 감싸개가 진짜 git 을 부르기 전에 저장소의 .git/shallow 를 고쳐 "조회 도중 배포의 fetch" 를 흉내 낸다.

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// installHookedGit — agent.gitProgram 을 "hook(셸 조각)을 돌린 뒤 실제 git 으로 넘기는" 스크립트로 바꾼다.
// hook 안에서 쓸 수 있는 것: "$@"(git 인자), $count(이번이 몇 번째 git 호출인지, 1부터), $state(호출 사이에 남는 디렉터리).
// 호출마다 인자를 $state/<번호>.arguments 에 NUL 로 남긴다.
func installHookedGit(t *testing.T, agent *Agent, hook string) (stateDirectory string) {
	t.Helper()
	realGit := absoluteProgram(t, "git")
	stateDirectory = t.TempDir()
	script := "#!/bin/sh\n" +
		"state='" + stateDirectory + "'\n" +
		"count=$(( $(cat \"$state/count\" 2>/dev/null || echo 0) + 1 ))\n" +
		"echo $count > \"$state/count\"\n" +
		"printf '%s\\0' \"$@\" > \"$state/$count.arguments\"\n" +
		hook + "\n" +
		"exec '" + realGit + "' \"$@\"\n"
	wrapper := filepath.Join(t.TempDir(), "git")
	if err := os.WriteFile(wrapper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	agent.gitProgram = wrapper
	return stateDirectory
}

func absoluteProgram(t *testing.T, name string) string {
	t.Helper()
	for _, directory := range filepath.SplitList(os.Getenv("PATH")) {
		candidate := filepath.Join(directory, name)
		if information, err := os.Stat(candidate); err == nil && !information.IsDir() {
			absolute, _ := filepath.Abs(candidate)
			return absolute
		}
	}
	t.Fatalf("%s 이 없다 — 이 테스트는 실제 %s 가 필요하다", name, name)
	return ""
}

// hookedInvocations — 감싸개가 남긴 호출 인자(호출 순서대로).
func hookedInvocations(t *testing.T, stateDirectory string) [][]string {
	t.Helper()
	var invocations [][]string
	for index := 1; ; index++ {
		content, err := os.ReadFile(filepath.Join(stateDirectory, strconv.Itoa(index)+".arguments"))
		if err != nil {
			return invocations
		}
		invocations = append(invocations, strings.Split(strings.TrimSuffix(string(content), "\x00"), "\x00"))
	}
}

// onceOnSubcommand — 그 하위 명령의 첫 호출 때 한 번만 command(셸)를 돌리는 hook.
func onceOnSubcommand(subcommand, command string) string {
	return "case \" $* \" in *\" " + subcommand + " \"*) if [ ! -e \"$state/done\" ]; then touch \"$state/done\"; " + command + "; fi;; esac"
}

func shallowFile(repository *testRepository) string {
	return filepath.Join(repository.directory, ".git", "shallow")
}

// ===== 상세 =====

func TestGitCommitRetriesWhenShallowBoundaryChangesOnceDuringRead(t *testing.T) {
	repository, shas := linearRepository(t, 5)
	target := shas[2] // c3 — 조회 도중 얕은 경계가 된다
	agent := newGitReadAgent(t, map[string]string{"llm": repository.directory})
	state := installHookedGit(t, agent, onceOnSubcommand("show", "echo "+target+" >> '"+shallowFile(repository)+"'"))
	recorder := gitGet(agent, commitTarget("llm", target))
	if recorder.Code == 200 {
		t.Fatalf("조회 도중 경계가 바뀌었는데 처음 경계로 답했다(전부 added 인 틀린 diff 일 수 있다): %.300s", recorder.Body.String())
	}
	requireGitFailure(t, recorder, 404, "parent_unavailable")
	if _, err := os.Stat(filepath.Join(state, "done")); err != nil {
		t.Fatal("받침 확인: 감싸개가 show 호출에서 경계를 바꾸지 않았다")
	}
}

// keepChangingHook — git 을 부를 때마다 .git/shallow 에 한 줄을 덧붙인다(내용이 늘 자라므로 앞뒤가 같아질 수 없다).
// 대상 커밋은 넣지 않는다(대상 자체의 판정은 바뀌지 않고 "이력이 바뀌는 중" 만 남게).
func keepChangingHook(repository *testRepository, candidates []string) string {
	return "line=$(( count % " + strconv.Itoa(len(candidates)) + " )); " +
		"i=0; for candidate in " + strings.Join(candidates, " ") + "; do if [ $i -eq $line ]; then echo $candidate >> '" + shallowFile(repository) + "'; fi; i=$((i+1)); done"
}

func TestGitCommitAnswersRepositoryChangingWhenBoundaryKeepsChanging(t *testing.T) {
	repository, shas := linearRepository(t, 6)
	agent := newGitReadAgent(t, map[string]string{"llm": repository.directory})
	installHookedGit(t, agent, keepChangingHook(repository, shas[:4]))
	recorder := gitGet(agent, commitTarget("llm", shas[5]))
	requireGitFailure(t, recorder, 503, "repository_changing")
}

// ===== 목록 =====

func TestGitCommitsRetriesWhenShallowBoundaryChangesOnceDuringRead(t *testing.T) {
	// 얕지 않은 저장소: main c1~c10, c2 에서 갈라진 x1. A=x1, B=c10.
	// 처음 경계(없음)로 판정하면 not_ancestor(이력이 온전) — 그러나 merge-base 첫 호출 때 c6 이 얕은 경계가 된다.
	// 바뀐 경계로 12.8 순서를 따르면: 조상 아님 → B 의 순회가 경계(c6)에 닿음 → B 는 A 의 조상 아님 → unknown.
	repository, shas := linearRepository(t, 10)
	repository.git("checkout", "-q", "-b", "side", shas[1])
	repository.writeFile("side.txt", "x\n")
	diverged := repository.commitMessage("x1")
	repository.git("checkout", "-q", "main")
	agent := newGitReadAgent(t, map[string]string{"llm": repository.directory})
	installHookedGit(t, agent, onceOnSubcommand("merge-base", "echo "+shas[5]+" >> '"+shallowFile(repository)+"'"))
	recorder := gitGet(agent, commitsTarget("llm", diverged, shas[9]))
	result := requireGitSuccess[GitCommitRange](t, recorder)
	if result.Relation != "unknown" || result.RangeComplete || len(result.Commits) != 0 {
		t.Fatalf("바뀐 뒤의 경계로 다시 판정해야 한다(unknown·range_complete:false·빈 목록): %.300s", recorder.Body.String())
	}
}

func TestGitCommitsAnswersRepositoryChangingWhenBoundaryKeepsChanging(t *testing.T) {
	repository, shas := linearRepository(t, 8)
	agent := newGitReadAgent(t, map[string]string{"llm": repository.directory})
	installHookedGit(t, agent, keepChangingHook(repository, shas[:3]))
	recorder := gitGet(agent, commitsTarget("llm", shas[4], shas[7]))
	requireGitFailure(t, recorder, 503, "repository_changing")
}

// ===== 바뀌지 않으면 한 번만 · 기한은 조회 전체에 =====

func TestGitReadRunsEachCommandOnceWhenNothingChanges(t *testing.T) {
	repository, shas := linearRepository(t, 5)
	agent := newGitReadAgent(t, map[string]string{"llm": repository.directory})
	for _, target := range []string{commitTarget("llm", shas[4]), commitsTarget("llm", shas[0], shas[4])} {
		state := installHookedGit(t, agent, "")
		if recorder := gitGet(agent, target); recorder.Code != 200 {
			t.Fatalf("%s: 200 이어야 한다: %d %.300s", target, recorder.Code, recorder.Body.String())
		}
		seen := map[string]bool{}
		invocations := hookedInvocations(t, state)
		if len(invocations) == 0 {
			t.Fatalf("%s: git 을 부르지 않았다", target)
		}
		for _, arguments := range invocations {
			key := strings.Join(arguments, "\x00")
			if seen[key] {
				t.Fatalf("%s: 바뀐 것이 없는데 같은 git 명령을 두 번 돌렸다(재시도가 늘 돈다): %q", target, arguments)
			}
			seen[key] = true
		}
	}
}

func TestGitReadDeadlineCoversRetriesWhenBoundaryKeepsChangingSlowly(t *testing.T) {
	repository, shas := linearRepository(t, 6)
	agent := newGitReadAgent(t, map[string]string{"llm": repository.directory})
	agent.gitTimeout = time.Second
	installHookedGit(t, agent, keepChangingHook(repository, shas[:4])+"; sleep 0.3")
	for _, target := range []string{commitTarget("llm", shas[5]), commitsTarget("llm", shas[4], shas[5])} {
		startedAt := time.Now()
		recorder := gitGet(agent, target)
		elapsed := time.Since(startedAt)
		if recorder.Code != 503 && recorder.Code != 504 {
			t.Fatalf("%s: 계속 바뀌면서 느리면 503 또는 504 로 끝나야 한다: %d %.300s", target, recorder.Code, recorder.Body.String())
		}
		if elapsed > time.Second+1500*time.Millisecond { // 기한 1초 + 자식 정리 몫
			t.Fatalf("%s: 재시도가 기한(1초)을 넘겨 %v 걸렸다 — 기한은 조회 하나 전체에 걸려야 한다", target, elapsed)
		}
	}
}
