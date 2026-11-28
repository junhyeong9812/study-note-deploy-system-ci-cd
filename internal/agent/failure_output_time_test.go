package agent

// 가림의 시간 상한이 agent 의 단계 잠금을 붙잡지 않는다 — 기준: requirement-spec.md(2026-11-25 admin-incident-assist) §17 의 1.
// 정한 계약: 최악 입력 1MB 의 실패 출력도 응답과 잠금 풀기가 10초 안이다(-race 에서는 4배 — 가림이 입력에 비례하는 시간).

import (
	"strings"
	"testing"
	"time"
)

func TestWorstCaseFailureOutputDoesNotHoldTheStageLock(t *testing.T) {
	agent := newScriptedAgent(t, &scriptedCommands{
		outputs:  map[string]string{"--target test": strings.Repeat("password", (1<<20)/8)},
		failures: map[string]bool{"--target test": true},
	})
	done := make(chan int, 1)
	started := time.Now()
	go func() { done <- postAsking(agent, "/agent/ci", "v1").Code }()
	select {
	case code := <-done:
		if code != 500 {
			t.Fatalf("테스트 실패는 500: %d", code)
		}
	case <-time.After(10 * time.Second * timeBudgetFactor):
		t.Fatal("최악 입력 1MB 의 실패 봉투를 10초 안에 만들지 못했다 — 그동안 단계 잠금을 쥐고 있다")
	}
	if !agent.busy.TryLock() {
		t.Fatal("응답 뒤에도 단계 잠금이 풀리지 않았다")
	}
	agent.busy.Unlock()
	t.Logf("최악 입력 1MB: %v", time.Since(started))
}
