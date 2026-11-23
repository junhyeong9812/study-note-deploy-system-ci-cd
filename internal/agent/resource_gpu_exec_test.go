package agent

// GPU 실행은 한 번에 하나 · 고착 로그 · 종료 코드 — 기준: requirement-spec.md(2026-11-24 resource-metrics) §12(앞 절보다 우선).
// 정한 세부(명세가 정하지 않은 것):
//   - 통계 칸 GPUSkipped: 앞 GPU 실행이 남아 있을 수 있어(도는 중 · 상태를 알 수 없음) 그 수집의 GPU 조회를 건너뛴 횟수.
//   - "끝났다" = 상태 조회가 Running:false. "사라짐" = 상태 조회 404. 그 밖(500 · 연결 끊김 · 기한 초과)은 "알 수 없음".
//   - 고착 로그의 시간은 수집기의 단조 시계로 잰다. 건너뜀이 시작될 때 1줄, 첫 줄로부터 1시간이 지날 때마다 1줄(건너뛴 횟수), 풀리면 1줄.

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

const gpuTestDeadline = 200 * time.Millisecond

// newExecHarness — GPU 를 설정하고 컨테이너 하나를 둔 받침. GPU 기한은 짧게.
func newExecHarness(t *testing.T) *resourceHarness {
	harness := newGPUHarness(t, "0, 0, 9, 8192, 28\n", 0)
	harness.collector.gpuDeadline = gpuTestDeadline
	return harness
}

// gpuCalls — from 번째 요청부터의 GPU 실행 요청(만들기 · 시작 · 상태)을 "방법 경로" 로.
func gpuCalls(harness *resourceHarness, from int) []string {
	var calls []string
	for _, request := range harness.docker.recorded()[from:] {
		if strings.HasPrefix(request.path, "/exec/") || dockerExecCreatePath.MatchString(request.path) {
			calls = append(calls, request.method+" "+request.path)
		}
	}
	return calls
}

func countCalls(calls []string, prefix string) int {
	count := 0
	for _, call := range calls {
		if strings.HasPrefix(call, prefix) {
			count++
		}
	}
	return count
}

func requireGPUMissingButOthersIntact(t *testing.T, situation string, entry resourceEntry) {
	t.Helper()
	if gpus := listOf(t, entry, "gpus"); len(gpus) != 0 {
		t.Fatalf("%s: gpus 는 []: %v", situation, gpus)
	}
	requireMissing(t, entry, situation, missingGPUs)
	if containers := listOf(t, entry, "containers"); len(containers) != 1 {
		t.Fatalf("%s: 컨테이너 갈래는 그대로: %v", situation, containers)
	}
	requireHostLevels(t, situation, hostOf(t, entry), 0.11)
}

func requireGPUValues(t *testing.T, situation string, entry resourceEntry) {
	t.Helper()
	gpus := listOf(t, entry, "gpus")
	if len(gpus) != 1 {
		t.Fatalf("%s: GPU 값을 읽어야 한다: %s", situation, entry.field("body"))
	}
	requireNumber(t, situation+" mem_used", gpus[0]["mem_used"], 9437184, 0)
	requireMissing(t, entry, situation)
}

// leaveStuckExec — 첫 수집에서 exec-1 이 시작에서 멈추고(기한에 끊김) 그 뒤로도 계속 도는 중으로 보이게 한다.
func leaveStuckExec(t *testing.T, harness *resourceHarness) {
	t.Helper()
	harness.docker.set(func(docker *fakeDocker) {
		docker.stallExecStart = true
		docker.execInspectBehaviour["exec-1"] = "running"
	})
	requireGPUMissingButOthersIntact(t, "실행이 기한에 끊긴 수집", harness.collect())
	harness.docker.set(func(docker *fakeDocker) { docker.stallExecStart = false })
	if creations := countCalls(gpuCalls(harness, 0), "POST /containers/"); creations != 1 {
		t.Fatalf("첫 수집에서 만든 실행은 하나: %d", creations)
	}
}

func TestResourceGPUDoesNotCreateAnotherExecWhileOneIsStillRunning(t *testing.T) {
	harness := newExecHarness(t)
	leaveStuckExec(t, harness)
	before := harness.collector.Stats().GPUSkipped
	for round := 1; round <= 3; round++ {
		from := len(harness.docker.recorded())
		requireGPUMissingButOthersIntact(t, fmt.Sprintf("남은 실행이 있는 수집 %d", round), harness.collect())
		calls := gpuCalls(harness, from)
		if len(calls) == 0 || calls[0] != "GET /exec/exec-1/json" {
			t.Fatalf("수집 %d: 먼저 앞 실행의 상태를 물어야 한다: %v", round, calls)
		}
		if countCalls(calls, "POST") != 0 {
			t.Fatalf("수집 %d: 앞 실행이 도는 중인데 새 실행을 만들거나 시작했다: %v", round, calls)
		}
	}
	if creations := countCalls(gpuCalls(harness, 0), "POST /containers/"); creations != 1 {
		t.Fatalf("수집을 여러 번 돌려도 만든 실행은 1개: %d", creations)
	}
	if skipped := harness.collector.Stats().GPUSkipped - before; skipped != 3 {
		t.Fatalf("건너뛴 GPU 조회 3번을 세야 한다: %d", skipped)
	}
}

func TestResourceGPUCreatesNewExecAfterPreviousFinished(t *testing.T) {
	harness := newExecHarness(t)
	leaveStuckExec(t, harness)
	harness.docker.set(func(docker *fakeDocker) { delete(docker.execInspectBehaviour, "exec-1") }) // 이제 Running:false
	from := len(harness.docker.recorded())
	requireGPUValues(t, "앞 실행이 끝난 뒤의 수집", harness.collect())
	calls := gpuCalls(harness, from)
	if len(calls) == 0 || calls[0] != "GET /exec/exec-1/json" || countCalls(calls, "POST /containers/") != 1 {
		t.Fatalf("앞 실행의 상태를 묻고(끝남) 새 실행 하나를 만든다: %v", calls)
	}
}

func TestResourceGPUForgetsExecThatVanished(t *testing.T) {
	harness := newExecHarness(t)
	leaveStuckExec(t, harness)
	harness.docker.set(func(docker *fakeDocker) { docker.execInspectBehaviour["exec-1"] = "404" }) // 컨테이너를 다시 만들어 실행이 사라졌다
	from := len(harness.docker.recorded())
	requireGPUValues(t, "앞 실행이 사라진 뒤의 수집", harness.collect())
	calls := gpuCalls(harness, from)
	if len(calls) == 0 || calls[0] != "GET /exec/exec-1/json" || countCalls(calls, "POST /containers/") != 1 {
		t.Fatalf("사라진 실행은 잊고 새 실행을 만든다: %v", calls)
	}
	from = len(harness.docker.recorded())
	harness.collect()
	if calls := gpuCalls(harness, from); countCalls(calls, "GET /exec/exec-1/") != 0 {
		t.Fatalf("잊은 실행을 다시 묻는다: %v", calls)
	}
}

func TestResourceGPUUnknownExecStateBlocksNewExec(t *testing.T) {
	for _, behaviour := range []string{"500", "drop", "stall"} {
		t.Run(behaviour, func(t *testing.T) {
			harness := newExecHarness(t)
			// 출력은 끝났지만 끝내 도는 중으로 보이는 실행을 남긴다(시작에서 멈추지 않으므로 releases 에 잡음이 없다)
			harness.docker.set(func(docker *fakeDocker) { docker.execInspectBehaviour["exec-1"] = "running" })
			requireGPUMissingButOthersIntact(t, "끝내 도는 중인 실행", harness.collect())
			harness.docker.set(func(docker *fakeDocker) { docker.execInspectBehaviour["exec-1"] = behaviour })
			before := harness.collector.Stats().GPUSkipped
			from := len(harness.docker.recorded())
			startedAt := time.Now()
			requireGPUMissingButOthersIntact(t, "상태를 알 수 없음("+behaviour+")", harness.collect())
			if elapsed := time.Since(startedAt); elapsed > 1500*time.Millisecond {
				t.Fatalf("상태 조회에도 GPU 기한(%v)이 걸려야 한다: %v", gpuTestDeadline, elapsed)
			}
			if calls := gpuCalls(harness, from); countCalls(calls, "POST") != 0 {
				t.Fatalf("상태를 모르는데 새 실행을 만들었다: %v", calls)
			}
			if skipped := harness.collector.Stats().GPUSkipped - before; skipped != 1 {
				t.Fatalf("알 수 없음도 건너뜀으로 센다: %d", skipped)
			}
			if behaviour == "stall" {
				requireReleasedByDeadline(t, harness.docker, "멈춘 상태 조회")
			}
		})
	}
}

func TestResourceGPURemembersExecWhoseStartFailed(t *testing.T) {
	harness := newExecHarness(t)
	harness.docker.set(func(docker *fakeDocker) {
		docker.execStartStatus = 500
		docker.execInspectBehaviour["exec-1"] = "running" // 시작은 오류였지만 실행은 남아 있을 수 있다
	})
	requireGPUMissingButOthersIntact(t, "시작이 실패한 수집", harness.collect())
	harness.docker.set(func(docker *fakeDocker) { docker.execStartStatus = 0 })
	from := len(harness.docker.recorded())
	requireGPUMissingButOthersIntact(t, "시작이 실패한 실행이 아직 도는 수집", harness.collect())
	calls := gpuCalls(harness, from)
	if len(calls) == 0 || calls[0] != "GET /exec/exec-1/json" || countCalls(calls, "POST") != 0 {
		t.Fatalf("만든 즉시 기억한 ID 로 먼저 상태를 묻고, 도는 중이면 새로 만들지 않는다: %v", calls)
	}

	harness.docker.set(func(docker *fakeDocker) { delete(docker.execInspectBehaviour, "exec-1") })
	from = len(harness.docker.recorded())
	requireGPUValues(t, "남은 실행이 끝난 뒤", harness.collect())
	if calls := gpuCalls(harness, from); calls[0] != "GET /exec/exec-1/json" || countCalls(calls, "POST /containers/") != 1 {
		t.Fatalf("끝난 것을 확인한 뒤 새로 만든다: %v", calls)
	}
}

func TestResourceGPUReadsValuesWhenExitStateSettlesLate(t *testing.T) {
	harness := newGPUHarness(t, "0, 0, 9, 8192, 28\n", 0) // 기본 GPU 기한(2초) — 다시 묻는 짧은 간격이 들어갈 자리
	harness.docker.set(func(docker *fakeDocker) { docker.execRunningCount = 2 })
	requireGPUValues(t, "출력 뒤 종료 상태가 두 번 늦게 바뀜", harness.collect())
	if inspections := countCalls(gpuCalls(harness, 0), "GET /exec/exec-1/json"); inspections < 3 {
		t.Fatalf("종료 상태가 바뀔 때까지 다시 물어야 한다: %d", inspections)
	}
	from := len(harness.docker.recorded())
	requireGPUValues(t, "다음 수집", harness.collect())
	if creations := countCalls(gpuCalls(harness, from), "POST /containers/"); creations != 1 {
		t.Fatalf("끝난 실행 뒤에는 새로 만든다: %d", creations)
	}
}

func TestResourceGPUExecNeverSettlingIsRememberedAsRemaining(t *testing.T) {
	harness := newExecHarness(t)
	harness.docker.set(func(docker *fakeDocker) { docker.execInspectBehaviour["exec-1"] = "running" })
	startedAt := time.Now()
	requireGPUMissingButOthersIntact(t, "기한 안에 끝내 도는 중", harness.collect())
	if elapsed := time.Since(startedAt); elapsed > 1500*time.Millisecond {
		t.Fatalf("다시 묻는 것도 GPU 기한(%v) 안에서: %v", gpuTestDeadline, elapsed)
	}
	from := len(harness.docker.recorded())
	harness.collect()
	if calls := gpuCalls(harness, from); len(calls) == 0 || calls[0] != "GET /exec/exec-1/json" || countCalls(calls, "POST") != 0 {
		t.Fatalf("남은 실행으로 기억해 다음 수집은 상태부터 묻고 새로 만들지 않는다: %v", calls)
	}
}

func TestResourceStuckGPUDoesNotAffectDeploys(t *testing.T) {
	harness := newExecHarness(t)
	leaveStuckExec(t, harness)
	harness.agent.busy.Lock()
	requireGPUMissingButOthersIntact(t, "배포 잠금 중 · GPU 고착", harness.collect())
	harness.agent.busy.Unlock()
	for _, path := range []string{"/agent/ci", "/agent/cd"} {
		if recorder := post(harness.agent, path); recorder.Code != 200 {
			t.Fatalf("GPU 고착 중에도 %s 는 그대로: %d %s", path, recorder.Code, recorder.Body.String())
		}
	}
	if stats := harness.collector.Stats(); stats.Skipped != 0 || stats.Panics != 0 {
		t.Fatalf("GPU 의 건너뜀은 수집 전체의 건너뜀이 아니다: %+v", stats)
	}
}

func TestResourceStuckGPULogsFirstThenHourlyThenRecovery(t *testing.T) {
	harness := newExecHarness(t)
	leaveStuckExec(t, harness)
	base := len(harness.logs.snapshot())
	newLines := func() []logLine { return harness.logs.snapshot()[base:] }

	harness.clock.advance(30 * time.Second)
	harness.collect() // 건너뜀 1 — 첫 줄
	if lines := newLines(); len(lines) != 1 {
		t.Fatalf("건너뜀이 시작되면 1줄: %v", lines)
	}
	for _, step := range []time.Duration{30 * time.Second, 30 * time.Second, 30 * time.Minute} {
		harness.clock.advance(step)
		harness.collect() // 건너뜀 2 · 3 · 4 — 아직 1시간 전
	}
	if lines := newLines(); len(lines) != 1 {
		t.Fatalf("1시간이 되기 전에는 줄이 없다: %v", lines)
	}
	harness.clock.advance(30 * time.Minute)
	harness.collect() // 건너뜀 5 — 첫 줄로부터 1시간 1분
	lines := newLines()
	if len(lines) != 2 || !strings.Contains(lines[1].message, "5") {
		t.Fatalf("1시간마다 1줄(건너뛴 횟수 5): %v", lines)
	}
	harness.clock.advance(30 * time.Second)
	harness.collect() // 건너뜀 6
	if lines := newLines(); len(lines) != 2 {
		t.Fatalf("시간 줄 뒤의 수집에는 줄이 없다: %v", lines)
	}

	harness.docker.set(func(docker *fakeDocker) { delete(docker.execInspectBehaviour, "exec-1") })
	harness.clock.advance(30 * time.Second)
	requireGPUValues(t, "풀린 수집", harness.collect())
	if lines := newLines(); len(lines) != 3 {
		t.Fatalf("풀리면 1줄: %v", lines)
	}
	harness.clock.advance(30 * time.Second)
	harness.collect()
	if lines := newLines(); len(lines) != 3 {
		t.Fatalf("풀린 뒤의 수집에는 줄이 없다: %v", lines)
	}
}

func TestResourceGPUNonZeroExitWithWellFormedOutputIsMissing(t *testing.T) {
	harness := newGPUHarness(t, "0, 0, 9, 8192, 28\n", 1)
	requireGPUMissingButOthersIntact(t, "꼴은 맞지만 종료 코드 1", harness.collect())
}
