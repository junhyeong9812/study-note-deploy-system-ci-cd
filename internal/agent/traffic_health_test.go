package agent

// G. 헬스 · 로그 · 격리 — 기준: requirement-spec.md(traffic-map) "T5 착수 — 헬스 · 로그 · 다른 것에 미치는 영향".
//
// 정한 계약:
//   - GET /health 의 data = {"mode":"agent","traffic":{…}}(AgentHealth). traffic 의 칸(차례 그대로): enabled · disabled_reason · generation ·
//     last_seq · last_sent_at(XADD 에 성공한 때의 벽시계 epoch ms) · pending(written 이 남음) · send_failures(이어지는 보내기 실패) ·
//     skipped_cycles · requests_filtered · time_reversals · clock_behind · oversized_entries · panics ·
//     containers[{name · state(ok · replaced · rotated · unreadable · cursor_lost · clock_behind — 아직 읽지 않았으면 null) · lines · unparsed · ignored(누적)}](이름 순).
//   - 접속 기록의 내용(경로 · 요청 ID)은 헬스 · 로그 어디에도 없다.
//   - 로그는 상태가 바뀔 때만(정확한 글자): 보내기 실패의 첫 번 `traffic stream send failed (further failures are counted, not logged)`(warning) ·
//     회복 `traffic stream recovered after <n> failed sends`(info) · `traffic container <이름> replaced — reading from the start`(warning) ·
//     `traffic container <이름> rotated — lines may be lost`(warning) · `traffic cursor lost (<missing|corrupt>) — new generation <세대>`(warning) ·
//     건너뜀의 첫 번 `traffic cycle skipped: previous cycle has not finished (skipped <n>)`(warning) · 패닉 `traffic cycle panic: <형>`(error — 값의 글은 싣지 않는다).
//     보통의 주기에는 한 줄도 없다.
//   - 수집기의 패닉 · 지연은 다음 주기 · 배포 경로(busy 잠금)를 막지 않는다 — Run 은 앞 주기가 끝나지 않았으면 건너뛰고 센다.

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestHealthReportsTrafficState(t *testing.T) {
	defer failOnPanic(t)
	harness := startedHarness(t, nginx, "front-nginx")
	harness.docker.write(nginx, trafficWallStart.Add(5*time.Second), "nginx: started",
		defaultAccess().with(func(line *accessLine) { line.path = "/a?b" }).text(), `{"v":1,"broken"`)
	harness.advance(30 * time.Second)
	harness.cycle()
	health := harness.collector.Health()
	if !health.Enabled || health.DisabledReason != nil || health.Generation == nil || *health.Generation != "00000000a0000001" ||
		health.LastSeq == nil || *health.LastSeq != 2 || health.LastSentAt == nil || *health.LastSentAt != harness.clock.wallNow().UnixMilli() ||
		health.Pending || health.SendFailures != 0 || health.RequestsFiltered != 1 {
		t.Fatalf("헬스: %+v", health)
	}
	if len(health.Containers) != 2 || health.Containers[0].Name != nginx || *health.Containers[0].State != "ok" ||
		health.Containers[0].Lines != 1 || health.Containers[0].Unparsed != 1 || health.Containers[0].Ignored != 1 {
		t.Fatalf("컨테이너의 누적 수 · 상태: %+v", health.Containers)
	}
	harness.stream.set(func(stream *fakeTrafficStream) { stream.failNext = 2 })
	for range 2 {
		harness.advance(30 * time.Second)
		harness.cycle()
	}
	if health := harness.collector.Health(); !health.Pending || health.SendFailures != 2 {
		t.Fatalf("written 이 남으면 pending · 이어지는 실패 수: %+v", health)
	}
	encoded, _ := json.Marshal(AgentHealth("agent", harness.collector))
	requireExactKeyOrder(t, "health", string(encoded), []string{"mode", "traffic"})
	var data struct {
		Traffic json.RawMessage `json:"traffic"`
	}
	json.Unmarshal(encoded, &data)
	requireExactKeyOrder(t, "traffic", string(data.Traffic), []string{"enabled", "disabled_reason", "generation", "last_seq", "last_sent_at", "pending",
		"send_failures", "skipped_cycles", "requests_filtered", "time_reversals", "clock_behind", "oversized_entries", "panics", "containers"})
	if !strings.HasPrefix(string(encoded), `{"mode":"agent","traffic":{`) {
		t.Fatalf("mode 는 그대로: %s", encoded)
	}
	recorder := httptest.NewRecorder()
	AgentHealthHandler("agent", harness.collector)(recorder, httptest.NewRequest("GET", "/health", nil))
	if !strings.HasPrefix(recorder.Body.String(), `{"data":{"mode":"agent","traffic":{"enabled":true`) {
		t.Fatalf("GET /health 의 봉투: %s", recorder.Body.String())
	}
}

func TestHealthAndLogsCarryNoAccessLogContent(t *testing.T) {
	defer failOnPanic(t)
	harness := startedHarness(t, nginx)
	secretPath, secretID := "/private-path-Zq81", "Zq81-request-id"
	harness.docker.write(nginx, trafficWallStart.Add(5*time.Second),
		defaultAccess().with(func(line *accessLine) { line.path, line.requestID = secretPath, secretID }).text(),
		defaultAccess().with(func(line *accessLine) { line.path, line.requestID = secretPath+"?q=1", secretID+"x" }).text())
	harness.stream.set(func(stream *fakeTrafficStream) { stream.failNext = 1 })
	harness.advance(30 * time.Second)
	harness.cycle()
	harness.docker.recreate(nginx, 1)
	for range 2 {
		harness.advance(30 * time.Second)
		harness.cycle()
	}
	encoded, _ := json.Marshal(AgentHealth("agent", harness.collector))
	var logged strings.Builder
	for _, line := range harness.logs.snapshot() {
		logged.WriteString(line.message + "\n")
	}
	if health := harness.collector.Health(); len(health.Containers) != 1 || health.Containers[0].Lines != 2 || !strings.Contains(logged.String(), "replaced") ||
		!strings.Contains(logged.String(), "send failed") {
		t.Fatalf("시험의 전제 — 두 줄을 읽어 보냈고 · 보내기 실패 · replaced 의 로그가 있다: %+v\n%s", health, logged.String())
	}
	for where, text := range map[string]string{"헬스": string(encoded), "로그": logged.String()} {
		if strings.Contains(text, "Zq81") {
			t.Fatalf("%s 에 접속 기록의 내용이 있다:\n%s", where, text)
		}
	}
}

func TestLogsOnlyOnStateChanges(t *testing.T) {
	defer failOnPanic(t)
	harness := startedHarness(t, nginx)
	initial := harness.logs.snapshot()
	if len(initial) != 1 || initial[0].message != "traffic cursor lost (missing) — new generation 00000000a0000001" || initial[0].level != "warning" {
		t.Fatalf("파일이 없던 첫 주기의 로그 한 줄: %+v", initial)
	}
	for range 5 {
		harness.docker.write(nginx, harness.clock.wallNow().Add(time.Second), accessLines(1, lineBaseMillis, nil)...)
		harness.advance(30 * time.Second)
		harness.cycle()
	}
	if lines := harness.logs.snapshot(); len(lines) != 1 {
		t.Fatalf("보통의 주기에는 로그가 없다: %+v", lines)
	}
	harness.stream.set(func(stream *fakeTrafficStream) { stream.failNext = 3 })
	for range 4 {
		harness.advance(30 * time.Second)
		harness.cycle()
	}
	harness.docker.recreate(nginx, 1)
	harness.advance(30 * time.Second)
	harness.cycle()
	want := []logLine{
		{"traffic stream send failed (further failures are counted, not logged)", "warning"},
		{"traffic stream recovered after 3 failed sends", "info"},
		{"traffic container " + nginx + " replaced — reading from the start", "warning"},
	}
	lines := harness.logs.snapshot()[1:]
	if len(lines) != len(want) {
		t.Fatalf("로그: %+v", lines)
	}
	for index := range want {
		if lines[index] != want[index] {
			t.Fatalf("%d번째 로그: %+v, 기대 %+v", index, lines[index], want[index])
		}
	}
}

func TestRotationAndCorruptCursorAreLogged(t *testing.T) {
	defer failOnPanic(t)
	harness := newTrafficHarness(t, nginx)
	writeFileForTest(t, harness.settings.CursorPath, "garbage")
	harness.cycle()
	harness.docker.write(nginx, trafficWallStart.Add(5*time.Second), accessLines(1, lineBaseMillis, nil)...)
	harness.advance(30 * time.Second)
	harness.cycle()
	harness.docker.write(nginx, trafficWallStart.Add(40*time.Second), accessLines(1, lineBaseMillis+1, nil)...)
	harness.docker.rotate(nginx, trafficWallStart.Add(39*time.Second))
	harness.advance(30 * time.Second)
	harness.cycle()
	messages := []string{}
	for _, line := range harness.logs.snapshot() {
		messages = append(messages, line.message)
	}
	want := "traffic cursor lost (corrupt) — new generation 00000000a0000001,traffic container " + nginx + " rotated — lines may be lost"
	if strings.Join(messages, ",") != want {
		t.Fatalf("로그: %v", messages)
	}
}

func TestPanicAndStallDoNotStopCyclesOrDeployments(t *testing.T) {
	defer failOnPanic(t)
	harness := startedHarness(t, nginx)
	harness.collector.readDeadline = 2 * time.Second
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ticks := make(chan time.Time)
	go harness.collector.Run(ctx, ticks)

	harness.stream.set(func(stream *fakeTrafficStream) { stream.panicNow = true })
	harness.advance(30 * time.Second)
	tick(t, ticks)
	waitFor(t, func() bool { return harness.collector.Health().Panics == 1 })
	harness.advance(30 * time.Second)
	tick(t, ticks)
	waitFor(t, func() bool { return len(harness.stream.items()) >= 2 })

	harness.docker.set(nginx, func(container *fakeLogContainer) { container.stallLogs = true })
	harness.advance(30 * time.Second)
	tick(t, ticks) // 이 주기는 2초 붙잡힌다
	time.Sleep(100 * time.Millisecond)
	tick(t, ticks) // 겹친다 — 건너뛴다
	if recorder := post(harness.agent, "/agent/ci"); recorder.Code != 200 {
		t.Fatalf("통신 수집이 붙잡혀도 배포 경로는 그대로: %d", recorder.Code)
	}
	waitFor(t, func() bool { return harness.collector.Health().SkippedCycles == 1 })
	messages := []string{}
	for _, line := range harness.logs.snapshot() {
		messages = append(messages, line.message)
	}
	joined := strings.Join(messages, "\n")
	if !strings.Contains(joined, "traffic cycle panic: string") || !strings.Contains(joined, "traffic cycle skipped: previous cycle has not finished (skipped 1)") ||
		strings.Contains(joined, "가짜 통신 스트림 패닉") {
		t.Fatalf("패닉(형만) · 건너뜀의 로그:\n%s", joined)
	}
}

// tick — Run 이 주기를 받을 때까지(받지 않으면 실패 — 시험이 멈추지 않게).
func tick(t *testing.T, ticks chan time.Time) {
	t.Helper()
	select {
	case ticks <- time.Now():
	case <-time.After(5 * time.Second):
		t.Fatal("Run 이 주기를 받지 않는다")
	}
}

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("기다린 상태가 오지 않았다")
}

func writeFileForTest(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
