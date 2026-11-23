package agent

// GPU 조회용 실행은 한 번에 하나 — 도커에게 묻는다(기억하지 않는다) · GPU 결측 로그 · 종료 코드.
// 기준: requirement-spec.md(2026-11-24 resource-metrics) §13 · §13.1(12절의 "ID 를 기억한다" · 고착 로그를 대신한다)
// · §12 의 나머지(종료 상태 재조회 · 종료 코드). 가짜 도커는 도커 29.1 실측 꼴을 낸다(resource_fixture_test.go 의 fakeExec).
// 정한 세부(명세가 정하지 않은 것):
//   - 우리 실행 = ProcessConfig.entrypoint 가 "nvidia-smi" 이고 arguments 가 고정 조회의 인자와 정확히 같은 것.
//   - 판단할 수 없음(컨테이너 조회 실패 · 실행 조회 실패 · 명령 칸 없음/꼴 다름 · 목록 20개 초과)도 결측이다. 그때 GPUSkipped 는 단언하지 않는다.
//   - 결측 로그의 시간은 수집기의 단조 시계로 잰다. 줄 글자는 단언하지 않고 줄 수와 "이어진 횟수 숫자"만 본다.

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

const gpuTestDeadline = 200 * time.Millisecond

const otherExecMarker = "OTHER_EXEC_MARKER_TEXT"

// newExecHarness — GPU 를 설정하고 컨테이너 하나를 둔 받침. GPU 기한은 짧게.
func newExecHarness(t *testing.T) *resourceHarness {
	harness := newGPUHarness(t, "0, 0, 9, 8192, 28\n", 0)
	harness.collector.gpuDeadline = gpuTestDeadline
	return harness
}

// gpuCalls — from 번째 요청부터의 GPU 관련 요청(GPU 컨테이너 조회 · 실행 만들기 · 시작 · 상태)을 "방법 경로" 로.
func gpuCalls(harness *resourceHarness, from int) []string {
	var calls []string
	for _, request := range harness.docker.recorded()[from:] {
		if strings.HasPrefix(request.path, "/exec/") || strings.HasPrefix(request.path, "/containers/llm-ollama/") {
			calls = append(calls, request.method+" "+request.path)
		}
	}
	return calls
}

func countCalls(calls []string, predicate func(call string) bool) int {
	count := 0
	for _, call := range calls {
		if predicate(call) {
			count++
		}
	}
	return count
}

func isCreate(call string) bool { return call == "POST /containers/llm-ollama/exec" }
func isStart(call string) bool {
	return strings.HasPrefix(call, "POST /exec/") && strings.HasSuffix(call, "/start")
}
func isPost(call string) bool { return strings.HasPrefix(call, "POST ") }

func execTotal(harness *resourceHarness) int {
	harness.docker.mutex.Lock()
	defer harness.docker.mutex.Unlock()
	return len(harness.docker.execs)
}

func addOurExec(docker *fakeDocker, id, state string) *fakeExec {
	return docker.addExec(id, state, "nvidia-smi", gpuQueryArguments...)
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

// ===== 1. 도는 우리 실행이 있으면 새로 만들지도 시작하지도 않는다 =====

func TestResourceGPUSkipsWhileOurQueryIsStillRunning(t *testing.T) {
	harness := newExecHarness(t)
	harness.docker.set(func(docker *fakeDocker) { docker.stallExecStart = true }) // 첫 실행은 시작에서 멈추고 끊긴 뒤에도 돈다
	requireGPUMissingButOthersIntact(t, "실행이 기한에 끊긴 수집", harness.collect())
	harness.docker.set(func(docker *fakeDocker) { docker.stallExecStart = false })
	before := harness.collector.Stats().GPUSkipped
	for round := 1; round <= 3; round++ {
		from := len(harness.docker.recorded())
		requireGPUMissingButOthersIntact(t, fmt.Sprintf("도는 실행이 있는 수집 %d", round), harness.collect())
		calls := gpuCalls(harness, from)
		if len(calls) < 2 || calls[0] != "GET /containers/llm-ollama/json" || calls[1] != "GET /exec/exec-1/json" {
			t.Fatalf("수집 %d: 컨테이너의 실행 목록을 받고 각 실행을 묻는다: %v", round, calls)
		}
		if countCalls(calls, isPost) != 0 {
			t.Fatalf("수집 %d: 우리 실행이 도는 중인데 만들거나 시작했다: %v", round, calls)
		}
	}
	if total := execTotal(harness); total != 1 {
		t.Fatalf("여러 수집에 걸쳐 실행은 처음 하나뿐: %d", total)
	}
	if skipped := harness.collector.Stats().GPUSkipped - before; skipped != 3 {
		t.Fatalf("건너뛴 GPU 조회 3번: %d", skipped)
	}
}

// ===== 2. agent 재시작 · 만들기 응답 유실 =====

func TestResourceGPURestartedAgentSeesRunningQuery(t *testing.T) {
	harness := newExecHarness(t) // 새 수집기 — 기억이 없다
	harness.docker.set(func(docker *fakeDocker) { addOurExec(docker, "exec-old", "running") })
	requireGPUMissingButOthersIntact(t, "재시작 뒤 첫 수집 · 앞 agent 의 실행이 도는 중", harness.collect())
	if calls := gpuCalls(harness, 0); countCalls(calls, isPost) != 0 {
		t.Fatalf("도는 우리 실행이 있으면 재시작 뒤에도 만들지 않는다: %v", calls)
	}
	if stats := harness.collector.Stats(); stats.GPUSkipped != 1 {
		t.Fatalf("건너뜀 1: %+v", stats)
	}
}

func TestResourceGPULostCreateResponseDoesNotPileUp(t *testing.T) {
	harness := newExecHarness(t)
	harness.docker.set(func(docker *fakeDocker) { docker.holdCreateResponse = true })
	requireGPUMissingButOthersIntact(t, "만들기 응답이 오지 않은 수집", harness.collect())
	harness.docker.set(func(docker *fakeDocker) { docker.holdCreateResponse = false })
	from := len(harness.docker.recorded())
	requireGPUValues(t, "다음 수집 — 시작 안 된 그 실행을 다시 쓴다", harness.collect())
	calls := gpuCalls(harness, from)
	if countCalls(calls, isCreate) != 0 || countCalls(calls, func(call string) bool { return call == "POST /exec/exec-1/start" }) != 1 {
		t.Fatalf("만들지 않고 exec-1 을 시작한다: %v", calls)
	}
	if total := execTotal(harness); total != 1 {
		t.Fatalf("실행 기록이 늘지 않는다: %d", total)
	}
}

// ===== 3. 막지 않는 것 =====

func TestResourceGPUIgnoresOtherCommandsAndNeverShipsThem(t *testing.T) {
	harness := newExecHarness(t)
	harness.docker.set(func(docker *fakeDocker) {
		docker.addExec("exec-shell", "running", "sh", "-c", "echo "+otherExecMarker)                           // 사람이 연 셸
		docker.addExec("exec-other-query", "running", "nvidia-smi", "--query-gpu=index", "--"+otherExecMarker) // 인자가 다른 nvidia-smi
		docker.addExec("exec-idle-shell", "created", "bash", otherExecMarker)
	})
	entry := harness.collect()
	requireGPUValues(t, "다른 명령의 실행만 있음", entry)
	if creates := countCalls(gpuCalls(harness, 0), isCreate); creates != 1 {
		t.Fatalf("다른 명령은 막지 않는다 — 새로 하나 만든다: %d", creates)
	}
	if strings.Contains(entry.text(), otherExecMarker) || strings.Contains(entry.text(), "exec-shell") {
		t.Fatal("다른 실행의 명령 · ID 를 실었다")
	}
	for _, line := range harness.logs.snapshot() {
		if strings.Contains(line.message, otherExecMarker) {
			t.Fatalf("다른 실행의 명령을 로그에 남겼다: %v", line)
		}
	}
	harness.clock.advance(30 * time.Second)
	requireGPUValues(t, "앞 실행이 끝난 다음 수집", harness.collect()) // 끝난 실행은 목록에서 빠진다
	if creates := countCalls(gpuCalls(harness, 0), isCreate); creates != 2 {
		t.Fatalf("끝난 실행은 막지 않는다: %d", creates)
	}
}

// ===== 4. 시작 안 된 우리 실행은 다시 쓴다 =====

func TestResourceGPUStartsUnstartedQueryInsteadOfCreating(t *testing.T) {
	harness := newExecHarness(t)
	harness.docker.set(func(docker *fakeDocker) {
		addOurExec(docker, "exec-a", "created")
		addOurExec(docker, "exec-b", "created")
	})
	requireGPUValues(t, "시작 안 된 우리 실행 둘", harness.collect())
	calls := gpuCalls(harness, 0)
	if countCalls(calls, isCreate) != 0 || countCalls(calls, isStart) != 1 {
		t.Fatalf("만들지 않고 하나만 시작한다: %v", calls)
	}
}

func TestResourceGPUFailingStartDoesNotPileUpExecs(t *testing.T) {
	for _, status := range []int{500, 409} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			harness := newExecHarness(t)
			harness.docker.set(func(docker *fakeDocker) { docker.execStartStatus = status })
			for round := 1; round <= 5; round++ {
				harness.clock.advance(30 * time.Second)
				requireGPUMissingButOthersIntact(t, fmt.Sprintf("시작 실패 %d", round), harness.collect())
			}
			if total := execTotal(harness); total != 1 {
				t.Fatalf("시작이 계속 실패해도 실행 기록은 1개: %d (%v)", total, gpuCalls(harness, 0))
			}
			if starts := countCalls(gpuCalls(harness, 0), isStart); starts != 5 {
				t.Fatalf("수집마다 그 실행을 다시 시작해 본다: %d", starts)
			}
		})
	}
}

// ===== 5. 판단할 수 없으면 만들지 않는다 =====

func TestResourceGPUCannotJudgeDoesNotCreate(t *testing.T) {
	cases := []struct {
		situation string
		setup     func(docker *fakeDocker)
		stalled   bool
	}{
		{"컨테이너 조회 500", func(docker *fakeDocker) { docker.gpuContainerInspect = "500" }, false},
		{"컨테이너 없음 404", func(docker *fakeDocker) { docker.gpuContainerInspect = "404" }, false},
		{"컨테이너 조회 연결 끊김", func(docker *fakeDocker) { docker.gpuContainerInspect = "drop" }, false},
		{"컨테이너 조회 멈춤", func(docker *fakeDocker) { docker.gpuContainerInspect = "stall" }, true},
		{"실행 조회 500", func(docker *fakeDocker) { docker.addExec("exec-x", "running", "sh").inspect = "500" }, false},
		{"실행 조회 멈춤", func(docker *fakeDocker) { docker.addExec("exec-x", "running", "sh").inspect = "stall" }, true},
		{"명령 칸 없음", func(docker *fakeDocker) { docker.addExec("exec-x", "running", "sh").processConfig = "missing" }, false},
		{"명령 칸 꼴이 다름", func(docker *fakeDocker) { docker.addExec("exec-x", "running", "sh").processConfig = "malformed" }, false},
		{"목록 21개", func(docker *fakeDocker) {
			for index := 0; index < 21; index++ {
				docker.addExec(fmt.Sprintf("exec-shell-%02d", index), "created", "sh")
			}
		}, false},
	}
	for _, testCase := range cases {
		t.Run(testCase.situation, func(t *testing.T) {
			harness := newExecHarness(t)
			harness.docker.set(testCase.setup)
			startedAt := time.Now()
			requireGPUMissingButOthersIntact(t, testCase.situation, harness.collect())
			if elapsed := time.Since(startedAt); elapsed > 1500*time.Millisecond {
				t.Fatalf("이 조회들도 GPU 기한(%v) 안: %v", gpuTestDeadline, elapsed)
			}
			if calls := gpuCalls(harness, 0); countCalls(calls, isPost) != 0 {
				t.Fatalf("판단할 수 없는데 만들거나 시작했다: %v", calls)
			}
			if testCase.stalled {
				requireReleasedByDeadline(t, harness.docker, testCase.situation)
			}
		})
	}
}

func TestResourceGPUTreatsVanishedExecAsAbsent(t *testing.T) {
	harness := newExecHarness(t)
	harness.docker.set(func(docker *fakeDocker) { addOurExec(docker, "exec-gone", "running").inspect = "404" }) // 목록과 조회 사이에 사라졌다
	requireGPUValues(t, "그 사이 사라진 실행", harness.collect())
	if creates := countCalls(gpuCalls(harness, 0), isCreate); creates != 1 {
		t.Fatalf("사라진 실행은 없음으로 보고 진행한다: %d", creates)
	}
}

func TestResourceGPUTwentyExecsAreStillJudged(t *testing.T) {
	harness := newExecHarness(t)
	harness.docker.set(func(docker *fakeDocker) {
		for index := 0; index < 20; index++ {
			docker.addExec(fmt.Sprintf("exec-shell-%02d", index), "created", "sh")
		}
	})
	requireGPUValues(t, "다른 명령의 실행 20개(상한 그대로)", harness.collect())
}

// ===== 6. §12 에서 그대로인 것 =====

func TestResourceGPUReadsValuesWhenExitStateSettlesLate(t *testing.T) {
	harness := newGPUHarness(t, "0, 0, 9, 8192, 28\n", 0) // 기본 GPU 기한(2초) — 다시 묻는 짧은 간격이 들어갈 자리
	harness.docker.set(func(docker *fakeDocker) { docker.execRunningCount = 2 })
	requireGPUValues(t, "출력 뒤 종료 상태가 두 번 늦게 바뀜", harness.collect())
	harness.clock.advance(30 * time.Second)
	requireGPUValues(t, "다음 수집", harness.collect())
}

func TestResourceGPUExecNeverSettlingIsMissingAndBlocksNext(t *testing.T) {
	harness := newExecHarness(t)
	harness.docker.set(func(docker *fakeDocker) { docker.execStayRunning = true })
	startedAt := time.Now()
	requireGPUMissingButOthersIntact(t, "기한 안에 끝내 도는 중", harness.collect())
	if elapsed := time.Since(startedAt); elapsed > 1500*time.Millisecond {
		t.Fatalf("다시 묻는 것도 GPU 기한(%v) 안: %v", gpuTestDeadline, elapsed)
	}
	from := len(harness.docker.recorded())
	before := harness.collector.Stats().GPUSkipped
	requireGPUMissingButOthersIntact(t, "다음 수집", harness.collect())
	if calls := gpuCalls(harness, from); countCalls(calls, isPost) != 0 {
		t.Fatalf("도는 실행이 남아 있으면 만들지 않는다: %v", calls)
	}
	if harness.collector.Stats().GPUSkipped != before+1 {
		t.Fatalf("건너뜀 +1: %+v", harness.collector.Stats())
	}
}

func TestResourceGPUNonZeroExitWithWellFormedOutputIsMissing(t *testing.T) {
	harness := newGPUHarness(t, "0, 0, 9, 8192, 28\n", 1)
	requireGPUMissingButOthersIntact(t, "꼴은 맞지만 종료 코드 1", harness.collect())
}

func TestResourceStuckGPUDoesNotAffectDeploys(t *testing.T) {
	harness := newExecHarness(t)
	harness.docker.set(func(docker *fakeDocker) { addOurExec(docker, "exec-old", "running") })
	harness.agent.busy.Lock()
	requireGPUMissingButOthersIntact(t, "배포 잠금 중 · GPU 고착", harness.collect())
	harness.agent.busy.Unlock()
	for _, path := range []string{"/agent/ci", "/agent/cd"} {
		if recorder := post(harness.agent, path); recorder.Code != 200 {
			t.Fatalf("GPU 고착 중에도 %s 는 그대로: %d %s", path, recorder.Code, recorder.Body.String())
		}
	}
	if stats := harness.collector.Stats(); stats.Skipped != 0 || stats.Panics != 0 || stats.GPUSkipped != 1 {
		t.Fatalf("GPU 의 건너뜀은 수집 전체의 건너뜀과 따로: %+v", stats)
	}
}

// ===== 7. 로그 = "GPU 결측이 이어짐" =====

func TestResourceGPUMissingStreakLogsFirstHourlyAndRecovery(t *testing.T) {
	harness := newExecHarness(t)
	harness.docker.set(func(docker *fakeDocker) { docker.stallExecStart = true })
	harness.collect() // 결측 1(기한 초과) — 첫 줄
	if lines := harness.logs.snapshot(); len(lines) != 1 {
		t.Fatalf("GPU 결측이 시작되면 1줄: %v", lines)
	}
	harness.docker.set(func(docker *fakeDocker) { docker.stallExecStart = false })
	for _, step := range []time.Duration{30 * time.Second, 30 * time.Second, 30 * time.Minute} {
		harness.clock.advance(step)
		harness.collect() // 결측 2 · 3 · 4 — 이유가 건너뜀으로 바뀌었다
	}
	harness.docker.set(func(docker *fakeDocker) {
		docker.execs["exec-1"].state = "exited" // 그 실행은 끝났고, 이번엔 명령이 실패한다
		docker.gpuExitCode = 255
	})
	if lines := harness.logs.snapshot(); len(lines) != 1 {
		t.Fatalf("이유가 바뀌어도 1시간 전에는 줄이 없다: %v", lines)
	}
	harness.clock.advance(30 * time.Minute)
	harness.collect() // 결측 5 — 첫 줄로부터 1시간 1분
	lines := harness.logs.snapshot()
	if len(lines) != 2 || !strings.Contains(lines[1].message, "5") {
		t.Fatalf("1시간마다 1줄(이어진 횟수 5): %v", lines)
	}
	harness.clock.advance(30 * time.Second)
	harness.collect() // 결측 6
	if lines := harness.logs.snapshot(); len(lines) != 2 {
		t.Fatalf("시간 줄 뒤의 수집에는 줄이 없다: %v", lines)
	}

	harness.docker.set(func(docker *fakeDocker) { docker.gpuExitCode = 0 })
	harness.clock.advance(30 * time.Second)
	requireGPUValues(t, "값이 다시 읽힘", harness.collect())
	if lines := harness.logs.snapshot(); len(lines) != 3 {
		t.Fatalf("값이 다시 읽히면 1줄: %v", lines)
	}
	harness.clock.advance(30 * time.Second)
	harness.collect()
	if lines := harness.logs.snapshot(); len(lines) != 3 {
		t.Fatalf("읽히는 동안은 줄이 없다: %v", lines)
	}
}

func TestResourceGPUSlowCommandLogsOnlyOnce(t *testing.T) {
	harness := newExecHarness(t)
	harness.collector.gpuDeadline = 100 * time.Millisecond
	harness.docker.set(func(docker *fakeDocker) { docker.slowStart = 150 * time.Millisecond }) // 기한보다 느리지만 끝난다
	for round := 1; round <= 4; round++ {
		harness.clock.advance(30 * time.Second)
		requireGPUMissingButOthersIntact(t, fmt.Sprintf("느린 명령 %d", round), harness.collect())
	}
	if lines := harness.logs.snapshot(); len(lines) != 1 {
		t.Fatalf("느린 명령이 이어져도 1시간 전에는 처음 1줄뿐: %v", lines)
	}
	harness.docker.mutex.Lock()
	running := 0
	for _, exec := range harness.docker.execs {
		if exec.state == "running" {
			running++
		}
	}
	harness.docker.mutex.Unlock()
	if running > 1 {
		t.Fatalf("남는 프로세스는 최대 1개: %d", running)
	}
}

func TestResourceNoGPUSettingLogsNothing(t *testing.T) {
	harness := newResourceHarness(t, defaultResourceSettings())
	for round := 0; round < 3; round++ {
		harness.clock.advance(30 * time.Second)
		harness.collect()
	}
	if lines := harness.logs.snapshot(); len(lines) != 0 {
		t.Fatalf("GPU 설정이 없으면 GPU 결측 줄도 없다: %v", lines)
	}
}
