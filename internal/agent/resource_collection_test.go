package agent

// 수집의 흐름 — 스트림 항목 · 설정 · 배포를 막지 않음 · 기한 · 겹침 방지 · 패닉 · Redis 실패.
// 기준: requirement-spec.md(2026-11-24 resource-metrics) §2.3 · §10.2 · §11.1.
// 정한 세부(명세가 정하지 않은 것):
//   - 기한: 한 번의 수집 10초 안에 도커 조회 4초 · GPU 명령 2초, Redis 전송은 남은 기한. 기한은 ctx 로 호출마다 전달한다.
//   - Redis 로그: 연속 실패의 첫 번에 warning 1줄, 회복한 첫 성공에 info 1줄(그동안 실패한 횟수를 적는다). 성공한 수집은 로그를 남기지 않는다.
//   - 첫 수집은 시작하고 한 주기(30초) 뒤. Redis 주소가 비면 시작하지 않고 로그 1줄.

import (
	"context"
	"io"
	"net"
	"os"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/junhyeong9812/study-note-deploy-system-ci-cd/internal/shared"
)

// ===== 스트림 항목 =====

func TestResourceStreamEntryFields(t *testing.T) {
	harness := newResourceHarness(t, defaultResourceSettings())
	entry := harness.collect()
	if entry.stream != "metrics" || entry.maxLength != 20000 {
		t.Fatalf("스트림 metrics · MAXLEN ~ 20000 이어야 한다: %q %d", entry.stream, entry.maxLength)
	}
	var names []string
	for _, field := range entry.fields {
		names = append(names, field.Name)
	}
	if strings.Join(names, ",") != "kind,v,server,at,body" {
		t.Fatalf("칸은 kind · v · server · at · body 순서: %v", names)
	}
	if entry.field("kind") != "resource" || entry.field("v") != "1" || entry.field("server") != resourceServer {
		t.Fatalf("kind · v · server 가 다르다: %s", entry.text())
	}
	if entry.field("at") != strconv.FormatInt(resourceWallStart.UnixMilli(), 10) {
		t.Fatalf("at 은 수집 시각의 epoch 밀리초(벽시계): %q", entry.field("at"))
	}
	if !entry.hasDeadline {
		t.Fatal("Redis 전송에 기한(ctx)이 없다")
	}
	if size := len(entry.field("body")); size == 0 || size > 64<<10 {
		t.Fatalf("body 크기: %d", size)
	}
}

func TestResourceStreamNameFromSettings(t *testing.T) {
	settings := defaultResourceSettings()
	settings.Stream, settings.Server = "metrics-staging", "home-164"
	entry := newResourceHarness(t, settings).collect()
	if entry.stream != "metrics-staging" || entry.field("server") != "home-164" {
		t.Fatalf("설정의 스트림 · 서버 이름을 써야 한다: %q %q", entry.stream, entry.field("server"))
	}
}

// ===== 설정 =====

func TestResourceMetricsSettingsFromEnvironment(t *testing.T) {
	t.Setenv("LOG_REDIS_URL", "redis://10.0.0.5:6379")
	t.Setenv("METRICS_STREAM", "")
	t.Setenv("SERVER_NAME", "ci-agent-9")
	t.Setenv("METRICS_EXTRA_CONTAINERS", " claude-bridge-relay, ,other-tool ")
	t.Setenv("METRICS_GPU_CONTAINER", "llm-ollama")
	settings := LoadResourceMetricsSettings()
	want := ResourceMetricsSettings{RedisAddress: "10.0.0.5:6379", Stream: "metrics", Server: "ci-agent-9",
		ExtraContainers: []string{"claude-bridge-relay", "other-tool"}, GPUContainer: "llm-ollama"}
	if !reflect.DeepEqual(settings, want) {
		t.Fatalf("설정:\n got %+v\nwant %+v", settings, want)
	}

	t.Setenv("LOG_REDIS_URL", "")
	t.Setenv("METRICS_STREAM", "metrics-x")
	t.Setenv("SERVER_NAME", "")
	t.Setenv("MODE", "agent")
	t.Setenv("METRICS_EXTRA_CONTAINERS", "")
	t.Setenv("METRICS_GPU_CONTAINER", "")
	settings = LoadResourceMetricsSettings()
	if settings.RedisAddress != "" || settings.Stream != "metrics-x" || settings.Server != "ci-agent" ||
		len(settings.ExtraContainers) != 0 || settings.GPUContainer != "" {
		t.Fatalf("빈 설정 · 기본값: %+v", settings)
	}
}

func TestResourceCollectorDefaults(t *testing.T) {
	agent := newPipelineAgent(t, &recordedCommands{})
	collector := agent.newResourceCollector(defaultResourceSettings())
	if collector.deadline != 10*time.Second || collector.dockerDeadline != 4*time.Second || collector.gpuDeadline != 2*time.Second {
		t.Fatalf("기한 기본값(전체 10초 · 도커 4초 · GPU 2초): %v %v %v", collector.deadline, collector.dockerDeadline, collector.gpuDeadline)
	}
	if collector.procRoot != "/proc" || collector.diskPath != "/" {
		t.Fatalf("읽는 곳: %q %q", collector.procRoot, collector.diskPath)
	}
	if collector.statfs == nil || collector.docker == nil || collector.monotonicNow == nil || collector.wallNow == nil || collector.logf == nil {
		t.Fatalf("운영 이음매가 비어 있다: %+v", collector)
	}
	if _, isRedis := collector.stream.(*shared.RedisStream); !isRedis {
		t.Fatalf("스트림은 Redis 여야 한다: %T", collector.stream)
	}
	if resourceMetricsInterval != 30*time.Second {
		t.Fatalf("수집 주기는 30초: %v", resourceMetricsInterval)
	}
	first := collector.monotonicNow()
	time.Sleep(2 * time.Millisecond)
	if collector.monotonicNow() <= first {
		t.Fatal("단조 시계가 흐르지 않는다")
	}
}

// ===== 배포를 막지 않는다 =====

func TestResourceCollectionRunsWhileDeployLockIsHeld(t *testing.T) {
	harness := newResourceHarness(t, defaultResourceSettings())
	harness.agent.busy.Lock()
	defer harness.agent.busy.Unlock()
	harness.collect() // busy 를 기다리거나 건너뛰면 항목이 오지 않는다
}

func TestDeployIsNotBusyWhileResourceCollectionRuns(t *testing.T) {
	harness := newResourceHarness(t, defaultResourceSettings())
	harness.collector.dockerDeadline = 500 * time.Millisecond
	harness.docker.set(func(docker *fakeDocker) { docker.stallList = true })
	harness.tick()
	waitForDockerRequest(t, harness.docker, "/containers/json")
	if !harness.collector.Stats().InFlight {
		t.Fatal("도커가 멈춘 동안 수집은 도는 중이어야 한다")
	}
	for _, path := range []string{"/agent/ci", "/agent/cd"} {
		if recorder := post(harness.agent, path); recorder.Code != 200 {
			t.Fatalf("수집이 도는 중에 %s 가 막혔다(agent_busy 면 수집이 잠금을 잡은 것): %d %s", path, recorder.Code, recorder.Body.String())
		}
	}
	harness.waitStats("멈춘 수집이 기한에 끝남", func(stats ResourceCollectorStats) bool { return stats.Sent == 1 && !stats.InFlight })
}

func waitForDockerRequest(t *testing.T, docker *fakeDocker, pathFragment string) {
	t.Helper()
	deadline := time.Now().Add(resourceWait)
	for time.Now().Before(deadline) {
		for _, request := range docker.recorded() {
			if strings.Contains(request.path, pathFragment) {
				return
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("도커에 %s 요청이 오지 않았다: %v", pathFragment, docker.recorded())
}

// ===== 기한 — 호출마다 전달된다 =====

func requireReleasedByDeadline(t *testing.T, docker *fakeDocker, situation string) {
	t.Helper()
	select {
	case release := <-docker.releases:
		if !release.byClient || release.after > time.Second {
			t.Fatalf("%s: 멈춘 요청이 기한(ctx)으로 풀려나야 한다: %+v", situation, release)
		}
	case <-time.After(resourceWait):
		t.Fatalf("%s: 멈춘 요청이 풀려나지 않았다", situation)
	}
}

func TestResourceDeadlineReleasesStalledDockerList(t *testing.T) {
	harness := newContainerHarness(t)
	harness.collector.dockerDeadline = 300 * time.Millisecond
	harness.docker.set(func(docker *fakeDocker) { docker.stallList = true })
	startedAt := time.Now()
	entry := harness.collect()
	if elapsed := time.Since(startedAt); elapsed > 1500*time.Millisecond {
		t.Fatalf("도커 기한(300ms)을 넘겨 붙잡혔다: %v", elapsed)
	}
	requireReleasedByDeadline(t, harness.docker, "목록 조회")
	if containers := listOf(t, entry, "containers"); len(containers) != 0 {
		t.Fatalf("목록이 기한을 넘기면 containers 는 []: %v", containers)
	}
	requireMissing(t, entry, "도커 목록 기한 초과", missingContainers)
	requireHostLevels(t, "도커가 멈춰도 host 는 그대로", hostOf(t, entry), 0.11)
}

func TestResourceDeadlineReleasesStalledContainerStats(t *testing.T) {
	harness := newContainerHarness(t)
	harness.collector.dockerDeadline = 300 * time.Millisecond
	harness.docker.set(func(docker *fakeDocker) { docker.stallStats = true })
	startedAt := time.Now()
	entry := harness.collect()
	if elapsed := time.Since(startedAt); elapsed > 1500*time.Millisecond {
		t.Fatalf("도커 기한(300ms)을 넘겨 붙잡혔다: %v", elapsed)
	}
	requireReleasedByDeadline(t, harness.docker, "통계 조회")
	requireHostLevels(t, "통계가 멈춰도 host 는 그대로", hostOf(t, entry), 0.11)
}

func TestResourceDeadlineReleasesStalledGPUCommand(t *testing.T) {
	harness := newGPUHarness(t, "0, 0, 9, 8192, 28\n", 0)
	harness.collector.gpuDeadline = 300 * time.Millisecond
	harness.docker.set(func(docker *fakeDocker) { docker.stallExecStart = true })
	startedAt := time.Now()
	entry := harness.collect()
	if elapsed := time.Since(startedAt); elapsed > 1500*time.Millisecond {
		t.Fatalf("GPU 기한(300ms)을 넘겨 붙잡혔다: %v", elapsed)
	}
	requireReleasedByDeadline(t, harness.docker, "GPU 명령")
	requireMissing(t, entry, "GPU 명령 기한 초과", missingGPUs)
	if containers := listOf(t, entry, "containers"); len(containers) != 1 {
		t.Fatalf("GPU 가 멈춰도 컨테이너는 그대로: %v", containers)
	}
}

func TestResourceDeadlineReleasesStalledStreamSend(t *testing.T) {
	harness := newResourceHarness(t, defaultResourceSettings())
	harness.collector.deadline = 300 * time.Millisecond
	harness.stream.set(func(stream *fakeStream) { stream.stall = true })
	startedAt := time.Now()
	stats := harness.collectOnce()
	if elapsed := time.Since(startedAt); elapsed > 1500*time.Millisecond {
		t.Fatalf("전체 기한(300ms)을 넘겨 붙잡혔다: %v", elapsed)
	}
	if stats.SendFailures != 1 || stats.Sent != 0 {
		t.Fatalf("기한에 풀려난 전송은 실패로 센다: %+v", stats)
	}
	select {
	case after := <-harness.stream.released:
		if after > time.Second {
			t.Fatalf("전송이 너무 늦게 풀려났다: %v", after)
		}
	case <-time.After(resourceWait):
		t.Fatal("멈춘 전송이 ctx 로 풀려나지 않았다")
	}
	harness.stream.set(func(stream *fakeStream) { stream.stall = false })
	harness.collect() // 다음 주기는 그대로 돈다
}

// ===== 겹침 방지 =====

func TestResourceSkipsTicksWhileUnfinishedCollectionRemains(t *testing.T) {
	harness := newResourceHarness(t, defaultResourceSettings())
	harness.collector.deadline = 200 * time.Millisecond
	release := make(chan struct{})
	harness.setStatfs(func(path string, stat *syscall.Statfs_t) error {
		<-release // 기한을 모르는 호출(멈춘 NFS 의 statfs 처럼) — ctx 로 풀 수 없다
		stat.Blocks, stat.Bfree, stat.Bsize = 1000, 250, 4096
		return nil
	})
	released := false
	defer func() {
		if !released {
			close(release)
		}
	}()
	harness.tick()
	harness.waitStats("첫 수집 시작", func(stats ResourceCollectorStats) bool { return stats.Collections == 1 })
	waitForStatfsCalls(t, harness, 1)
	time.Sleep(300 * time.Millisecond) // 기한은 지났지만 수집은 실제로 끝나지 않았다
	for range 3 {
		harness.tick()
	}
	stats := harness.waitStats("건너뛴 주기 3", func(stats ResourceCollectorStats) bool { return stats.Skipped == 3 })
	if stats.Collections != 1 || !stats.InFlight {
		t.Fatalf("끝나지 않은 수집이 있으면 새 수집을 시작하지 않는다: %+v", stats)
	}
	if calls := statfsCallCount(harness); calls != 1 {
		t.Fatalf("멈춘 호출이 있는데 다시 불렀다(겹침): %d", calls)
	}

	close(release)
	released = true
	harness.waitStats("멈춘 수집이 실제로 끝남", func(stats ResourceCollectorStats) bool { return !stats.InFlight })
	harness.setStatfs(nil)
	stats = harness.collectOnce()
	if stats.Collections != 2 || stats.Skipped != 3 {
		t.Fatalf("끝난 뒤에는 다음 주기가 돈다: %+v", stats)
	}
}

func statfsCallCount(harness *resourceHarness) int {
	harness.statfsMutex.Lock()
	defer harness.statfsMutex.Unlock()
	return len(harness.statfsPaths)
}

func waitForStatfsCalls(t *testing.T, harness *resourceHarness, count int) {
	t.Helper()
	deadline := time.Now().Add(resourceWait)
	for statfsCallCount(harness) < count {
		if time.Now().After(deadline) {
			t.Fatalf("statfs 가 %d 번 불리지 않았다: %d", count, statfsCallCount(harness))
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// ===== 패닉 =====

func TestResourcePanicDoesNotStopCollector(t *testing.T) {
	cases := map[string]func(harness *resourceHarness){
		"수집 중 패닉": func(harness *resourceHarness) {
			harness.setStatfs(func(string, *syscall.Statfs_t) error { panic("가짜 statfs 패닉") })
		},
		"전송 중 패닉": func(harness *resourceHarness) {
			harness.stream.set(func(stream *fakeStream) { stream.panicNext = true })
		},
	}
	for situation, breakIt := range cases {
		t.Run(situation, func(t *testing.T) {
			harness := newResourceHarness(t, defaultResourceSettings())
			breakIt(harness)
			if stats := harness.collectOnce(); stats.Panics != 1 {
				t.Fatalf("패닉을 되살려 세야 한다: %+v", stats)
			}
			var errorLines int
			for _, line := range harness.logs.snapshot() {
				if line.level == "error" {
					errorLines++
				}
			}
			if errorLines != 1 {
				t.Fatalf("패닉은 error 로그 1줄: %v", harness.logs.snapshot())
			}

			harness.setStatfs(nil)
			harness.clock.advance(30 * time.Second)
			harness.collectOnce()
			wantAt := strconv.FormatInt(harness.clock.wallNow().UnixMilli(), 10)
			for {
				select {
				case entry := <-harness.stream.entries:
					if entry.field("at") != wantAt {
						continue // 패닉 난 수집이 남긴 항목
					}
					requireHostLevels(t, "패닉 다음 주기", hostOf(t, entry), 0.11)
					return
				case <-time.After(resourceWait):
					t.Fatal("패닉 다음 주기의 항목이 없다")
				}
			}
		})
	}
}

// ===== Redis =====

func TestResourceSendFailuresAreCountedAndLoggedOnceUntilRecovery(t *testing.T) {
	harness := newResourceHarness(t, defaultResourceSettings())
	harness.stream.set(func(stream *fakeStream) { stream.failNext = 3 })
	for range 3 {
		harness.collectOnce()
	}
	if stats := harness.collector.Stats(); stats.SendFailures != 3 || stats.Sent != 0 {
		t.Fatalf("실패 3번: %+v", stats)
	}
	lines := harness.logs.snapshot()
	if len(lines) != 1 || lines[0].level != "warning" {
		t.Fatalf("연속 실패는 처음 1줄(warning)만: %v", lines)
	}

	harness.collect()
	lines = harness.logs.snapshot()
	if len(lines) != 2 || lines[1].level != "info" || !strings.Contains(lines[1].message, "3") {
		t.Fatalf("회복하면 1줄(info, 실패 횟수 3): %v", lines)
	}
	harness.collect()
	if lines := harness.logs.snapshot(); len(lines) != 2 {
		t.Fatalf("성공한 수집은 로그를 남기지 않는다: %v", lines)
	}

	harness.stream.set(func(stream *fakeStream) { stream.failNext = 1 })
	harness.collectOnce()
	lines = harness.logs.snapshot()
	if len(lines) != 3 || lines[2].level != "warning" {
		t.Fatalf("새로 시작한 연속 실패는 다시 1줄: %v", lines)
	}
	if stats := harness.collector.Stats(); stats.SendFailures != 4 || stats.Sent != 2 {
		t.Fatalf("횟수: %+v", stats)
	}
}

func closedAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	listener.Close()
	return address
}

func TestResourceDeadRedisLeavesAgentAndDeploysIntact(t *testing.T) {
	harness := newResourceHarness(t, defaultResourceSettings())
	harness.collector.stream = shared.NewRedisStream(closedAddress(t))
	for range 2 {
		harness.collectOnce()
	}
	if stats := harness.collector.Stats(); stats.SendFailures != 2 || stats.Sent != 0 {
		t.Fatalf("닿지 못한 Redis 는 실패로 센다: %+v", stats)
	}
	for _, path := range []string{"/agent/ci", "/agent/cd"} {
		if recorder := post(harness.agent, path); recorder.Code != 200 {
			t.Fatalf("Redis 가 죽어도 %s 는 그대로: %d %s", path, recorder.Code, recorder.Body.String())
		}
	}
	harness.collectOnce() // 다음 주기도 돈다
}

// ===== 시작 =====

func captureStandardOutput(t *testing.T, run func()) string {
	t.Helper()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	original := os.Stdout
	os.Stdout = writer
	captured := make(chan string)
	go func() {
		content, _ := io.ReadAll(reader)
		captured <- string(content)
	}()
	func() {
		defer func() { os.Stdout = original }()
		run()
	}()
	writer.Close()
	return <-captured
}

func TestResourceMetricsDisabledWithoutRedisAddress(t *testing.T) {
	t.Setenv("LOG_REDIS_URL", "")
	agent := newPipelineAgent(t, &recordedCommands{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var collector *ResourceCollector
	output := captureStandardOutput(t, func() { collector = agent.StartResourceMetrics(ctx) })
	if collector != nil {
		t.Fatal("Redis 주소가 비면 수집을 시작하지 않는다")
	}
	if lines := strings.Split(strings.TrimSpace(output), "\n"); strings.TrimSpace(output) == "" || len(lines) != 1 {
		t.Fatalf("시작하지 않는다는 로그 1줄: %q", output)
	}
}

func TestResourceMetricsStartsWithRedisAddress(t *testing.T) {
	t.Setenv("LOG_REDIS_URL", "")
	agent := newPipelineAgent(t, &recordedCommands{}) // 로그는 stdout 만(아래 주소로 로그를 보내지 않게 먼저 만든다)
	t.Setenv("LOG_REDIS_URL", "redis://"+closedAddress(t))
	t.Setenv("METRICS_GPU_CONTAINER", "llm-ollama")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	collector := agent.StartResourceMetrics(ctx)
	if collector == nil {
		t.Fatal("Redis 주소가 있으면 수집을 시작한다")
	}
	if collector.settings.GPUContainer != "llm-ollama" || collector.settings.Stream != "metrics" {
		t.Fatalf("환경 변수의 설정으로 시작한다: %+v", collector.settings)
	}
	if stats := collector.Stats(); stats.Collections != 0 {
		t.Fatalf("첫 수집은 한 주기 뒤: %+v", stats)
	}
}

// §13: 주소가 비었거나 해석할 수 없어 수집을 끄는 줄은 그 둘을 함께 말한다 — 해석할 수 없는 값을 "비어 있음"으로만 말하지 않는다.
func TestResourceMetricsDisabledLineCoversUnparsableAddress(t *testing.T) {
	disabledLine := func(rawURL string) string {
		t.Setenv("LOG_REDIS_URL", "")
		agent := newPipelineAgent(t, &recordedCommands{})
		t.Setenv("LOG_REDIS_URL", rawURL)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		var collector *ResourceCollector
		output := strings.TrimSpace(captureStandardOutput(t, func() { collector = agent.StartResourceMetrics(ctx) }))
		if collector != nil || output == "" || strings.Contains(output, "\n") {
			t.Fatalf("%q: 시작하지 않고 1줄: %v %q", rawURL, collector != nil, output)
		}
		return strings.ToLower(output)
	}
	line := disabledLine("not-a-redis-address")
	saysUnparsable := false
	for _, word := range []string{"invalid", "unparsable", "unparseable", "parse", "해석", "잘못", "틀린"} {
		if strings.Contains(line, word) {
			saysUnparsable = true
		}
	}
	if !saysUnparsable {
		t.Fatalf("해석할 수 없는 주소를 비어 있음으로만 말한다: %q", line)
	}
}
