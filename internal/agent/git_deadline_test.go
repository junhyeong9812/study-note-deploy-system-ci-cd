package agent

// 기한의 합 — 기준: requirement-spec.md §12.9. agent 의 조회 기한(3초) + 자식 프로세스 정리 대기 < master 의 중계 기한(4초).
// 그래야 git_timeout(agent) 과 agent_timeout(master) 이 섞이지 않는다. master 쪽 4초는 master 패키지에서 따로 고정한다.

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestGitReadDefaultTimeoutIsThreeSeconds(t *testing.T) {
	if agent := newGitReadAgent(t, nil); agent.gitTimeout != 3*time.Second {
		t.Fatalf("agent 조회 기한 기본값은 3초여야 한다: %v", agent.gitTimeout)
	}
}

// 기본 기한 그대로, 파이프를 물려받은 손자 프로세스가 남는 가장 나쁜 경우에도 504 git_timeout 이 4초(master 중계 기한) 안에 온다.
// 정리 대기(Wait 가 남은 파이프를 기다리는 시간)가 기한에 더해지는 몫을 실제로 잰다.
func TestGitReadTimesOutBeforeMasterRelayDeadlineEvenWithLingeringGrandchild(t *testing.T) {
	repository, shas := linearRepository(t, 2)
	agent := newGitReadAgent(t, map[string]string{"llm": repository.directory})
	sleepProgram := absoluteProgram(t, "sleep")
	pidFile := filepath.Join(t.TempDir(), "pids")
	// 손자(sleep 8 &)가 stdout 파이프를 쥔 채 남는다 — 자식만 죽여서는 파이프가 닫히지 않는다
	script := "#!/bin/sh\n'" + sleepProgram + "' 8 &\necho $! >> '" + pidFile + "'\necho $$ >> '" + pidFile + "'\nexec '" + sleepProgram + "' 8\n"
	wrapper := filepath.Join(t.TempDir(), "git")
	if err := os.WriteFile(wrapper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	agent.gitProgram = wrapper
	t.Cleanup(func() {
		for _, pid := range readPids(pidFile) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})
	startedAt := time.Now()
	recorder := gitGet(agent, commitTarget("llm", shas[1]))
	elapsed := time.Since(startedAt)
	requireGitFailure(t, recorder, 504, "git_timeout")
	if elapsed >= 4*time.Second {
		t.Fatalf("agent 기한(3초) + 정리 대기가 master 중계 기한(4초)을 넘었다: %v (pids %v)", elapsed, readPids(pidFile))
	}
	if elapsed < 3*time.Second-100*time.Millisecond {
		t.Fatalf("기본 기한(3초)보다 일찍 끊었다: %v", elapsed)
	}
}
