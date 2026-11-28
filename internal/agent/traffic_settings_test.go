package agent

// A. 설정 · 시작 — 기준: requirement-spec.md(traffic-map) "T5 착수 — 설정 · 저장" · "선검증 반영" 5.
//
// 정한 계약:
//   - TRAFFIC_CONTAINERS: 쉼표로 나눈 이름(앞뒤 공백 제거 · 빈 칸 버림, 받은 차례 그대로). 1~16개 · 이름은 ^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$ · 겹침 없음.
//   - SERVER_NAME: 꼴 ^[a-z0-9][a-z0-9._-]{0,62}$ — 없거나 틀리면 보내지 않는다(자원 지표의 기본 이름 ci-<MODE> 를 쓰지 않는다).
//   - LOG_REDIS_URL: 비었거나 해석할 수 없으면 시작하지 않는다. TRAFFIC_STREAM 기본 traffic · TRAFFIC_CURSOR_PATH 기본 /data/traffic-cursor.json.
//   - 꺼진 까닭의 낱말(DisabledReason · 헬스의 disabled_reason): no_containers · too_many_containers · invalid_container_name ·
//     duplicate_container · no_server_name · invalid_server_name · no_redis.
//   - StartTraffic: 꺼졌으면 주기를 돌리지 않고 로그 한 줄 `traffic collection disabled: <까닭>`(info) — 그래도 nil 이 아닌 수집기(헬스가 까닭을 낸다).
//     켜졌으면 로그 한 줄 `traffic collection every 30s from <n> containers to stream <stream>`(info).
//   - 자원 지표 수집은 통신 설정이 어떻든 그대로 시작한다.

import (
	"context"
	"strings"
	"testing"
)

func setTrafficEnvironment(t *testing.T, values map[string]string) {
	t.Helper()
	defaults := map[string]string{"LOG_REDIS_URL": "redis://192.0.2.10:6379", "SERVER_NAME": trafficServer,
		"TRAFFIC_CONTAINERS": "backend-nginx", "TRAFFIC_STREAM": "", "TRAFFIC_CURSOR_PATH": ""}
	for name, value := range defaults {
		if override, present := values[name]; present {
			value = override
		}
		t.Setenv(name, value)
	}
}

func TestTrafficSettingsAreReadFromEnvironment(t *testing.T) {
	defer failOnPanic(t)
	setTrafficEnvironment(t, map[string]string{"TRAFFIC_CONTAINERS": " front-nginx, backend-nginx ,,"})
	settings := LoadTrafficSettings()
	if settings.DisabledReason != "" || strings.Join(settings.Containers, ",") != "front-nginx,backend-nginx" || settings.Stream != "traffic" ||
		settings.CursorPath != "/data/traffic-cursor.json" || settings.Server != trafficServer || settings.RedisAddress != "192.0.2.10:6379" {
		t.Fatalf("설정이 다르다: %+v", settings)
	}
	setTrafficEnvironment(t, map[string]string{"TRAFFIC_STREAM": "traffic-test", "TRAFFIC_CURSOR_PATH": "/tmp/cursor.json"})
	if settings := LoadTrafficSettings(); settings.Stream != "traffic-test" || settings.CursorPath != "/tmp/cursor.json" {
		t.Fatalf("TRAFFIC_STREAM · TRAFFIC_CURSOR_PATH 를 읽지 않았다: %+v", settings)
	}
}

func TestTrafficSettingsRejectBadValues(t *testing.T) {
	defer failOnPanic(t)
	names := func(count, length int) string {
		parts := make([]string, count)
		for index := range parts {
			parts[index] = strings.Repeat(string(rune('a'+index%26)), length-2) + string(rune('a'+index/26)) + "x"
		}
		return strings.Join(parts, ",")
	}
	cases := []struct {
		name   string
		values map[string]string
		reason string
	}{
		{"빈 목록", map[string]string{"TRAFFIC_CONTAINERS": ""}, "no_containers"},
		{"쉼표만", map[string]string{"TRAFFIC_CONTAINERS": " , ,"}, "no_containers"},
		{"17개", map[string]string{"TRAFFIC_CONTAINERS": names(17, 8)}, "too_many_containers"},
		{"16개 · 128자는 받는다", map[string]string{"TRAFFIC_CONTAINERS": names(16, 128)}, ""},
		{"129자", map[string]string{"TRAFFIC_CONTAINERS": names(1, 129)}, "invalid_container_name"},
		{"앞이 -", map[string]string{"TRAFFIC_CONTAINERS": "-nginx"}, "invalid_container_name"},
		{"빈칸", map[string]string{"TRAFFIC_CONTAINERS": "backend nginx"}, "invalid_container_name"},
		{"/", map[string]string{"TRAFFIC_CONTAINERS": "x/nginx"}, "invalid_container_name"},
		{"겹침", map[string]string{"TRAFFIC_CONTAINERS": "backend-nginx,front-nginx,backend-nginx"}, "duplicate_container"},
		{"서버 이름 없음", map[string]string{"SERVER_NAME": ""}, "no_server_name"},
		{"서버 이름 대문자", map[string]string{"SERVER_NAME": "Ci-agent"}, "invalid_server_name"},
		{"서버 이름 앞이 -", map[string]string{"SERVER_NAME": "-agent"}, "invalid_server_name"},
		{"서버 이름 64자", map[string]string{"SERVER_NAME": "a" + strings.Repeat("b", 63)}, "invalid_server_name"},
		{"서버 이름 63자는 받는다", map[string]string{"SERVER_NAME": "a" + strings.Repeat("b", 62)}, ""},
		{"Redis 없음", map[string]string{"LOG_REDIS_URL": ""}, "no_redis"},
		{"Redis 해석 불가", map[string]string{"LOG_REDIS_URL": "::not a url"}, "no_redis"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			defer failOnPanic(t)
			setTrafficEnvironment(t, testCase.values)
			if settings := LoadTrafficSettings(); settings.DisabledReason != testCase.reason {
				t.Fatalf("까닭 %q, 기대 %q (%+v)", settings.DisabledReason, testCase.reason, settings)
			}
		})
	}
}

func TestDisabledTrafficLogsOnceAndResourceMetricsStillStart(t *testing.T) {
	defer failOnPanic(t)
	for _, values := range []map[string]string{{"SERVER_NAME": ""}, {"TRAFFIC_CONTAINERS": ""}, {"SERVER_NAME": "BAD"}} {
		setTrafficEnvironment(t, values)
		t.Setenv("LOG_REDIS_URL", "redis://"+closedAddress(t))
		agent := newPipelineAgent(t, &recordedCommands{})
		ctx, cancel := context.WithCancel(context.Background())
		var collector *TrafficCollector
		var resource *ResourceCollector
		logged := captureStandardOutput(t, func() {
			collector = agent.StartTraffic(ctx)
			resource = agent.StartResourceMetrics(ctx)
		})
		cancel()
		reason := LoadTrafficSettings().DisabledReason
		if collector == nil || collector.Health().Enabled || collector.Health().DisabledReason == nil || *collector.Health().DisabledReason != reason {
			t.Fatalf("%v: 꺼진 수집기는 헬스에 까닭을 낸다", values)
		}
		if strings.Count(logged, "traffic collection disabled: "+reason) != 1 || strings.Count(logged, "traffic collection") != 1 {
			t.Fatalf("%v: 꺼짐의 로그는 한 줄:\n%s", values, logged)
		}
		if resource == nil {
			t.Fatalf("%v: 통신 설정이 틀려도 자원 지표 수집은 시작한다", values)
		}
	}
}

func TestEnabledTrafficLogsStartLine(t *testing.T) {
	defer failOnPanic(t)
	setTrafficEnvironment(t, map[string]string{"TRAFFIC_CONTAINERS": "backend-nginx,front-nginx", "TRAFFIC_CURSOR_PATH": t.TempDir() + "/cursor.json"})
	t.Setenv("LOG_REDIS_URL", "redis://"+closedAddress(t))
	agent := newPipelineAgent(t, &recordedCommands{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var collector *TrafficCollector
	logged := captureStandardOutput(t, func() { collector = agent.StartTraffic(ctx) })
	health := collector.Health()
	if !health.Enabled || health.DisabledReason != nil || len(health.Containers) != 2 {
		t.Fatalf("켜진 수집기의 헬스: %+v", health)
	}
	if !strings.Contains(logged, "traffic collection every 30s from 2 containers to stream traffic") {
		t.Fatalf("시작 로그가 다르다:\n%s", logged)
	}
}
