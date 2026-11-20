package agent

import (
	"context"
	"errors"
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
