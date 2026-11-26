package agent

// 특성 테스트 — 배포 실패의 전체 출력을 더하기 전, 단계 실패 봉투의 code · detail 과 성공 봉투의 칸을 그대로 적었다.
// 기준: requirement-spec.md(2026-11-25 admin-incident-assist) §15 "기존 동작은 그대로" — detail 은 실패 글의 끝 2000바이트(가리지 않은 원래 글).

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"testing"
)

// scriptedCommands — 명령의 조각마다 돌려줄 출력과 실패 여부를 정해 둔다. 정하지 않은 명령은 빈 출력으로 성공한다.
type scriptedCommands struct {
	outputs  map[string]string // 명령 조각 → 출력
	failures map[string]bool   // 명령 조각 → 실패(exit status 1)
}

func (scripted *scriptedCommands) run(_ context.Context, _ string, _ []string, arguments ...string) (string, error) {
	line := strings.Join(arguments, " ")
	if strings.Contains(line, "rev-parse") {
		return headSha + "\n", nil
	}
	for fragment, output := range scripted.outputs {
		if strings.Contains(line, fragment) {
			if scripted.failures[fragment] {
				return output, errors.New("exit status 1")
			}
			return output, nil
		}
	}
	return "", nil
}

func newScriptedAgent(t *testing.T, scripted *scriptedCommands) *Agent {
	agent := newPipelineAgent(t, &recordedCommands{})
	agent.run = scripted.run
	return agent
}

type stageEnvelope struct {
	Success bool `json:"success"`
	Error   struct {
		Code   string          `json:"code"`
		Detail string          `json:"detail"`
		Output json.RawMessage `json:"output"`
	} `json:"error"`
	Data map[string]json.RawMessage `json:"data"`
}

func decodeStageEnvelope(t *testing.T, body []byte) stageEnvelope {
	t.Helper()
	var envelope stageEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("봉투가 아니다: %v %s", err, body)
	}
	return envelope
}

func TestStageFailureDetailIsLast2000BytesOfFailureText(t *testing.T) {
	testOutput := strings.Repeat("0123456789", 300) + "FAILED test_order" // 3017바이트
	agent := newScriptedAgent(t, &scriptedCommands{
		outputs:  map[string]string{"--target test": testOutput},
		failures: map[string]bool{"--target test": true},
	})
	recorder := post(agent, "/agent/ci")
	envelope := decodeStageEnvelope(t, recorder.Body.Bytes())
	failureText := testOutput + "\n" + "exit status 1" // 앞선 단계(받기 · 맞추기)의 출력은 비었다
	if recorder.Code != 500 || envelope.Success || envelope.Error.Code != "ci_test" || envelope.Error.Detail != failureText[len(failureText)-2000:] {
		t.Fatalf("detail 은 실패 글의 끝 2000바이트여야 한다: %d code=%q detail 길이 %d", recorder.Code, envelope.Error.Code, len(envelope.Error.Detail))
	}
}

func TestStageFailureDetailKeepsShortFailureTextWhole(t *testing.T) {
	agent := newScriptedAgent(t, &scriptedCommands{
		outputs:  map[string]string{"compose up": "Error response from daemon: port is already allocated"},
		failures: map[string]bool{"compose up": true},
	})
	envelope := decodeStageEnvelope(t, post(agent, "/agent/cd").Body.Bytes())
	if envelope.Error.Code != "cd_up" || envelope.Error.Detail != "Error response from daemon: port is already allocated\nexit status 1" {
		t.Fatalf("짧은 실패 글은 그대로 detail 이어야 한다: %+v", envelope.Error)
	}
}

func TestStageSuccessEnvelopeFieldsStayTheSame(t *testing.T) {
	agent := newScriptedAgent(t, &scriptedCommands{outputs: map[string]string{"compose build": "built ok"}})
	recorder := post(agent, "/agent/ci")
	envelope := decodeStageEnvelope(t, recorder.Body.Bytes())
	keys := make([]string, 0, len(envelope.Data))
	for key := range envelope.Data {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	if recorder.Code != 200 || strings.Join(keys, ",") != "output,took_ms" || string(envelope.Data["output"]) != `"built ok"` {
		t.Fatalf("성공 봉투의 칸이 바뀌었다: %d %s", recorder.Code, recorder.Body.String())
	}
}
