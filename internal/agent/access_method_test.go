package agent

// 꼴 밖의 메서드로 쓴 접속 줄도 읽는 쪽(classifyTrafficLine)이 읽는다 — method 가 "" 인 줄은 request 갈래(규약 3.6 · 4.3 의 표).

import (
	"strings"
	"testing"
)

func TestAccessLineWithMalformedMethodIsReadAsRequest(t *testing.T) {
	agent := newPipelineAgent(t, &recordedCommands{})
	for _, method := range []string{"get", "M3TH0D", strings.Repeat("M", 36)} {
		for _, target := range []string{"/health", "/agent/ci"} {
			recorder, text := servedAgentLine(t, agent, method, target, "", "127.0.0.1:40000", map[string]string{"X-Caller-Service": "alert"})
			kind, record := classifyTrafficLine([]byte(text))
			if kind != trafficUnitLine || record == nil {
				t.Fatalf("%s %s: 읽는 쪽이 줄을 버렸다(kind %d): %s", method, target, kind, text)
			}
			if record.method != "" || record.status != int64(recorder.Code) || record.path != target || record.class != "request" {
				t.Fatalf("%s %s: method \"\" · 상태 그대로 · request 갈래여야 한다: %+v", method, target, *record)
			}
		}
	}
}
