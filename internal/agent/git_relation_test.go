package agent

// relation 판정 네 갈래 — 기준: requirement-spec.md §12.8(앞 절보다 우선).
//  1. A 가 B 의 조상 → ancestor  2. B 의 조상 순회가 얕은 경계에 닿지 않음 → not_ancestor
//  3. B 가 A 의 조상(확실한 되돌림) → not_ancestor  4. 그 밖 → unknown(200, 빈 목록, range_complete:false)

import (
	"strconv"
	"strings"
	"testing"
)

// refetchedShallowHistory — 원본 c1~c10 → clone --depth=5(HEAD=c10) → 원본에 c11~c20 →
// clone 에서 fetch --depth=5 origin main + reset --hard c20. c10 의 객체는 남지만 c20 에서 c10 까지의 이력은 c16 경계에서 끊긴다.
func refetchedShallowHistory(t *testing.T) (shallow *testRepository, shas []string) {
	t.Helper()
	source := newTestRepository(t)
	for index := 1; index <= 10; index++ {
		source.writeFile("history.txt", "c"+strconv.Itoa(index)+"\n")
		shas = append(shas, source.commitMessage("c"+strconv.Itoa(index)))
	}
	shallow = source.shallowClone(5)
	for index := 11; index <= 20; index++ {
		source.writeFile("history.txt", "c"+strconv.Itoa(index)+"\n")
		shas = append(shas, source.commitMessage("c"+strconv.Itoa(index)))
	}
	shallow.git("fetch", "-q", "--depth=5", "origin", "main")
	shallow.git("reset", "-q", "--hard", shas[19])
	if shallow.git("cat-file", "-t", shas[9]) != "commit" {
		t.Fatal("받침 확인: clone 에 c10 객체가 남아 있어야 한다")
	}
	if !strings.Contains(readFileString(t, shallow.directory+"/.git/shallow"), shas[15]) {
		t.Fatal("받침 확인: 다시 받은 뒤의 얕은 경계에 c16 이 있어야 한다")
	}
	return shallow, shas
}

func TestGitCommitsRelationIsUnknownWhenHistoryIsCutByShallowBoundary(t *testing.T) {
	shallow, shas := refetchedShallowHistory(t)
	agent := newGitReadAgent(t, map[string]string{"llm": shallow.directory})
	recorder := gitGet(agent, commitsTarget("llm", shas[9], shas[19])) // from=c10, to=c20
	result := requireGitSuccess[GitCommitRange](t, recorder)
	commits, isList := rawData(t, recorder.Body.Bytes())["commits"].([]any)
	if result.Relation != "unknown" || !isList || len(commits) != 0 || result.RangeComplete {
		t.Fatalf("경계에서 끊긴 이력은 relation=unknown·commits=[]·range_complete=false 여야 한다: %.300s", recorder.Body.String())
	}
}

func TestGitCommitsRelationIsAncestorInsideRefetchedHistory(t *testing.T) {
	shallow, shas := refetchedShallowHistory(t)
	agent := newGitReadAgent(t, map[string]string{"llm": shallow.directory})
	result := requireGitSuccess[GitCommitRange](t, gitGet(agent, commitsTarget("llm", shas[16], shas[19]))) // c17 → c20
	if result.Relation != "ancestor" || strings.Join(commitShas(result.Commits), ",") != strings.Join([]string{shas[19], shas[18], shas[17]}, ",") || !result.RangeComplete {
		t.Fatalf("이력 안의 두 커밋은 ancestor(c20,c19,c18) 여야 한다: %+v", result)
	}
}

func TestGitCommitsRelationIsNotAncestorForCertainRollbackInShallowHistory(t *testing.T) {
	shallow, shas := refetchedShallowHistory(t)
	agent := newGitReadAgent(t, map[string]string{"llm": shallow.directory})
	recorder := gitGet(agent, commitsTarget("llm", shas[19], shas[17])) // from=c20, to=c18 — B 가 A 의 조상
	result := requireGitSuccess[GitCommitRange](t, recorder)
	commits, isList := rawData(t, recorder.Body.Bytes())["commits"].([]any)
	if result.Relation != "not_ancestor" || !isList || len(commits) != 0 {
		t.Fatalf("확실한 되돌림은 not_ancestor·빈 목록이어야 한다: %.300s", recorder.Body.String())
	}
}

func TestGitCommitsRelationIsNotAncestorWhenToHistoryNeverReachesBoundary(t *testing.T) {
	shallow, shas := refetchedShallowHistory(t)
	// 얕은 저장소 안에서 고아 가지의 뿌리 커밋 B — 그 조상(자기 자신)에는 얕은 경계가 없다
	shallow.git("checkout", "-q", "--orphan", "lonely")
	shallow.git("rm", "-q", "-rf", ".")
	shallow.writeFile("lonely.txt", "root of an orphan branch\n")
	orphan := shallow.commitMessage("orphan root")
	agent := newGitReadAgent(t, map[string]string{"llm": shallow.directory})
	recorder := gitGet(agent, commitsTarget("llm", shas[19], orphan))
	result := requireGitSuccess[GitCommitRange](t, recorder)
	commits, isList := rawData(t, recorder.Body.Bytes())["commits"].([]any)
	if result.Relation != "not_ancestor" || !isList || len(commits) != 0 {
		t.Fatalf("B 의 이력이 온전하면(경계에 닿지 않음) not_ancestor 여야 한다: %.300s", recorder.Body.String())
	}
}
