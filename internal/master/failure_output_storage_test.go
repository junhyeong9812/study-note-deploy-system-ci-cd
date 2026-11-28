package master

// 실패 출력의 저장 보강 — 기준: requirement-spec.md(2026-11-25 admin-incident-assist) §17 의 1 · 2 · 3 · 4 · 9.
//
// 정한 계약:
//   - 가림이 오래 걸리지 않는다: 최악 입력 1MB 의 실패도 10초(-race 에서는 4배) 안에 기록되고 조회된다(agent 를 믿지 않고 master 가 다시 가리는 길).
//   - 시계: failure_output_sequence_test.go(§18 — 순서는 저장 번호, 기한은 적힌 시각).
//   - 폴더: 이미 있는 저장 폴더도 master 가 뜰 때 0700 으로 맞춘다.
//   - 임시 파일: 저장 폴더의 `.<요청 ID>.json.<무엇이든>.tmp` 가운데 수정 시각이 1시간 넘은 것은 정리 때 지운다(방금 것 · 그 꼴이 아닌 숨김 파일은 그대로).

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWorstCaseOutputIsRecordedQuickly(t *testing.T) {
	agent := newOutputAgent(t)
	master, _, _ := newOutputMaster(t)
	worst := strings.Repeat("token-", (1<<20)/6)
	agent.script("llm", outputScript{path: "/agent/ci", code: "ci_test", detail: "exit status 1", output: rawOutput(worst)})
	started := time.Now()
	submitService(master, "llm", "gh-s1-1")
	deadline := started.Add(10 * time.Second * timeBudgetFactor)
	for time.Now().Before(deadline) {
		master.mutex.Lock()
		running := master.inFlight["llm"]
		master.mutex.Unlock()
		if !running {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if elapsed := time.Since(started); elapsed >= 10*time.Second*timeBudgetFactor {
		t.Fatalf("최악 입력 1MB 의 실패를 %v 안에 기록하지 못했다 — 가림이 입력의 제곱으로 느리다", elapsed)
	}
	if record := decodeOutputRecord(t, lookupOutput(master, "gh-s1-1")); !record.Truncated || record.FailedStage != "ci_test" {
		t.Fatalf("최악 입력의 출력이 남지 않았다: %+v", record.FailedStage)
	}
}

func TestStoredFileHasNoLeftoverAfterMask(t *testing.T) {
	agent := newOutputAgent(t)
	master, directory, _ := newOutputMaster(t)
	raw := strings.Join([]string{
		"DB_PASSWORD=" + masterFakeToken(30) + "!Zq81LeakC",
		"password=[가림:x]Zq81LeakA",
		"Authorization: Bearer [가림:x]Zq81LeakB",
		"password=[가림:secret_field]Zq81LeakI",
		strings.Repeat(".", 65533) + "s" + "k-" + strings.Repeat("A", 40) + ",end", // 조각 경계에 걸친 키 — master 의 두 번째 가림
	}, "\n")
	failDeployment(t, master, agent, "gh-s2-1", outputScript{path: "/agent/ci", code: "ci_test", detail: "exit status 1", output: rawOutput(raw)})
	onDisk := directoryBytes(t, directory)
	if strings.Contains(onDisk, "Zq81") || strings.Contains(onDisk, strings.Repeat("A", 12)) {
		t.Fatal("저장 파일에 가림 표시 뒤의 나머지 · 경계에 걸친 키의 조각이 남았다")
	}
	record := decodeOutputRecord(t, lookupOutput(master, "gh-s2-1"))
	if !strings.HasPrefix(record.Output, "DB_PASSWORD=[가림:secret_field]\npassword=[가림:secret_field]\nAuthorization: Bearer [가림:authorization]\n") ||
		!strings.HasSuffix(record.Output, strings.Repeat(".", 100)+"[가림:token],end") {
		t.Fatalf("남긴 출력이 다르다: 처음 %q", record.Output[:min(200, len(record.Output))])
	}
}

// ---- 4. 시계 — failure_output_sequence_test.go(§18 의 2: 보관의 순서는 저장 번호로) ----

// ---- 9. 폴더와 임시 파일 ----

func TestExistingOutputDirectoryIsTightenedOnStart(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "failure-outputs")
	if err := os.Mkdir(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	os.Chmod(directory, 0o755) // umask 와 상관없이
	t.Setenv("DEPLOY_OUTPUT_DIR", directory)
	newTestMaster(t)
	if info, err := os.Stat(directory); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("이미 있던 저장 폴더를 0700 으로 맞추지 않았다: %v %v", info.Mode(), err)
	}
}

func TestStaleTemporaryFilesAreRemovedOnRetention(t *testing.T) {
	agent := newOutputAgent(t)
	master, directory, clock := newOutputMaster(t)
	now := time.Now().UTC().Truncate(time.Second) // 파일의 수정 시각과 같은 시계로 — 어느 시계로 재든 같은 답
	clock.mutex.Lock()
	clock.current = now
	clock.mutex.Unlock()
	failDeployment(t, master, agent, "gh-s6-0", outputScript{path: "/agent/ci", code: "ci_test", detail: "tail", output: rawOutput("first")})
	files := map[string]time.Time{
		".gh-s6-dead.json.123456.tmp":  now.Add(-2 * time.Hour),    // 쓰다 죽어 남은 것 — 지운다
		".gh-s6-other.json.987.tmp":    now.Add(-61 * time.Minute), // 1시간 넘음 — 지운다
		".gh-s6-fresh.json.55555.tmp":  now.Add(-10 * time.Minute), // 방금 것 — 남긴다
		".hidden.json":                 now.Add(-48 * time.Hour),   // 꼴이 아닌 숨김 파일 — 남긴다
		".notes.tmp":                   now.Add(-48 * time.Hour),
		"gh-s6-dead.json.123456.tmp":   now.Add(-48 * time.Hour), // 점으로 시작하지 않는다 — 남긴다
		".bad name.json.1.tmp":         now.Add(-48 * time.Hour), // 요청 ID 꼴이 아니다 — 남긴다
		".gh-s6-dir.json.1.tmp.backup": now.Add(-48 * time.Hour),
	}
	for name, modified := range files {
		path := filepath.Join(directory, name)
		if err := os.WriteFile(path, []byte("partial"), 0o600); err != nil {
			t.Fatal(err)
		}
		os.Chtimes(path, modified, modified)
	}
	failDeployment(t, master, agent, "gh-s6-1", outputScript{path: "/agent/ci", code: "ci_test", detail: "tail", output: rawOutput("second")})
	for name := range files {
		_, err := os.Stat(filepath.Join(directory, name))
		removed := os.IsNotExist(err)
		if wantRemoved := name == ".gh-s6-dead.json.123456.tmp" || name == ".gh-s6-other.json.987.tmp"; removed != wantRemoved {
			t.Fatalf("%q: 지웠나 %v, 기대 %v", name, removed, wantRemoved)
		}
	}
	decodeOutputRecord(t, lookupOutput(master, "gh-s6-1"))
}
