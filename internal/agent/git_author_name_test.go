package agent

// 작성자 이름 상한 — 기준: requirement-spec.md §12.10. author_name 은 200바이트까지(글자 경계에서 자르고 끝에 …).
// '&' 는 JSON 에서 6바이트(&) — 40만 개면 이름 하나로 2.4MB 다.

import (
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// commitWithAuthorName — 이름이 커서(환경 변수·인자 한 개의 상한 128KB 를 넘는다) 저장소 설정의 user.name 으로 커밋한다.
func (repository *testRepository) commitWithAuthorName(name, message string, withFile bool) string {
	repository.t.Helper()
	configPath := repository.directory + "/.git/config"
	original := readFileString(repository.t, configPath)
	if err := os.WriteFile(configPath, []byte(original+"[user]\n\tname = \""+name+"\"\n"), 0o644); err != nil {
		repository.t.Fatal(err)
	}
	defer os.WriteFile(configPath, []byte(original), 0o644)
	if withFile {
		repository.writeFile("named.txt", message+"\n")
	}
	repository.git("add", "-A")
	repository.commits++
	environment := []string{}
	for _, entry := range hermeticGitEnvironment() {
		if !strings.HasPrefix(entry, "GIT_AUTHOR_NAME=") {
			environment = append(environment, entry)
		}
	}
	command := gitCommandWithEnvironment(repository, environment, "commit", "-q", "--allow-empty", "-m", message)
	if output, err := command.CombinedOutput(); err != nil {
		repository.t.Fatalf("긴 이름 커밋 실패: %v %.300s", err, output)
	}
	return repository.git("rev-parse", "HEAD")
}

func requireCappedName(t *testing.T, situation, name, original string) {
	t.Helper()
	if len(original) <= 200 {
		if name != original {
			t.Fatalf("%s: 200바이트 이하의 이름은 그대로여야 한다: %q", situation, name)
		}
		return
	}
	if len(name) > 200 || !strings.HasSuffix(name, "…") || !utf8.ValidString(name) || !strings.HasPrefix(original, strings.TrimSuffix(name, "…")) {
		t.Fatalf("%s: 이름은 200바이트 이하, 글자 경계에서 잘리고 …로 끝나야 한다: %d바이트 %q", situation, len(name), name[:min(len(name), 40)])
	}
}

func TestGitReadCapsAuthorName(t *testing.T) {
	huge := strings.Repeat("&", 400000)
	korean := strings.Repeat("가", 100) // 300바이트 — 200바이트 경계가 글자 가운데에 걸린다
	exact := strings.Repeat("n", 200)
	repository := newTestRepository(t)
	base := repository.commitMessage("base")
	type namedCommit struct{ sha, name string }
	commits := []namedCommit{
		{repository.commitWithAuthorName(huge, "huge empty", false), huge},
		{repository.commitWithAuthorName(huge, "huge with file", true), huge},
		{repository.commitWithAuthorName(korean, "korean", true), korean},
		{repository.commitWithAuthorName(exact, "exact", true), exact},
	}
	agent := newGitReadAgent(t, map[string]string{"llm": repository.directory})
	for _, commit := range commits {
		recorder := gitGet(agent, commitTarget("llm", commit.sha))
		if recorder.Body.Len() > 2<<20 {
			t.Fatalf("상세 응답이 2MB 를 넘는다: %d바이트", recorder.Body.Len())
		}
		detail := requireGitSuccess[GitCommitDetail](t, recorder)
		requireCappedName(t, "상세 "+detail.Subject, detail.AuthorName, commit.name)
	}
	recorder := gitGet(agent, commitsTarget("llm", base, commits[len(commits)-1].sha))
	if recorder.Body.Len() > 2<<20 {
		t.Fatalf("목록 응답이 2MB 를 넘는다: %d바이트", recorder.Body.Len())
	}
	result := requireGitSuccess[GitCommitRange](t, recorder)
	if len(result.Commits) != len(commits) {
		t.Fatalf("목록에 네 커밋이 다 있어야 한다: %d", len(result.Commits))
	}
	for index, commit := range result.Commits {
		requireCappedName(t, "목록 "+commit.Subject, commit.AuthorName, commits[len(commits)-1-index].name)
	}
}

// gitCommandWithEnvironment — 환경을 통째로 정해 저장소에서 git 을 돌릴 명령(작성자 이름을 설정 파일로 줄 때).
func gitCommandWithEnvironment(repository *testRepository, environment []string, arguments ...string) *exec.Cmd {
	authored := gitTestClock.Add(time.Duration(repository.commits) * time.Minute).Format(time.RFC3339)
	command := exec.Command("git", arguments...)
	command.Dir = repository.directory
	command.Env = append(environment, "GIT_AUTHOR_DATE="+authored, "GIT_COMMITTER_DATE="+authored)
	return command
}
