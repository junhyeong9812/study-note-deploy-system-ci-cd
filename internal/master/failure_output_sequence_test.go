package master

// 보관의 순서는 저장 번호로 — 기준: requirement-spec.md(2026-11-25 admin-incident-assist) §18 의 2.
//
// 정한 계약:
//   - 출력 파일과 표지에 칸 `sequence`(정수)를 적는다. 남길 때마다 1씩 는다 — 빈 폴더의 첫 번호는 1, master 가 다시 뜨면
//     폴더의 가장 큰 번호(표지의 번호도 센다) + 1 부터. 조회 응답에는 싣지 않는다.
//   - "최근 50건"은 이 번호로 가른다(벽시계가 아니다).
//   - 14일 · 90일의 기한은 벽시계와 파일에 적힌 시각(recorded_at · expired_at)으로 본다 — 적힌 시각이 지금보다 미래면 기한으로 지우지 않는다.
//     미래 시각도 그 시각 + 14일(표지는 + 90일)이 지나면 규칙대로 지운다.
//   - 번호가 없는 파일(이 칸 이전의 꼴)은 지원하지 않지만 master 가 뜨고 · 남기고 · 조회하는 것을 깨지 않는다.

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/junhyeong9812/study-note-deploy-system-ci-cd/internal/shared"
)

func storedSequence(t *testing.T, directory, requestID string) int64 {
	t.Helper()
	var sequence int64
	raw := storedFile(t, directory, requestID)["sequence"]
	if raw == nil || json.Unmarshal(raw, &sequence) != nil {
		t.Fatalf("%s 의 파일에 정수 칸 sequence 가 없다: %s", requestID, raw)
	}
	return sequence
}

func TestStoredSequenceIncreasesAndIsNotServed(t *testing.T) {
	agent := newOutputAgent(t)
	master, directory, _ := newOutputMaster(t)
	for index := range 3 {
		failDeployment(t, master, agent, "gh-q1-"+itoa(index), outputScript{path: "/agent/ci", code: "ci_test", detail: "tail", output: rawOutput("x")})
	}
	for index := range 3 {
		if sequence := storedSequence(t, directory, "gh-q1-"+itoa(index)); sequence != int64(index+1) {
			t.Fatalf("저장 번호는 1 부터 하나씩: gh-q1-%d 는 %d", index, sequence)
		}
	}
	for _, key := range dataKeys(t, lookupOutput(master, "gh-q1-0")) {
		if key == "sequence" {
			t.Fatal("저장 번호를 조회 응답에 실었다")
		}
	}
}

func TestRetentionOrderFollowsSavedSequenceNotTheClock(t *testing.T) {
	agent := newOutputAgent(t)
	master, directory, clock := newOutputMaster(t)
	for index := range 50 {
		failDeployment(t, master, agent, "gh-q2-"+itoa(index), outputScript{path: "/agent/ci", code: "ci_test", detail: "tail", output: rawOutput("before " + itoa(index))})
		clock.Advance(time.Second)
	}
	clock.Advance(-time.Hour) // 시계 맞춤 — 앞의 50건이 모두 미래가 된다
	failDeployment(t, master, agent, "gh-q2-new-a", outputScript{path: "/agent/ci", code: "ci_test", detail: "tail", output: rawOutput("new a")})
	clock.Advance(time.Second)
	failDeployment(t, master, agent, "gh-q2-new-b", outputScript{path: "/agent/ci", code: "ci_test", detail: "tail", output: rawOutput("new b")})
	for _, requestID := range []string{"gh-q2-new-a", "gh-q2-new-b", "gh-q2-2", "gh-q2-49"} {
		decodeOutputRecord(t, lookupOutput(master, requestID))
	}
	for _, requestID := range []string{"gh-q2-0", "gh-q2-1"} { // 가장 먼저 남긴 둘
		requireFailure(t, lookupOutput(master, requestID), http.StatusGone, "output_expired")
	}
	if count := filesWithOutput(t, directory); count != 50 {
		t.Fatalf("50건은 그대로: %d", count)
	}
}

func TestSequenceContinuesAfterRestart(t *testing.T) {
	agent := newOutputAgent(t)
	master, directory, clock := newOutputMaster(t)
	failDeployment(t, master, agent, "gh-q3-a", outputScript{path: "/agent/ci", code: "ci_test", detail: "tail", output: rawOutput("a")})
	failDeployment(t, master, agent, "gh-q3-b", outputScript{path: "/agent/ci", code: "ci_test", detail: "tail", output: rawOutput("b")})
	// gh-q3-b 를 번호 7 의 표지로 바꿔 둔다 — 가장 큰 번호가 표지에 있다
	marker, _ := json.Marshal(map[string]any{"request_id": "gh-q3-b", "failed_stage": "ci_test", "recorded_at": clockStart.Format(time.RFC3339Nano),
		"expired_at": clockStart.Format(time.RFC3339Nano), "sequence": 7})
	if err := os.WriteFile(filepath.Join(directory, "gh-q3-b.json"), marker, 0o600); err != nil {
		t.Fatal(err)
	}
	restarted, err := New(shared.NewLogger()) // 같은 이력 · 같은 폴더로 다시 뜬다
	if err != nil {
		t.Fatal(err)
	}
	restarted.clock = clock.Now
	agent.script("llm", outputScript{path: "/agent/ci", code: "ci_test", detail: "tail", output: rawOutput("c")})
	submitService(restarted, "llm", "gh-q3-c")
	waitSettled(t, restarted, "gh-q3-c")
	if sequence := storedSequence(t, directory, "gh-q3-c"); sequence != 8 {
		t.Fatalf("다시 뜬 뒤의 번호는 폴더의 가장 큰 번호(표지의 7) + 1 이어야 한다: %d", sequence)
	}
	decodeOutputRecord(t, lookupOutput(restarted, "gh-q3-c"))
	_ = master
}

func TestFutureDatedOutputIsExpiredFromItsWrittenTime(t *testing.T) {
	agent := newOutputAgent(t)
	master, directory, clock := newOutputMaster(t)
	failDeployment(t, master, agent, "gh-q4-old", outputScript{path: "/agent/ci", code: "ci_test", detail: "tail", output: rawOutput("old-output-text")})
	clock.Advance(100 * 24 * time.Hour) // 시계가 앞으로 튄 채 남긴다
	failDeployment(t, master, agent, "gh-q4-future", outputScript{path: "/agent/ci", code: "ci_test", detail: "tail", output: rawOutput("future-output-text")})
	clock.Advance(-85 * 24 * time.Hour) // 보정 — 지금은 처음에서 15일
	failDeployment(t, master, agent, "gh-q4-new", outputScript{path: "/agent/ci", code: "ci_test", detail: "tail", output: rawOutput("new-output-text")})
	decodeOutputRecord(t, lookupOutput(master, "gh-q4-new"))
	decodeOutputRecord(t, lookupOutput(master, "gh-q4-future")) // 미래 시각은 기한으로 지우지 않는다
	requireFailure(t, lookupOutput(master, "gh-q4-old"), http.StatusGone, "output_expired")
	if fields := storedFile(t, directory, "gh-q4-future"); fields["output"] == nil {
		t.Fatal("보정 뒤 정리가 미래 시각의 출력을 지웠다")
	}
	clock.Advance(99 * 24 * time.Hour) // 처음에서 114일 = 미래 시각 + 14일
	decodeOutputRecord(t, lookupOutput(master, "gh-q4-future"))
	clock.Advance(time.Second)
	requireFailure(t, lookupOutput(master, "gh-q4-future"), http.StatusGone, "output_expired") // 적힌 시각 + 14일이 지나면 규칙대로
	failDeployment(t, master, agent, "gh-q4-later", outputScript{path: "/agent/ci", code: "ci_test", detail: "tail", output: rawOutput("later")})
	if fields := storedFile(t, directory, "gh-q4-future"); fields["output"] != nil {
		t.Fatal("적힌 시각 + 14일이 지난 출력을 정리하지 않았다")
	}
}

func TestFutureDatedMarkerIsKeptAsExpired(t *testing.T) {
	agent := newOutputAgent(t)
	master, directory, clock := newOutputMaster(t)
	failDeployment(t, master, agent, "gh-q5-1", outputScript{path: "/agent/ci", code: "ci_test", detail: "tail", output: rawOutput("x")})
	sequence := storedSequence(t, directory, "gh-q5-1")
	marker, _ := json.Marshal(map[string]any{"request_id": "gh-q5-1", "failed_stage": "ci_test", "recorded_at": clockStart.Format(time.RFC3339Nano),
		"expired_at": clockStart.Add(200 * 24 * time.Hour).Format(time.RFC3339Nano), "sequence": sequence})
	os.WriteFile(filepath.Join(directory, "gh-q5-1.json"), marker, 0o600)
	clock.Advance(time.Hour)
	failDeployment(t, master, agent, "gh-q5-2", outputScript{path: "/agent/ci", code: "ci_test", detail: "tail", output: rawOutput("y")})
	requireFailure(t, lookupOutput(master, "gh-q5-1"), http.StatusGone, "output_expired")
	if _, err := os.Stat(filepath.Join(directory, "gh-q5-1.json")); err != nil {
		t.Fatalf("미래에 지운 표지는 기한으로 지우지 않는다: %v", err)
	}
}

func TestFileWithoutSequenceDoesNotBreakTheMaster(t *testing.T) {
	agent := newOutputAgent(t)
	master, directory, _ := newOutputMaster(t)
	legacy, _ := json.Marshal(map[string]any{"request_id": "gh-q6-legacy", "service": "llm", "commit_sha": deploySha, "failed_stage": "ci_test",
		"recorded_at": clockStart.Format(time.RFC3339Nano), "output": "legacy", "truncated": false, "omitted_bytes": 0, "bytes": 6, "redactions": map[string]int{}})
	os.WriteFile(filepath.Join(directory, "gh-q6-legacy.json"), legacy, 0o600)
	for index := range 3 {
		failDeployment(t, master, agent, "gh-q6-"+itoa(index), outputScript{path: "/agent/ci", code: "ci_test", detail: "tail", output: rawOutput("x")})
		decodeOutputRecord(t, lookupOutput(master, "gh-q6-"+itoa(index)))
	}
	if recorder := lookupOutput(master, "gh-q6-legacy"); recorder.Code == 0 || !json.Valid(recorder.Body.Bytes()) {
		t.Fatalf("번호 없는 파일의 조회가 봉투가 아니다: %d %q", recorder.Code, recorder.Body.String())
	}
	if _, err := New(shared.NewLogger()); err != nil {
		t.Fatalf("번호 없는 파일이 있어도 master 는 뜬다: %v", err)
	}
}
