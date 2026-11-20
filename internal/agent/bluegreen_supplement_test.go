package agent

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// blind 테스트(bluegreen_test.go)가 다루지 않은 것: 되돌림 실패 보고, CI의 운영 버전 판정, 성공 뒤 이미지 정리.

func TestBlueGreenReportsWhenUndoItselfFails(t *testing.T) {
	agent, world := slotNewAgent(t, slotSetup{activeContent: slotBlueTemplate, servingSlot: "blue"})
	stopCalls := 0
	var mutex sync.Mutex
	agent.run = func(ctx context.Context, directory string, environment []string, arguments ...string) (string, error) {
		line := strings.Join(arguments, " ")
		mutex.Lock()
		defer mutex.Unlock()
		if strings.Contains(line, "stop wrapper-green") {
			stopCalls++
			if stopCalls > 1 { // 2번(자리 비우기)은 성공, 실패 뒤의 정지는 실패
				return "cannot stop", errors.New("exit status 1")
			}
		}
		if strings.Contains(line, "up -d") {
			return "no such image", errors.New("exit status 1")
		}
		return world.run(ctx, directory, environment, arguments...)
	}
	recorder := slotPost(agent, "2222222222222222222222222222222222222222")
	slotRequireFailure(t, recorder, "cd_up")
	if !strings.Contains(recorder.Body.String(), "되돌림 실패") {
		t.Fatalf("되돌림(대상 자리 정지)이 실패했는데 응답에 드러나지 않는다: %s", recorder.Body.String())
	}
}

func TestBlueGreenIntegrationBuildsOneSlotAndRestoresToActiveSlotVersion(t *testing.T) {
	agent, _ := slotNewAgent(t, slotSetup{activeContent: slotGreenTemplate, servingSlot: "green"})
	const headCommit, runningCommit, requestCommit = "1111111111111111111111111111111111111111",
		"3333333333333333333333333333333333333333", "2222222222222222222222222222222222222222"
	var lines []string
	agent.run = func(_ context.Context, _ string, _ []string, arguments ...string) (string, error) {
		line := strings.Join(arguments, " ")
		lines = append(lines, line)
		switch {
		case strings.Contains(line, "rev-parse"):
			return headCommit + "\n", nil
		case strings.Contains(line, "compose ps -q"):
			return "container-1\n", nil
		case strings.HasPrefix(line, "docker inspect"):
			return "llm-wrapper:" + runningCommit + "\n", nil
		case strings.Contains(line, "compose build"):
			return "build failed", errors.New("exit status 1")
		}
		return "", nil
	}
	if _, err := agent.integrate("llm", requestCommit); err == nil {
		t.Fatal("빌드 실패가 오류로 돌아오지 않았다")
	}
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "docker compose ps -q wrapper-green") {
		t.Fatalf("운영 버전을 활성 자리(green)에서 읽지 않았다:\n%s", joined)
	}
	if !strings.Contains(joined, "docker compose build wrapper-blue") {
		t.Fatalf("이미지는 한쪽 자리(blue)로 빌드해야 한다:\n%s", joined)
	}
	if last := lines[len(lines)-1]; !strings.HasSuffix(last, "reset --hard "+runningCommit) {
		t.Fatalf("소스를 활성 자리의 버전으로 되돌리지 않았다: %s", last)
	}
}

func TestBlueGreenRemovesOldImagesAfterCleanup(t *testing.T) {
	agent, world := slotNewAgent(t, slotSetup{activeContent: slotBlueTemplate, servingSlot: "blue"})
	const requestCommit = "2222222222222222222222222222222222222222"
	var removed []string
	var mutex sync.Mutex
	agent.run = func(ctx context.Context, directory string, environment []string, arguments ...string) (string, error) {
		line := strings.Join(arguments, " ")
		switch {
		case strings.Contains(line, "compose ps -q"):
			return "container-1\n", nil
		case strings.HasPrefix(line, "docker inspect"):
			return "llm-wrapper:" + requestCommit + "\n", nil
		case strings.HasPrefix(line, "docker images"):
			return strings.Join([]string{
				"2026-11-20 16:00:00 +0900 KST\t" + requestCommit,
				"2026-11-20 15:00:00 +0900 KST\taaaaaaa5", "2026-11-20 14:00:00 +0900 KST\taaaaaaa4",
				"2026-11-20 13:00:00 +0900 KST\taaaaaaa3", "2026-11-20 12:00:00 +0900 KST\taaaaaaa2",
				"2026-11-20 11:00:00 +0900 KST\taaaaaaa1",
			}, "\n"), nil
		case strings.HasPrefix(line, "docker rmi"):
			mutex.Lock()
			removed = append(removed, line)
			mutex.Unlock()
			return "", nil
		}
		return world.run(ctx, directory, environment, arguments...)
	}
	if recorder := slotPost(agent, requestCommit); recorder.Code != 200 {
		t.Fatalf("전환 성공이어야 한다: %d %s", recorder.Code, recorder.Body.String())
	}
	slotWaitBackground(t, agent)
	mutex.Lock()
	defer mutex.Unlock()
	if strings.Join(removed, ",") != "docker rmi llm-wrapper:aaaaaaa1" {
		t.Fatalf("배포한 것 포함 5개만 남겨야 한다: %v", removed)
	}
}

// ---- 리뷰 반영 검증: 실제로 응답 중인 자리는 어떤 경우에도 내리지 않는다 ----

const supplementCommit = "2222222222222222222222222222222222222222"

func supplementStopped(world *slotWorld, slot string) bool {
	return slotCount(world.snapshot(), "run", "stop wrapper-"+slot) > 0
}

func TestBlueGreenKeepsBothSlotsWhenRevertCannotBeConfirmed(t *testing.T) {
	// 7단계 reload가 오류를 돌려줬지만 실제로는 적용됐고(nginx는 green으로 보냄), 되돌림 reload도 실패한다.
	agent, world := slotNewAgent(t, slotSetup{activeContent: slotBlueTemplate, servingSlot: "blue"})
	agent.run = func(ctx context.Context, directory string, environment []string, arguments ...string) (string, error) {
		line := strings.Join(arguments, " ")
		if strings.Contains(line, "nginx -s reload") {
			world.mutex.Lock()
			world.servingSlot = "green"
			world.events = append(world.events, slotEvent{kind: "run", text: line})
			world.mutex.Unlock()
			return "signal process failed", errors.New("exit status 1")
		}
		return world.run(ctx, directory, environment, arguments...)
	}
	recorder := slotPost(agent, supplementCommit)
	slotRequireFailure(t, recorder, "cd_switch")
	if !strings.Contains(recorder.Body.String(), "되돌림 미확인") {
		t.Fatalf("구 자리 복귀를 확인하지 못했는데 응답에 드러나지 않는다: %s", recorder.Body.String())
	}
	slotWaitBackground(t, agent)
	if slotCount(world.snapshot(), "run", "up -d") != 1 {
		t.Fatalf("테스트 전제 오류 — 대상 자리가 기동되지 않았다:\n%s", slotDescribe(world.snapshot()))
	}
	stopsAfterStart := 0
	started := false
	for _, event := range world.snapshot() {
		if event.kind == "run" && strings.Contains(event.text, "up -d") {
			started = true
		}
		if started && event.kind == "run" && strings.Contains(event.text, "compose stop") {
			stopsAfterStart++
		}
	}
	if stopsAfterStart != 0 {
		t.Fatalf("nginx가 어느 자리로 보내는지 모르는데 자리를 내렸다:\n%s", slotDescribe(world.snapshot()))
	}
}

func TestDrainNeverStopsTheSlotThatIsServing(t *testing.T) {
	agent, world := slotNewAgent(t, slotSetup{activeContent: slotBlueTemplate, servingSlot: "blue"})
	agent.startDrain("llm", "blue", "") // 정리 대상이 실제로는 응답 중인 자리
	slotWaitBackground(t, agent)
	if supplementStopped(world, "blue") {
		t.Fatalf("응답 중인 자리를 내렸다:\n%s", slotDescribe(world.snapshot()))
	}
}

func TestDrainDoesNotStopWhenServingSlotIsUnknown(t *testing.T) {
	agent, world := slotNewAgent(t, slotSetup{activeContent: slotBlueTemplate, servingSlot: "blue"})
	world.mutex.Lock()
	world.probeError = errors.New("connection refused")
	world.mutex.Unlock()
	agent.startDrain("llm", "green", "")
	slotWaitBackground(t, agent)
	if supplementStopped(world, "green") || supplementStopped(world, "blue") {
		t.Fatalf("nginx 상태를 모르는데 자리를 내렸다:\n%s", slotDescribe(world.snapshot()))
	}
}

func TestNextDeliveryIsRefusedWhenCleanupNeverFinishes(t *testing.T) {
	agent, world := slotNewAgent(t, slotSetup{activeContent: slotBlueTemplate, servingSlot: "blue"})
	agent.drainLimit, agent.drainWaitMargin = 20*time.Millisecond, 30*time.Millisecond
	stuck := make(chan struct{})
	t.Cleanup(func() { close(stuck) })
	agent.run = func(ctx context.Context, directory string, environment []string, arguments ...string) (string, error) {
		if strings.Contains(strings.Join(arguments, " "), "stop wrapper-green") {
			<-stuck // 자리 정지가 끝나지 않는다
		}
		return world.run(ctx, directory, environment, arguments...)
	}
	agent.startDrain("llm", "green", "")
	started := time.Now()
	recorder := slotPost(agent, supplementCommit)
	slotRequireFailure(t, recorder, "cd_up")
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("끝나지 않는 정리를 상한 없이 기다렸다: %v", elapsed)
	}
	if slotCount(world.snapshot(), "run", "up -d") != 0 {
		t.Fatal("앞선 정리가 끝나지 않았는데 새 자리를 기동했다")
	}
}

func TestIntegrationUsesServingSlotNotActiveFile(t *testing.T) {
	// 활성 파일은 green인데 nginx는 blue로 보낸다(파일을 바꾼 직후 agent가 죽은 경우) — 운영 버전은 blue의 것
	agent, _ := slotNewAgent(t, slotSetup{activeContent: slotGreenTemplate, servingSlot: "blue"})
	var lines []string
	agent.run = func(_ context.Context, _ string, _ []string, arguments ...string) (string, error) {
		line := strings.Join(arguments, " ")
		lines = append(lines, line)
		switch {
		case strings.Contains(line, "rev-parse"):
			return "1111111111111111111111111111111111111111\n", nil
		case strings.Contains(line, "compose ps -q"):
			return "container-1\n", nil
		case strings.HasPrefix(line, "docker inspect"):
			return "llm-wrapper:3333333333333333333333333333333333333333\n", nil
		case strings.Contains(line, "--target test"):
			return "FAILED", errors.New("exit status 1")
		}
		return "", nil
	}
	if _, err := agent.integrate("llm", supplementCommit); err == nil {
		t.Fatal("테스트 실패가 오류로 돌아오지 않았다")
	}
	if joined := strings.Join(lines, "\n"); !strings.Contains(joined, "docker compose ps -q wrapper-blue") || strings.Contains(joined, "ps -q wrapper-green") {
		t.Fatalf("운영 버전을 실제 응답 자리(blue)에서 읽어야 한다:\n%s", joined)
	}
}

func TestReconcileRetriesUntilServingSlotIsKnown(t *testing.T) {
	// 서버가 막 부팅했다: 처음 몇 번은 외부 포트가 응답하지 않다가 뜬다
	agent, world := slotNewAgent(t, slotSetup{activeContent: slotBlueTemplate, servingSlot: "blue"})
	attempts := 0
	var mutex sync.Mutex
	agent.probeSlot = func(healthURL string) (string, error) {
		mutex.Lock()
		attempts++
		current := attempts
		mutex.Unlock()
		if current <= 2 {
			return "", errors.New("connection refused")
		}
		return world.probe(healthURL)
	}
	agent.ReconcileSlotsUntil(2*time.Second, 5*time.Millisecond)
	slotWaitBackground(t, agent)
	if !supplementStopped(world, "green") || supplementStopped(world, "blue") {
		t.Fatalf("응답이 돌아온 뒤 반대쪽 자리(green)만 정리해야 한다:\n%s", slotDescribe(world.snapshot()))
	}
}

func TestReconcileGivesUpAfterBudgetWithoutStopping(t *testing.T) {
	agent, world := slotNewAgent(t, slotSetup{activeContent: slotBlueTemplate, servingSlot: "blue"})
	world.mutex.Lock()
	world.probeError = errors.New("connection refused")
	world.mutex.Unlock()
	started := time.Now()
	agent.ReconcileSlotsUntil(50*time.Millisecond, 5*time.Millisecond)
	slotWaitBackground(t, agent)
	if time.Since(started) > 2*time.Second || slotRunCount(world.snapshot()) != 0 {
		t.Fatalf("상한 안에 포기하고 아무것도 실행하지 않아야 한다:\n%s", slotDescribe(world.snapshot()))
	}
}

func TestReconcileAlignsActiveFileWithServingSlot(t *testing.T) {
	// 활성 파일은 green으로 바뀌었는데 reload 전에 agent가 죽었다: nginx는 여전히 blue로 보낸다
	agent, world := slotNewAgent(t, slotSetup{activeContent: slotGreenTemplate, servingSlot: "blue"})
	agent.ReconcileSlots()
	slotWaitBackground(t, agent)
	slotRequireActiveContent(t, world, slotBlueTemplate, "기동 시 정리 뒤")
	if supplementStopped(world, "blue") {
		t.Fatal("응답 중인 자리를 내렸다")
	}
}

func TestIntegrationFallsBackToActiveFileWhenProbeFails(t *testing.T) {
	agent, world := slotNewAgent(t, slotSetup{activeContent: slotGreenTemplate, servingSlot: "green"})
	world.mutex.Lock()
	world.probeError = errors.New("connection refused") // 서비스가 죽어 있다 — 고치는 배포
	world.mutex.Unlock()
	var lines []string
	agent.run = func(_ context.Context, _ string, _ []string, arguments ...string) (string, error) {
		line := strings.Join(arguments, " ")
		lines = append(lines, line)
		if strings.Contains(line, "rev-parse") {
			return "1111111111111111111111111111111111111111\n", nil
		}
		return "", nil
	}
	if _, err := agent.integrate("llm", supplementCommit); err != nil {
		t.Fatalf("CI는 진행돼야 한다: %v", err)
	}
	if joined := strings.Join(lines, "\n"); !strings.Contains(joined, "docker compose ps -q wrapper-green") {
		t.Fatalf("응답이 없으면 활성 파일의 자리를 차선으로 써야 한다:\n%s", joined)
	}
}

func TestReconcileDoesNotStopWhenActiveFileCannotBeAligned(t *testing.T) {
	// 파일은 green, nginx는 blue. green을 내리려면 먼저 파일을 blue로 고쳐야 하는데 blue 템플릿이 없다.
	agent, world := slotNewAgent(t, slotSetup{activeContent: slotGreenTemplate, servingSlot: "blue"})
	if err := os.Remove(filepath.Join(agent.directories["llm"], "deploy", "nginx", "upstream-blue.conf")); err != nil {
		t.Fatal(err)
	}
	agent.ReconcileSlots()
	slotWaitBackground(t, agent)
	if supplementStopped(world, "green") || supplementStopped(world, "blue") {
		t.Fatalf("활성 파일이 가리키는 자리를 내렸다 — nginx가 다시 뜨면 갈 곳이 없다:\n%s", slotDescribe(world.snapshot()))
	}
	slotRequireActiveContent(t, world, slotGreenTemplate, "맞추지 못한 경우")
}

func TestReconcileStaysOutOfDeliveryInProgress(t *testing.T) {
	// CD가 대상 자리(green)를 띄우는 동안 기동 시 정리가 그 자리를 정리 대상으로 삼으면 안 된다
	agent, world := slotNewAgent(t, slotSetup{activeContent: slotBlueTemplate, servingSlot: "blue"})
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	agent.waitHealthy = func(healthURL string, budget time.Duration) error {
		once.Do(func() { close(started) })
		<-release // CD가 대상 자리의 헬스를 기다리는 중
		return world.waitHealthy(healthURL, budget)
	}
	delivered := make(chan int, 1)
	go func() { delivered <- slotPost(agent, supplementCommit).Code }()
	<-started
	agent.ReconcileSlots() // 배포 중에는 아무것도 하지 않고 돌아온다
	slotWaitBackground(t, agent)
	if slotCount(world.snapshot(), "notify", "") != 0 || slotCount(world.snapshot(), "run", "stop wrapper-green") != 1 {
		t.Fatalf("배포 중인데 기동 시 정리가 끼어들었다(대상 자리 정지는 2단계의 1회뿐이어야 한다):\n%s", slotDescribe(world.snapshot()))
	}
	close(release)
	if code := <-delivered; code != 200 {
		t.Fatalf("CD는 성공해야 한다: %d", code)
	}
	slotWaitBackground(t, agent)
	events := world.snapshot()
	if last := slotFind(events, "run", "up -d", 0); last < 0 || slotFind(events, "run", "stop wrapper-green", last) >= 0 {
		t.Fatalf("전환이 끝난 뒤 새 활성 자리(green)를 내렸다:\n%s", slotDescribe(events))
	}
}

func TestRevertIsConfirmedByMarkerEvenWhenOldSlotIsDown(t *testing.T) {
	// 고치는 배포: 구 자리(blue)는 죽어 502 + 표식 blue. 8단계에서 새 자리가 응답하지 않아 되돌린다.
	// "blue로 돌아왔다"는 표식만으로 확인되므로 새 자리(green)는 정리로 넘어가야 한다(두 벌 방치 금지).
	agent, world := slotNewAgent(t, slotSetup{activeContent: slotBlueTemplate, servingSlot: "blue"})
	world.mutex.Lock()
	world.reloadIgnored = true // reload가 적용되지 않는다 → 8단계 실패
	world.mutex.Unlock()
	agent.probeSlot = func(healthURL string) (string, error) {
		slot, err := world.probe(healthURL)
		if err == nil && slot == "blue" {
			return slot, errors.New("status 502")
		}
		return slot, err
	}
	recorder := slotPost(agent, supplementCommit)
	slotRequireFailure(t, recorder, "cd_switch")
	if strings.Contains(recorder.Body.String(), "되돌림 미확인") {
		t.Fatalf("표식으로 복귀가 확인되는데 미확인으로 보고했다: %s", recorder.Body.String())
	}
	slotWaitBackground(t, agent)
	events := world.snapshot()
	if started := slotFind(events, "run", "up -d", 0); started < 0 || slotFind(events, "run", "stop wrapper-green", started) < 0 {
		t.Fatalf("되돌린 뒤 새 자리를 정리하지 않았다:\n%s", slotDescribe(events))
	}
}

func TestProxyConfigIsCopiedOnSwitchAndRestoredOnFailure(t *testing.T) {
	agent, world := slotNewAgent(t, slotSetup{activeContent: slotBlueTemplate, servingSlot: "blue"})
	directory := agent.directories["llm"]
	source := filepath.Join(directory, "deploy", "nginx", "nginx.conf")
	active := filepath.Join(directory, "deploy", "active", "nginx.conf")
	write := func(path, content string) {
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	read := func(path string) string {
		content, _ := os.ReadFile(path)
		return string(content)
	}
	write(active, "# 운영 중인 설정\n")
	write(source, "# 새 커밋의 설정\n")

	world.setFailure("nginx -t", 1) // 새 설정이 검사에 실패
	slotRequireFailure(t, slotPost(agent, supplementCommit), "cd_switch")
	slotWaitBackground(t, agent)
	if read(active) != "# 운영 중인 설정\n" {
		t.Fatalf("설정 검사에 실패했는데 nginx 본 설정이 이전 내용으로 돌아오지 않았다: %q", read(active))
	}

	if recorder := slotPost(agent, supplementCommit); recorder.Code != 200 {
		t.Fatalf("두 번째 배포는 성공해야 한다: %d %s", recorder.Code, recorder.Body.String())
	}
	slotWaitBackground(t, agent)
	if read(active) != "# 새 커밋의 설정\n" {
		t.Fatalf("전환에 성공했는데 nginx 본 설정이 새 내용으로 바뀌지 않았다: %q", read(active))
	}
}

func TestProbeSlotReturnsMarkerEvenWhenStatusIsNotOk(t *testing.T) {
	respond := func(status int, slot string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			if slot != "" {
				writer.Header().Set("X-Deploy-Slot", slot)
			}
			writer.WriteHeader(status)
		}))
	}
	healthy, down, unmarked := respond(200, "green"), respond(502, "blue"), respond(200, "")
	defer healthy.Close()
	defer down.Close()
	defer unmarked.Close()
	if slot, err := probeSlot(healthy.URL); slot != "green" || err != nil {
		t.Fatalf("200 + 표식: %q %v", slot, err)
	}
	if slot, err := probeSlot(down.URL); slot != "blue" || err == nil {
		t.Fatalf("502여도 표식은 돌려주고 오류도 알려야 한다: %q %v", slot, err)
	}
	if slot, err := probeSlot(unmarked.URL); slot != "" || err != nil {
		t.Fatalf("표식 없음: %q %v", slot, err)
	}
	if _, err := probeSlot("http://127.0.0.1:1/"); err == nil {
		t.Fatal("응답 없음은 오류여야 한다")
	}
}

func TestSwitchIsRefusedWhenLiveProxyConfigCannotBeRead(t *testing.T) {
	// repo에는 새 본 설정이 있는데 운영 사본이 없다(읽을 수 없다): 되돌릴 내용을 모르므로 바꾸지 않는다
	agent, world := slotNewAgent(t, slotSetup{activeContent: slotBlueTemplate, servingSlot: "blue"})
	directory := agent.directories["llm"]
	if err := os.WriteFile(filepath.Join(directory, "deploy", "nginx", "nginx.conf"), []byte("# 새 커밋의 설정\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	slotRequireFailure(t, slotPost(agent, supplementCommit), "cd_switch")
	slotWaitBackground(t, agent)
	if _, err := os.Stat(filepath.Join(directory, "deploy", "active", "nginx.conf")); err == nil {
		t.Fatal("되돌릴 수 없는 상태에서 운영 사본을 새로 썼다")
	}
	slotRequireActiveContent(t, world, slotBlueTemplate, "거절 뒤")
	if slotCount(world.snapshot(), "run", "nginx -s reload") != 0 {
		t.Fatal("거절했는데 reload를 실행했다")
	}
}

func TestDrainWaitsForReloadToSettleBeforeJudging(t *testing.T) {
	// reload 직후의 틈: nginx가 아직 구 worker를 종료 중으로 표시하지 않았고, 구 worker가 한 요청을 더 받는다.
	// 그 순간만 보고 "남은 연결 없음 + 아직 구 자리가 응답"으로 단정하면 구 자리가 영영 남는다(서버 실측 2026-11-20).
	agent, world := slotNewAgent(t, slotSetup{activeContent: slotBlueTemplate, servingSlot: "blue"})
	agent.drainSettle = 30 * time.Millisecond
	probes := 0
	var mutex sync.Mutex
	settled := time.Now().Add(20 * time.Millisecond)
	agent.probeSlot = func(healthURL string) (string, error) {
		mutex.Lock()
		probes++
		mutex.Unlock()
		if time.Now().Before(settled) {
			return "blue", nil // 틈 사이에는 구 worker가 받는다
		}
		return "green", nil
	}
	agent.startDrain("llm", "blue", "")
	slotWaitBackground(t, agent)
	if !supplementStopped(world, "blue") {
		t.Fatalf("reload가 자리 잡은 뒤에는 구 자리를 내려야 한다:\n%s", slotDescribe(world.snapshot()))
	}
}

func TestDrainRechecksBeforeHoldingTheSlot(t *testing.T) {
	// 첫 확인에서 구 자리가 응답했더라도 곧 새 자리로 바뀌면 내린다
	agent, world := slotNewAgent(t, slotSetup{activeContent: slotBlueTemplate, servingSlot: "blue"})
	calls := 0
	var mutex sync.Mutex
	agent.probeSlot = func(healthURL string) (string, error) {
		mutex.Lock()
		defer mutex.Unlock()
		calls++
		if calls <= 2 {
			return "blue", nil
		}
		return "green", nil
	}
	agent.startDrain("llm", "blue", "")
	slotWaitBackground(t, agent)
	if !supplementStopped(world, "blue") {
		t.Fatalf("다시 확인해 새 자리가 응답하면 구 자리를 내려야 한다:\n%s", slotDescribe(world.snapshot()))
	}
}
