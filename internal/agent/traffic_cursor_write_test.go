package agent

// R1 — 읽은 자리의 파일을 쓰지 못할 때. 기준: requirement-spec.md(traffic-map) "T5 리뷰 루프 1 의 반영" R1.
//
// 정한 계약:
//   - 헬스 traffic 의 칸 cursor_write_failures(이어지는 쓰기 실패 수 — 쓰기에 성공하면 0)가 send_failures 바로 뒤에 있다.
//   - 적지 못한 항목은 보내지 않는다(XADD 0) · 자리를 옮기지 않는다 · pending · last_seq 는 그대로.
//   - 로그: 첫 실패에 `traffic cursor write failed: <오류>` 한 줄(둘째부터 없음) · 회복하면 `traffic cursor write recovered after <n> failures` 한 줄.
//   - 쓰기가 돌아오면 그 주기에 0 이고, 그사이의 줄은 빠짐없이 한 번씩 간다(순번마다 한 번 센 합).
//   - sent 를 적지 못한 것도 쓰기 실패다 — 이미 보낸 항목은 다음 주기에 같은 순번을 같은 바이트로 다시 보낸다(지금의 계약).
// 칸은 시험이 JSON 으로 읽는다(구조체의 이름에 기대지 않는다).

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func healthNumber(t *testing.T, harness *trafficHarness, name string) int64 {
	t.Helper()
	encoded, _ := json.Marshal(harness.collector.Health())
	var fields map[string]any
	json.Unmarshal(encoded, &fields)
	value, present := fields[name].(float64)
	if !present {
		t.Fatalf("헬스에 %s 가 없다: %s", name, encoded)
	}
	return int64(value)
}

func cursorWriteLogs(harness *trafficHarness) (failed, recovered []string) {
	for _, line := range harness.logs.snapshot() {
		if strings.HasPrefix(line.message, "traffic cursor write failed: ") {
			failed = append(failed, line.message)
		}
		if strings.HasPrefix(line.message, "traffic cursor write recovered") {
			recovered = append(recovered, line.message)
		}
	}
	return failed, recovered
}

func TestCursorWriteFailuresAreCountedAndNothingIsSent(t *testing.T) {
	defer failOnPanic(t)
	harness := startedHarness(t, nginx)
	before := harness.cursorFile()
	lastSeq := *harness.collector.Health().LastSeq
	harness.setCursorWriteFault(func([]byte) error { return errors.New("디스크 가득") })
	written := 0
	for cycle := 1; cycle <= 3; cycle++ {
		harness.docker.write(nginx, harness.clock.wallNow().Add(time.Second), accessLines(4, lineBaseMillis+int64(written), nil)...)
		written += 4
		attempts := len(harness.stream.attempts)
		harness.advance(30 * time.Second)
		harness.cycle()
		if len(harness.stream.attempts) != attempts {
			t.Fatalf("주기 %d: 적지 못한 항목을 보냈다", cycle)
		}
		if failures := healthNumber(t, harness, "cursor_write_failures"); failures != int64(cycle) {
			t.Fatalf("주기 %d: cursor_write_failures %d", cycle, failures)
		}
		health := harness.collector.Health()
		if health.Pending || health.LastSeq == nil || *health.LastSeq != lastSeq {
			t.Fatalf("주기 %d: pending · last_seq 는 그대로: %+v", cycle, health)
		}
		if after := harness.cursorFile(); after.Containers[0].Position != before.Containers[0].Position || after.Last.Seq != before.Last.Seq {
			t.Fatalf("주기 %d: 자리를 옮기지 않는다", cycle)
		}
	}
	failed, recovered := cursorWriteLogs(harness)
	if len(failed) != 1 || !strings.Contains(failed[0], "디스크 가득") || len(recovered) != 0 {
		t.Fatalf("첫 실패에 한 줄뿐: %v %v", failed, recovered)
	}
	harness.setCursorWriteFault(nil)
	harness.advance(30 * time.Second)
	harness.cycle()
	if failures := healthNumber(t, harness, "cursor_write_failures"); failures != 0 {
		t.Fatalf("쓰기가 돌아온 주기에 0: %d", failures)
	}
	if _, recovered := cursorWriteLogs(harness); len(recovered) != 1 || recovered[0] != "traffic cursor write recovered after 3 failures" {
		t.Fatalf("회복의 로그 한 줄: %v", recovered)
	}
	for range 2 {
		harness.advance(30 * time.Second)
		harness.cycle()
	}
	distinct := distinctBySequence(t, harness.stream.items())
	total := int64(0)
	for _, item := range distinct {
		requireObserveAccepts(t, item)
		total += decodeTrafficBody(t, item).source(t, nginx).Lines
	}
	if total != int64(written) {
		t.Fatalf("그사이의 줄은 빠짐없이 한 번씩: %d ≠ %d", total, written)
	}
}

func TestSentWriteFailureIsCountedAndResentSameBytes(t *testing.T) {
	defer failOnPanic(t)
	harness := startedHarness(t, nginx)
	harness.docker.write(nginx, trafficWallStart.Add(5*time.Second), accessLines(3, lineBaseMillis, nil)...)
	harness.setCursorWriteFault(func(content []byte) error {
		if strings.Contains(string(content), `"state":"sent"`) {
			return errors.New("권한 없음")
		}
		return nil
	})
	for cycle := 1; cycle <= 2; cycle++ {
		harness.advance(30 * time.Second)
		harness.cycle()
		if failures := healthNumber(t, harness, "cursor_write_failures"); failures != int64(cycle) {
			t.Fatalf("sent 를 적지 못한 주기 %d: cursor_write_failures %d", cycle, failures)
		}
	}
	harness.setCursorWriteFault(nil)
	harness.advance(30 * time.Second)
	harness.cycle()
	if failures := healthNumber(t, harness, "cursor_write_failures"); failures != 0 {
		t.Fatalf("회복하면 0: %d", failures)
	}
	failed, recovered := cursorWriteLogs(harness)
	if len(failed) != 1 || !strings.Contains(failed[0], "권한 없음") || len(recovered) != 1 || recovered[0] != "traffic cursor write recovered after 2 failures" {
		t.Fatalf("로그: %v %v", failed, recovered)
	}
	harness.advance(30 * time.Second)
	harness.cycle()
	distinct := distinctBySequence(t, harness.stream.items()) // 같은 순번은 같은 바이트
	total := int64(0)
	for _, item := range distinct {
		requireObserveAccepts(t, item)
		total += decodeTrafficBody(t, item).source(t, nginx).Lines
	}
	if total != 3 || len(harness.stream.items()) <= len(distinct) {
		t.Fatalf("이미 보낸 항목을 같은 순번으로 다시 보낸다 · 줄은 한 번씩: 합 %d · 보냄 %d · 순번 %d", total, len(harness.stream.items()), len(distinct))
	}
}
