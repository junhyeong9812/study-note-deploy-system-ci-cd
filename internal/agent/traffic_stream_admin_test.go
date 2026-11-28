package agent

// 어드민의 통신 지도 SSE 를 stream 갈래로 센다(⑬ T8b · 통신 규약 3.6 의 표 "admin — 지도의 SSE" · 4.3 · 7.1 의 T8 줄).
//   - service = admin · method = GET · path 가 정확히 /api/traffic/stream 인 줄 = stream. 호출자 · 상태 · 걸린 시간과 상관없다
//     (3.6: stream 은 호출자와 상관없이 메서드 · 경로로 가른다 · 상태로 가르지 않는다).
//   - 그 밖(POST · 끝의 / · /api/traffic/streams · /api/traffic · ; 가 붙은 경로 · 잘린 경로 · 다른 서비스의 같은 경로)은 request.
//   - 판정 차례 health → stream → request 그대로 · ci-agent 의 두 스트림 경로 그대로.
//   - 수(4.3): stream 묶음은 count · status 에는 들고, buckets 는 모두 0 · max_ms 는 null · 요청별 줄(requests)에는 없다 —
//     68분 열린 SSE 하나가 어드민으로 들어오는 request 묶음(분포 · max_ms · count)을 바꾸지 않는다.
// 줄은 어드민 nginx 의 log_format(14칸 · service "admin")과 같은 꼴(defaultAccess 의 text).

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func adminAccess(change func(line *accessLine)) accessLine {
	return defaultAccess().with(func(line *accessLine) {
		line.service, line.caller, line.method, line.path = "admin", "outside", "GET", "/api/traffic/stream"
		line.upstreamTime, line.upstreamStatus = "", "" // 지도는 SSE 를 자리(upstream)로 넘기지만 값은 줄마다 다르다 — 갈래와 무관
	}).with(change)
}

func TestAdminTrafficStreamIsClassifiedAsStream(t *testing.T) {
	defer failOnPanic(t)
	type expectation struct {
		label string
		line  accessLine
		class string
	}
	cases := []expectation{
		{"SSE 200 · 3초", adminAccess(func(line *accessLine) { line.requestTime = "3.000" }), "stream"},
		{"SSE 200 · 68분", adminAccess(func(line *accessLine) { line.requestTime = "4080.000" }), "stream"},
		{"SSE 499(브라우저가 닫음)", adminAccess(func(line *accessLine) { line.status, line.requestTime = "499", "612.345" }), "stream"},
		{"SSE 504", adminAccess(func(line *accessLine) { line.status, line.requestTime = "504", "30.001" }), "stream"},
		{"SSE 호출자 edge", adminAccess(func(line *accessLine) { line.caller = "edge" }), "stream"},
		{"SSE 호출자 admin", adminAccess(func(line *accessLine) { line.caller = "admin" }), "stream"},
		{"SSE 호출자 alert", adminAccess(func(line *accessLine) { line.caller = "alert" }), "stream"},
		{"POST 같은 경로", adminAccess(func(line *accessLine) { line.method = "POST" }), "request"},
		{"HEAD 같은 경로", adminAccess(func(line *accessLine) { line.method = "HEAD" }), "request"},
		{"끝에 /", adminAccess(func(line *accessLine) { line.path = "/api/traffic/stream/" }), "request"},
		{"접두사가 같은 다른 경로", adminAccess(func(line *accessLine) { line.path = "/api/traffic/streams" }), "request"},
		{"위 경로", adminAccess(func(line *accessLine) { line.path = "/api/traffic" }), "request"},
		{"; 가 붙은 경로", adminAccess(func(line *accessLine) { line.path = "/api/traffic/stream;x" }), "request"},
		{"아래 경로", adminAccess(func(line *accessLine) { line.path = "/api/traffic/stream/more" }), "request"},
		{"대문자 경로", adminAccess(func(line *accessLine) { line.path = "/API/traffic/stream" }), "request"},
		{"잘린 경로(같은 앞머리)", adminAccess(func(line *accessLine) {
			line.path, line.pathTruncated = "/api/traffic/stream"+strings.Repeat("x", 1024-len("/api/traffic/stream")), true
		}), "request"},
		{"front 의 같은 경로", adminAccess(func(line *accessLine) { line.service = "front" }), "request"},
		{"observe 의 같은 경로", adminAccess(func(line *accessLine) { line.service = "observe" }), "request"},
		{"backend 의 같은 경로", adminAccess(func(line *accessLine) { line.service = "backend" }), "request"},
		{"어드민 헬스(감시)", adminAccess(func(line *accessLine) { line.caller, line.path = "alert", "/api/health" }), "health"},
		{"어드민 헬스(감시가 아님)", adminAccess(func(line *accessLine) { line.caller, line.path = "front", "/api/health" }), "request"},
		{"에이전트 배포 ci", adminAccess(func(line *accessLine) { line.service, line.method, line.path = "ci-agent", "POST", "/agent/ci" }), "stream"},
		{"에이전트 배포 cd", adminAccess(func(line *accessLine) { line.service, line.method, line.path = "ci-agent", "POST", "/agent/cd" }), "stream"},
	}
	// 줄 하나씩 — 읽는 쪽의 함수로 판정
	for _, testCase := range cases {
		kind, record := classifyTrafficLine([]byte(testCase.line.text()))
		if kind != trafficUnitLine || record == nil {
			t.Fatalf("%s: 줄이 읽히지 않았다(kind %d): %s", testCase.label, kind, testCase.line.text())
		}
		if record.class != testCase.class {
			t.Errorf("%s: 갈래 %q (기대 %q)", testCase.label, record.class, testCase.class)
		}
	}
	// 항목까지 — 줄마다 다른 30초 칸이라 묶음 하나 = 줄 하나
	lines := make([]string, len(cases))
	for index := range cases {
		lines[index] = cases[index].line.with(func(line *accessLine) {
			line.requestID = fmt.Sprintf("req-admin-stream-%02d", index)
			line.ts = fmt.Sprintf("%d.000", 1764201600+int64(index)*30)
		}).text()
	}
	body, _, _ := cycleWith(t, lines...)
	if len(body.Segments) != len(cases) {
		t.Fatalf("묶음 %d (기대 %d)", len(body.Segments), len(cases))
	}
	zero := fmt.Sprint(make([]int64, 20))
	for index, testCase := range cases {
		segment := body.Segments[index]
		if segment.Class != testCase.class || segment.Count != 1 {
			t.Errorf("%s: 묶음의 갈래 %q · 수 %d (기대 %q · 1)", testCase.label, segment.Class, segment.Count, testCase.class)
		}
		if testCase.class == "stream" && (segment.MaxMs != nil || fmt.Sprint(segment.Buckets) != zero) {
			t.Errorf("%s: stream 묶음은 분포 0 · max_ms null: %v %v", testCase.label, segment.Buckets, segment.MaxMs)
		}
	}
	for _, request := range body.Requests {
		if request.Service == "admin" && request.Method == "GET" && request.Path == "/api/traffic/stream" {
			t.Fatalf("SSE 가 요청별 줄에 실렸다: %+v", request)
		}
	}
}

// 68분 열린 SSE 하나가 같은 30초 칸 · 같은 호출자의 보통 요청 묶음을 바꾸지 않는다 — SSE 가 없을 때의 항목과 request 묶음이 같다.
func TestLongAdminStreamDoesNotChangeRequestNumbers(t *testing.T) {
	defer failOnPanic(t)
	ordinary := []string{}
	for index, duration := range []string{"0.041", "0.120", "1.500"} {
		ordinary = append(ordinary, adminAccess(func(line *accessLine) {
			line.path, line.requestTime, line.requestID = "/api/traffic", duration, fmt.Sprintf("req-admin-%d", index)
			line.ts = fmt.Sprintf("1764201601.%03d", index)
		}).text())
	}
	sse := adminAccess(func(line *accessLine) {
		line.requestTime, line.requestID, line.ts = "4080.000", "req-admin-sse", "1764201601.500"
	}).text()

	without, _, _ := cycleWith(t, ordinary...)
	with, _, _ := cycleWith(t, append(append([]string{}, ordinary...), sse)...)

	requestSegment := func(body trafficBody) trafficSegmentBody {
		t.Helper()
		for _, segment := range body.Segments {
			if segment.Service == "admin" && segment.Class == "request" {
				return segment
			}
		}
		t.Fatalf("admin request 묶음이 없다: %+v", body.Segments)
		return trafficSegmentBody{}
	}
	before, after := requestSegment(without), requestSegment(with)
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("SSE 하나가 request 묶음을 바꿨다:\n전 %+v\n후 %+v", before, after)
	}
	if after.Count != 3 || after.MaxMs == nil || *after.MaxMs != 1500 {
		t.Fatalf("request 묶음은 보통 요청 셋 · 가장 긴 응답 1500ms: %+v", after)
	}
	if len(with.Segments) != 2 {
		t.Fatalf("SSE 는 따로 stream 묶음 하나: %+v", with.Segments)
	}
	var stream trafficSegmentBody
	for _, segment := range with.Segments {
		if segment.Class == "stream" {
			stream = segment
		}
	}
	if stream.Service != "admin" || stream.Count != 1 || stream.Status["2xx"] != 1 || stream.MaxMs != nil || fmt.Sprint(stream.Buckets) != fmt.Sprint(make([]int64, 20)) {
		t.Fatalf("SSE 의 stream 묶음: 수 1 · 2xx 1 · 분포 0 · max_ms null: %+v", stream)
	}
	if !reflect.DeepEqual(without.Requests, with.Requests) {
		t.Fatalf("SSE 가 요청별 줄을 바꿨다:\n전 %+v\n후 %+v", without.Requests, with.Requests)
	}
}

// 판정 차례 health → stream: 한 경로가 두 표에 함께 오르면 health 가 앞선다(규약 3.6). 표를 잠시 넓혀 본다.
func TestHealthPrecedesStreamForTheSameRoute(t *testing.T) {
	defer failOnPanic(t)
	original := trafficStreamRoutes["admin"]
	trafficStreamRoutes["admin"] = append(append([]string{}, original...), "GET /api/health")
	defer func() { trafficStreamRoutes["admin"] = original }()
	health := adminAccess(func(line *accessLine) { line.caller, line.path = "alert", "/api/health" }).text()
	if _, record := classifyTrafficLine([]byte(health)); record == nil || record.class != "health" {
		t.Fatalf("감시가 부른 어드민 헬스는 stream 표에 함께 있어도 health: %+v", record)
	}
	visit := adminAccess(func(line *accessLine) { line.caller, line.path = "front", "/api/health" }).text()
	if _, record := classifyTrafficLine([]byte(visit)); record == nil || record.class != "stream" {
		t.Fatalf("헬스의 호출자가 아니면 그 다음 차례(stream): %+v", record)
	}
}
