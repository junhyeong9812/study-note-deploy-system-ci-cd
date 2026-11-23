package agent

// git 조회 테스트의 받침 — 임시 디렉터리에 실제 git 저장소를 만들고, agent 를 그 저장소로 세우고, 응답을 읽는다.
// 기준: docs/cross-cutting/plans/2026-11-22/admin-skeleton/requirement-spec.md §12(12.7 우선). 구현을 보지 않고 쓴 계약 테스트다.
// 실제 git 출력의 파싱이 검증 대상이므로 가짜 명령 실행기를 쓰지 않는다. git 이 없으면 실패한다(건너뛰지 않는다).

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/junhyeong9812/study-note-deploy-system-ci-cd/internal/shared"
)

const (
	gitTestSecret         = "test-value"
	gitTestAuthorName     = "Test Author"
	gitTestAuthorEmail    = "secret-author@example.invalid"
	gitTestCommitterEmail = "secret-committer@example.invalid"
	gitTestEmailDomain    = "example.invalid"
	gitMissingSha         = "0123456789abcdef0123456789abcdef01234567" // 어느 저장소에도 없는 SHA
)

// 커밋 시각의 기준 — +09:00 으로 넣고 UTC 로 나오는지 본다.
var gitTestClock = time.Date(2026, 1, 2, 12, 0, 0, 0, time.FixedZone("KST", 9*60*60))

// hermeticGitEnvironment — 저장소를 만드는 git 이 사용자 설정·부모의 GIT_* 에 흔들리지 않게 한다.
func hermeticGitEnvironment() []string {
	environment := []string{}
	for _, entry := range os.Environ() {
		if strings.HasPrefix(entry, "GIT_") || strings.HasPrefix(entry, "LC_ALL=") {
			continue
		}
		environment = append(environment, entry)
	}
	return append(environment, "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1", "LC_ALL=C",
		"GIT_AUTHOR_NAME="+gitTestAuthorName, "GIT_AUTHOR_EMAIL="+gitTestAuthorEmail,
		"GIT_COMMITTER_NAME=Test Committer", "GIT_COMMITTER_EMAIL="+gitTestCommitterEmail)
}

func runGit(t *testing.T, directory string, extraEnvironment []string, arguments ...string) string {
	t.Helper()
	command := exec.Command("git", arguments...)
	command.Dir = directory
	command.Env = append(hermeticGitEnvironment(), extraEnvironment...)
	var standardOutput, standardError bytes.Buffer
	command.Stdout, command.Stderr = &standardOutput, &standardError
	if err := command.Run(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(arguments, " "), err, standardError.String())
	}
	return strings.TrimSpace(standardOutput.String())
}

// testRepository — 임시 디렉터리의 실제 git 저장소. 커밋마다 시각이 1분씩 늘어난다(결정적).
type testRepository struct {
	t          *testing.T
	directory  string
	commits    int
	authoredAt map[string]string // 전체 SHA → 기대하는 authored_at(UTC)
}

func newTestRepository(t *testing.T) *testRepository {
	t.Helper()
	repository := &testRepository{t: t, directory: t.TempDir(), authoredAt: map[string]string{}}
	runGit(t, repository.directory, nil, "init", "-q", "-b", "main")
	return repository
}

func (repository *testRepository) git(arguments ...string) string {
	repository.t.Helper()
	return runGit(repository.t, repository.directory, nil, arguments...)
}

func (repository *testRepository) writeFile(path, content string) {
	repository.t.Helper()
	fullPath := filepath.Join(repository.directory, path)
	if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
		repository.t.Fatal(err)
	}
	if err := os.WriteFile(fullPath, []byte(content), 0o644); err != nil {
		repository.t.Fatal(err)
	}
}

func (repository *testRepository) removeFile(path string) {
	repository.t.Helper()
	if err := os.Remove(filepath.Join(repository.directory, path)); err != nil {
		repository.t.Fatal(err)
	}
}

// commitMessage — 작업 트리 전체를 담아 커밋한다(빈 커밋 허용). 새 HEAD 의 전체 SHA 를 돌려준다.
func (repository *testRepository) commitMessage(message string) string {
	repository.t.Helper()
	repository.git("add", "-A")
	return repository.record("commit", "-q", "--allow-empty", "-m", message)
}

// record — 날짜를 고정해 커밋을 만드는 명령을 돌리고 새 HEAD 를 돌려준다.
func (repository *testRepository) record(arguments ...string) string {
	repository.t.Helper()
	authored := gitTestClock.Add(time.Duration(repository.commits) * time.Minute)
	repository.commits++
	date := authored.Format(time.RFC3339)
	runGit(repository.t, repository.directory, []string{"GIT_AUTHOR_DATE=" + date, "GIT_COMMITTER_DATE=" + date}, arguments...)
	sha := repository.git("rev-parse", "HEAD")
	repository.authoredAt[sha] = authored.UTC().Format(time.RFC3339)
	return sha
}

// shallowClone — `git clone --depth=N file://…` 로 얕은 저장소를 만든다(배포 경로의 fetch --depth=50 과 같은 꼴).
func (repository *testRepository) shallowClone(depth int) *testRepository {
	repository.t.Helper()
	parent := repository.t.TempDir()
	runGit(repository.t, parent, nil, "clone", "-q", "--depth="+strconv.Itoa(depth), "file://"+repository.directory, "clone")
	return &testRepository{t: repository.t, directory: filepath.Join(parent, "clone"), authoredAt: repository.authoredAt}
}

// shallowMergeHistory — main(m1…m20) 과 m1 에서 갈라진 side(s1…s15) 를 m21 에서 합치고 m22 를 더한 원본,
// 그리고 그 원본의 --depth=5 clone. clone 의 얕은 경계는 m18 과 s13 이다(실측 git 2.43).
func shallowMergeHistory(t *testing.T) (source *testRepository, shallow *testRepository, shas map[string]string) {
	t.Helper()
	source = newTestRepository(t)
	shas = map[string]string{}
	for index := 1; index <= 20; index++ {
		source.writeFile("main.txt", "m"+strconv.Itoa(index)+"\n")
		shas["m"+strconv.Itoa(index)] = source.commitMessage("m" + strconv.Itoa(index))
	}
	source.git("checkout", "-q", "-b", "side", shas["m1"])
	for index := 1; index <= 15; index++ {
		source.writeFile("side.txt", "s"+strconv.Itoa(index)+"\n")
		shas["s"+strconv.Itoa(index)] = source.commitMessage("s" + strconv.Itoa(index))
	}
	source.git("checkout", "-q", "main")
	shas["m21"] = source.record("merge", "-q", "--no-ff", "-m", "m21", "side")
	source.writeFile("main.txt", "m22\n")
	shas["m22"] = source.commitMessage("m22")
	shallow = source.shallowClone(5)
	boundaries := strings.Fields(readFileString(t, filepath.Join(shallow.directory, ".git", "shallow")))
	sort.Strings(boundaries)
	want := []string{shas["m18"], shas["s13"]}
	sort.Strings(want)
	if strings.Join(boundaries, ",") != strings.Join(want, ",") {
		t.Fatalf("얕은 경계가 예상(m18·s13)과 다르다 — 테스트 받침을 다시 확인: %v", boundaries)
	}
	return source, shallow, shas
}

func readFileString(t *testing.T, path string) string {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(content)
}

// --- agent 세우기와 부르기 ---

// newGitReadAgent — 서비스 → 저장소 디렉터리. 허용 목록의 나머지 서비스는 비워 둔다(사용자 환경의 값이 새지 않게).
func newGitReadAgent(t *testing.T, directories map[string]string) *Agent {
	t.Helper()
	t.Setenv("DEPLOY_SECRET", gitTestSecret)
	for _, service := range []string{"front", "backend", "llm", "alert", "observe", "admin"} {
		t.Setenv("DEPLOY_DIR_"+strings.ToUpper(service), directories[service])
	}
	return New(shared.NewLogger())
}

func gitQuery(pairs ...string) string {
	var parts []string
	for index := 0; index+1 < len(pairs); index += 2 {
		parts = append(parts, queryEscape(pairs[index])+"="+queryEscape(pairs[index+1]))
	}
	return strings.Join(parts, "&")
}

func queryEscape(value string) string {
	// url.QueryEscape 와 같지만 순서·중복을 그대로 두려고 직접 잇는다.
	var builder strings.Builder
	for _, character := range []byte(value) {
		switch {
		case character >= 'a' && character <= 'z', character >= 'A' && character <= 'Z', character >= '0' && character <= '9',
			character == '-', character == '_', character == '.', character == '~':
			builder.WriteByte(character)
		default:
			builder.WriteString("%" + strings.ToUpper(hex.EncodeToString([]byte{character})))
		}
	}
	return builder.String()
}

// gitCall — secret 이 비면 인증 헤더 없이 부른다.
func gitCall(agent *Agent, method, target, secret string) *httptest.ResponseRecorder {
	mux := http.NewServeMux()
	agent.Register(mux)
	httpRequest := httptest.NewRequest(method, target, nil)
	if secret != "" {
		httpRequest.Header.Set(shared.SecretHeader, secret)
	}
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, httpRequest)
	return recorder
}

func gitGet(agent *Agent, target string) *httptest.ResponseRecorder {
	return gitCall(agent, http.MethodGet, target, gitTestSecret)
}

func commitsTarget(service, from, to string) string {
	return "/agent/git/commits?" + gitQuery("service", service, "from", from, "to", to)
}

func commitTarget(service, sha string) string {
	return "/agent/git/commit?" + gitQuery("service", service, "sha", sha)
}

type gitEnvelope[Data any] struct {
	Success bool `json:"success"`
	Data    Data `json:"data"`
	Error   struct {
		Code   string `json:"code"`
		Detail string `json:"detail"`
	} `json:"error"`
}

func decodeGitEnvelope[Data any](t *testing.T, recorder *httptest.ResponseRecorder) gitEnvelope[Data] {
	t.Helper()
	var envelope gitEnvelope[Data]
	if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("봉투가 아니다(%d): %v\n%.500s", recorder.Code, err, recorder.Body.String())
	}
	return envelope
}

func requireGitSuccess[Data any](t *testing.T, recorder *httptest.ResponseRecorder) Data {
	t.Helper()
	if recorder.Code != http.StatusOK {
		t.Fatalf("200 이어야 한다: %d %.500s", recorder.Code, recorder.Body.String())
	}
	envelope := decodeGitEnvelope[Data](t, recorder)
	if !envelope.Success {
		t.Fatalf("success:true 여야 한다: %.500s", recorder.Body.String())
	}
	return envelope.Data
}

func requireGitFailure(t *testing.T, recorder *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	envelope := decodeGitEnvelope[json.RawMessage](t, recorder)
	if recorder.Code != status || envelope.Success || envelope.Error.Code != code {
		t.Fatalf("%d %s 이어야 한다: %d %.500s", status, code, recorder.Code, recorder.Body.String())
	}
}

func commitShas(commits []GitCommit) []string {
	shas := []string{}
	for _, commit := range commits {
		shas = append(shas, commit.Sha)
	}
	return shas
}

func findFile(t *testing.T, detail GitCommitDetail, path string) GitFileChange {
	t.Helper()
	for _, file := range detail.Files {
		if file.Path == path {
			return file
		}
	}
	var paths []string
	for _, file := range detail.Files {
		paths = append(paths, strconv.Quote(file.Path))
	}
	t.Fatalf("파일 %q 이 응답에 없다: %v", path, paths)
	return GitFileChange{}
}

// --- git 이음매: 실제 git 을 감싸 인자·환경을 남기는 것, 멈춰 있는 것 ---

type gitInvocation struct {
	arguments   []string
	environment map[string]string
}

// installRecordingGit — agent.gitProgram 을 "인자·환경을 남기고 실제 git 으로 넘기는" 셸 스크립트로 바꾼다.
// 남기는 데 셸 내장 명령만 쓴다(구현이 PATH 를 비워도 동작).
func installRecordingGit(t *testing.T, agent *Agent) func() []gitInvocation {
	t.Helper()
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatalf("git 이 없다 — 이 테스트는 실제 git 이 필요하다: %v", err)
	}
	realGit, _ = filepath.Abs(realGit)
	logDirectory := t.TempDir()
	script := "#!/bin/sh\n" +
		"printf '%s\\0' \"$@\" > '" + logDirectory + "'/$$.arguments\n" +
		"export -p > '" + logDirectory + "'/$$.environment\n" +
		"exec '" + realGit + "' \"$@\"\n"
	wrapper := filepath.Join(t.TempDir(), "git")
	if err := os.WriteFile(wrapper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	agent.gitProgram = wrapper
	return func() []gitInvocation {
		entries, err := os.ReadDir(logDirectory)
		if err != nil {
			t.Fatal(err)
		}
		var invocations []gitInvocation
		for _, entry := range entries {
			if !strings.HasSuffix(entry.Name(), ".arguments") {
				continue
			}
			process := strings.TrimSuffix(entry.Name(), ".arguments")
			arguments := strings.Split(strings.TrimSuffix(readFileString(t, filepath.Join(logDirectory, entry.Name())), "\x00"), "\x00")
			invocations = append(invocations, gitInvocation{arguments: arguments,
				environment: parseExportedEnvironment(readFileString(t, filepath.Join(logDirectory, process+".environment")))})
		}
		return invocations
	}
}

// parseExportedEnvironment — `export -p` 출력(dash·busybox: export NAME='value')을 이름 → 값으로.
func parseExportedEnvironment(listing string) map[string]string {
	environment := map[string]string{}
	for _, line := range strings.Split(listing, "\n") {
		line = strings.TrimPrefix(strings.TrimPrefix(line, "export "), "declare -x ")
		name, value, found := strings.Cut(line, "=")
		if name == "" {
			continue
		}
		if found && len(value) >= 2 && strings.HasPrefix(value, "'") && strings.HasSuffix(value, "'") {
			value = strings.ReplaceAll(value[1:len(value)-1], `'\''`, `'`)
		} else if found && len(value) >= 2 && strings.HasPrefix(value, `"`) && strings.HasSuffix(value, `"`) {
			value = value[1 : len(value)-1]
		}
		environment[name] = value
	}
	return environment
}

// gitSubcommand — 전역 옵션(-C 경로, -c 설정, --no-pager 등)을 건너뛴 첫 낱말.
func gitSubcommand(arguments []string) string {
	for index := 0; index < len(arguments); index++ {
		argument := arguments[index]
		if argument == "-C" || argument == "-c" {
			index++
			continue
		}
		if strings.HasPrefix(argument, "-") {
			continue
		}
		return argument
	}
	return ""
}

// installHangingGit — agent.gitProgram 을 "자기 PID 를 남기고 30초 잠드는" 스크립트로 바꾼다(exec 이라 PID 가 곧 잠든 프로세스).
// 테스트가 끝나면 남은 프로세스를 죽인다.
func installHangingGit(t *testing.T, agent *Agent) (pidFile string) {
	t.Helper()
	sleepProgram, err := exec.LookPath("sleep")
	if err != nil {
		t.Fatal(err)
	}
	sleepProgram, _ = filepath.Abs(sleepProgram)
	pidFile = filepath.Join(t.TempDir(), "pids")
	script := "#!/bin/sh\necho $$ >> '" + pidFile + "'\nexec '" + sleepProgram + "' 30\n"
	wrapper := filepath.Join(t.TempDir(), "git")
	if err := os.WriteFile(wrapper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	agent.gitProgram = wrapper
	t.Cleanup(func() {
		for _, pid := range readPids(pidFile) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})
	return pidFile
}

func readPids(pidFile string) []int {
	content, err := os.ReadFile(pidFile)
	if err != nil {
		return nil
	}
	var pids []int
	for _, field := range strings.Fields(string(content)) {
		if pid, err := strconv.Atoi(field); err == nil {
			pids = append(pids, pid)
		}
	}
	return pids
}

func waitForPids(t *testing.T, pidFile string, count int) []int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if pids := readPids(pidFile); len(pids) >= count {
			return pids
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("git 이 %d 번 불리지 않았다: %v", count, readPids(pidFile))
	return nil
}

func processGone(pid int) bool {
	return errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
}

// --- 읽기만 확인: 저장소 전체(작업 트리 + .git) 의 파일 목록과 내용 해시. git 을 부르지 않는다(색인 갱신 방지). ---

func snapshotTree(t *testing.T, root string) map[string]string {
	t.Helper()
	snapshot := map[string]string{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, _ := filepath.Rel(root, path)
		if entry.IsDir() {
			snapshot[relative+"/"] = "directory"
			return nil
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		digest := sha256.Sum256(content)
		snapshot[relative] = hex.EncodeToString(digest[:])
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func requireSameSnapshot(t *testing.T, before, after map[string]string) {
	t.Helper()
	var differences []string
	for path, digest := range before {
		if after[path] != digest {
			differences = append(differences, "바뀜·사라짐 "+path)
		}
	}
	for path := range after {
		if _, existed := before[path]; !existed {
			differences = append(differences, "생김 "+path)
		}
	}
	sort.Strings(differences)
	if len(differences) > 0 {
		t.Fatalf("조회가 저장소를 바꿨다:\n%s", strings.Join(differences, "\n"))
	}
}
