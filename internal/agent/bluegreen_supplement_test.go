package agent

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
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
