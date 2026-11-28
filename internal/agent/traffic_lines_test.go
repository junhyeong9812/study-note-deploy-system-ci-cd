package agent

// C. 줄 → 수 — 기준: 규약 §3.1 · §3.2 · §3.6 · §4.2 · §4.3, requirement-spec.md(traffic-map) "T5 착수 — 줄 → 수" · "선검증 반영" 8,
// 검사 도구 tools/traffic-access-log-check/access_log_check.py(check_line 의 칸 · 값 규칙 · classify · SERVICE_ROUTES),
// 관측 서버의 해석 TrafficEntry.kt(항목 전체를 버리는 값 · 그 줄만 버리는 경로).
//
// 정한 계약:
//   - `{"v":` 로 시작하지 않는 줄 = ignored. 시작하는데 아래에 걸리면 unparsed(묶음 · 요청별 줄 어디에도 싣지 않는다):
//     JSON 객체가 아님(뒤에 더 붙은 글자 포함) · 겹친 칸 · 14칸(nginx) 또는 loopback(참 · 거짓)을 더한 15칸(ci-cd 앱 — 규약 3.4 · 7.2)이 아님(모르는 칸 · 빠진 칸 · 참 · 거짓이 아닌 loopback) ·
//     v 가 정수 글자 1 이 아님 · ts 가 `^[0-9]+\.[0-9]{3}$` 의 수가 아니거나 [10억, 40억) 밖 · request_id 가 ^[A-Za-z0-9_-]{1,64}$ 밖 ·
//     caller 가 front · backend · llm-wrapper · alert · observe · admin · ci-master · ci-agent · edge · outside 밖 ·
//     service 가 front … ci-agent · edge 밖 · method 가 글이 아니거나 ^[A-Z_-]{0,32}$ 밖(관측 서버) · path 가 글이 아니거나 1024자를 넘거나
//     제어 문자(U+0000–U+001F · U+007F — 관측 서버)가 있음 · path_truncated 가 참 · 거짓이 아니거나 참인데 path 가 1024바이트보다 짧음 ·
//     status 가 정수 글자 100~599 밖 · request_time 이 `^[0-9]+\.[0-9]{3}$` 밖 · upstream_time · upstream_status 가 검사 도구의 꼴 밖 ·
//     upstream_status 가 128자를 넘음(관측 서버) · slot 이 blue · green · "" 밖 · bytes_sent 가 0 이상 정수 글자가 아님.
//     (검사 도구의 "줄에 ? · 주소 꼴이 있다"는 unparsed 가 아니다 — 경로 필터로 요청별 줄에서만 뺀다.)
//   - 갈래: health(caller 가 alert · ci-agent · method 가 GET · HEAD · path 가 그 서비스의 헬스 경로와 정확히 같음) → stream(그 서비스의 (method, path)) → request.
//     edge 처럼 경로표에 없는 서비스는 request.
//   - 값: end_ms · duration_ms · upstream_ms 는 글자에서 소수점을 뺀 정수(부동소수 곱셈 금지) · upstream_ms 는 `,` · `:` 로 나눈 조각의 합(`-` · "" 조각은 건너뜀,
//     없으면 null) · upstream_status · slot 은 "" · "-" 면 null.
//   - 분포 20칸(위쪽 경계 이하): 5 10 25 50 100 250 500 1000 2500 5000 10000 15000 20000 30000 45000 60000 90000 120000 180000, 20번째는 초과.
//   - 요청별 줄: request 이고 method · path 가 "" 가 아니고 경로 필터(? · %3F · %3f · %3A %2E %5B %5D 를 푼 뒤 IPv4 · IPv6 꼴)를 지난 줄.
//     걸러진 줄은 헬스의 requests_filtered. (end_ms, request_id, service, method, path) 오름차순 앞에서부터 100줄 · body 32,768바이트 이하가
//     되도록 담고(담지 못하는 첫 줄에서 멈춘다) 나머지는 requests_dropped.
//   - 직렬화: 빈칸 없는 JSON · HTML 이스케이프 없음 · 칸 생략 없음(null · 빈 배열) · 칸의 차례는 규약 §4.2 의 예시대로.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"
)

// cycleWith — 첫 주기 뒤 lines 를 찍고 한 주기를 돈 항목의 body.
func cycleWith(t *testing.T, lines ...string) (trafficBody, trafficItem, *trafficHarness) {
	t.Helper()
	harness := startedHarness(t, nginx)
	for index, line := range lines {
		harness.docker.writeRaw(nginx, trafficWallStart.Add(5*time.Second+time.Duration(index)*time.Millisecond).UnixNano(), line+"\n")
	}
	harness.advance(30 * time.Second)
	harness.cycle()
	items := harness.stream.items()
	if len(items) < 2 {
		t.Fatalf("항목이 없다: %d", len(items))
	}
	requireObserveAccepts(t, items[1])
	return decodeTrafficBody(t, items[1]), items[1], harness
}

func TestUnparsedAndIgnoredLines(t *testing.T) {
	defer failOnPanic(t)
	valid := defaultAccess()
	replace := func(old, new string) string { return strings.Replace(valid.text(), old, new, 1) }
	unparsed := map[string]string{
		"깨진 JSON":              `{"v":1,"ts":`,
		"뒤에 더 붙은 글자":           valid.text() + ` x`,
		"빠진 칸":                 replace(`,"slot":"blue"`, ``),
		"모르는 칸":                replace(`}`, `,"extra":false}`),
		"loopback 이 참 · 거짓 아님": replace(`}`, `,"loopback":"true"}`),
		"loopback 이 수":         replace(`}`, `,"loopback":1}`),
		"loopback 과 모르는 칸":     replace(`}`, `,"loopback":false,"extra":1}`),
		"겹친 칸":                 replace(`"slot":"blue"`, `"slot":"blue","slot":"green"`),
		"v 2":                  replace(`{"v":1`, `{"v":2`),
		"v 1.0":                replace(`{"v":1`, `{"v":1.0`),
		"ts 소수 둘":              valid.with(func(line *accessLine) { line.ts = "1764201601.23" }).text(),
		"ts 글자":                valid.with(func(line *accessLine) { line.ts = `"1764201601.235"` }).text(),
		"ts 범위 밖":              valid.with(func(line *accessLine) { line.ts = "999999999.000" }).text(),
		"request_id 빈칸":        valid.with(func(line *accessLine) { line.requestID = "a b" }).text(),
		"request_id 65자":       valid.with(func(line *accessLine) { line.requestID = strings.Repeat("r", 65) }).text(),
		"caller 목록 밖":          valid.with(func(line *accessLine) { line.caller = "stranger" }).text(),
		"service 대문자":          valid.with(func(line *accessLine) { line.service = "Backend" }).text(),
		"method 소문자":           valid.with(func(line *accessLine) { line.method = "get" }).text(),
		"path 1025자":           valid.with(func(line *accessLine) { line.path = "/" + strings.Repeat("p", 1024) }).text(),
		"path 제어 문자":           valid.with(func(line *accessLine) { line.path = "/a\u0001b" }).text(),
		"path_truncated 인데 짧음": valid.with(func(line *accessLine) { line.pathTruncated = true }).text(),
		"path_truncated 글자":    replace(`"path_truncated":false`, `"path_truncated":"no"`),
		"status 99":            valid.with(func(line *accessLine) { line.status = "99" }).text(),
		"status 600":           valid.with(func(line *accessLine) { line.status = "600" }).text(),
		"status 200.0":         valid.with(func(line *accessLine) { line.status = "200.0" }).text(),
		"status 글자":            valid.with(func(line *accessLine) { line.status = `"200"` }).text(),
		"request_time 음수":      valid.with(func(line *accessLine) { line.requestTime = "-0.001" }).text(),
		"request_time 소수 둘":    valid.with(func(line *accessLine) { line.requestTime = "0.04" }).text(),
		"upstream_time 꼴":      valid.with(func(line *accessLine) { line.upstreamTime = "0.04" }).text(),
		"upstream_status 꼴":    valid.with(func(line *accessLine) { line.upstreamStatus = "2000" }).text(),
		"upstream_status 129자": valid.with(func(line *accessLine) { line.upstreamStatus = strings.TrimSuffix(strings.Repeat("200, ", 27), ", ") }).text(),
		"slot red":             valid.with(func(line *accessLine) { line.slot = "red" }).text(),
		"bytes_sent 음수":        valid.with(func(line *accessLine) { line.bytesSent = "-1" }).text(),
		"bytes_sent 소수":        valid.with(func(line *accessLine) { line.bytesSent = "1.5" }).text(),
		"칸 하나뿐":                `{"v":1}`,
	}
	lines := []string{"nginx: worker process started", valid.text(),
		valid.with(func(line *accessLine) { line.path = "/search?q=x"; line.requestID = "req-query" }).text()} // ? 는 unparsed 가 아니다
	names := make([]string, 0, len(unparsed))
	for name := range unparsed {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		lines = append(lines, unparsed[name])
	}
	body, _, _ := cycleWith(t, lines...)
	source := body.source(t, nginx)
	if source.Ignored != 1 || source.Lines != 2 || source.Unparsed != int64(len(unparsed)) {
		t.Fatalf("ignored 1 · lines 2 · unparsed %d: %+v", len(unparsed), source)
	}
	if len(body.Segments) != 1 || body.Segments[0].Count != 2 {
		t.Fatalf("unparsed 줄은 묶음에 넣지 않는다: %+v", body.Segments)
	}
}

func TestPathTruncatedWithLongPathIsValid(t *testing.T) {
	defer failOnPanic(t)
	line := defaultAccess().with(func(line *accessLine) { line.path = "/" + strings.Repeat("p", 1023); line.pathTruncated = true })
	body, _, _ := cycleWith(t, line.text())
	if source := body.source(t, nginx); source.Lines != 1 || len(body.Requests) != 1 || !body.Requests[0].PathTruncated {
		t.Fatalf("1024자 경로 · path_truncated 참은 맞는 줄: %+v %+v", source, body.Requests)
	}
}

func TestClassesFollowServiceRoutes(t *testing.T) {
	defer failOnPanic(t)
	type expectation struct {
		service, caller, method, path, class string
	}
	cases := []expectation{
		{"front", "alert", "GET", "/", "health"},
		{"front", "front", "GET", "/", "request"}, // 호출자가 감시자가 아니면 방문
		{"backend", "ci-agent", "HEAD", "/actuator/health", "health"},
		{"backend", "alert", "GET", "/api/health/dependencies", "health"},
		{"backend", "alert", "GET", "/actuator/health/", "request"}, // 끝의 / 는 헬스가 아니다
		{"backend", "alert", "POST", "/actuator/health", "request"},
		{"llm-wrapper", "alert", "GET", "/health", "health"},
		{"alert", "ci-agent", "GET", "/health", "health"},
		{"observe", "alert", "GET", "/health", "health"},
		{"admin", "alert", "GET", "/api/health", "health"},
		{"admin", "alert", "GET", "/health", "request"},
		{"ci-master", "alert", "GET", "/health", "health"},
		{"ci-agent", "alert", "GET", "/health", "health"},
		{"ci-agent", "ci-master", "POST", "/agent/ci", "stream"},
		{"ci-agent", "outside", "POST", "/agent/cd", "stream"},
		{"ci-agent", "ci-master", "GET", "/agent/ci", "request"},
		{"edge", "outside", "GET", "/", "request"}, // 경로표에 없는 서비스
	}
	lines := make([]string, len(cases))
	for index, testCase := range cases {
		lines[index] = defaultAccess().with(func(line *accessLine) {
			line.service, line.caller, line.method, line.path = testCase.service, testCase.caller, testCase.method, testCase.path
			line.requestID = fmt.Sprintf("req-class-%02d", index)
			line.ts = fmt.Sprintf("%d.000", 1764201600+int64(index)*30) // 줄마다 다른 칸 — 묶음 하나 = 줄 하나
		}).text()
	}
	body, _, harness := cycleWith(t, lines...)
	if len(body.Segments) != len(cases) {
		t.Fatalf("묶음 %d, 기대 %d", len(body.Segments), len(cases))
	}
	for index, testCase := range cases {
		segment := body.Segments[index]
		if segment.Class != testCase.class || segment.Service != testCase.service || segment.BucketStartMs != (1764201600+int64(index)*30)*1000 {
			t.Fatalf("%+v: 갈래 %s", testCase, segment.Class)
		}
		if testCase.class != "request" && (segment.MaxMs != nil || fmt.Sprint(segment.Buckets) != fmt.Sprint(make([]int64, 20))) {
			t.Fatalf("%+v: request 가 아닌 묶음은 분포 0 · max_ms null", testCase)
		}
	}
	requestCount := 0
	for _, testCase := range cases {
		if testCase.class == "request" {
			requestCount++
		}
	}
	if len(body.Requests) != requestCount {
		t.Fatalf("요청별 줄은 request 갈래만: %d, 기대 %d", len(body.Requests), requestCount)
	}
	_ = harness
}

func TestDistributionBoundariesAreInclusive(t *testing.T) {
	defer failOnPanic(t)
	durations := []string{"0.000", "0.005", "0.006", "0.010", "0.011", "1.000", "1.001", "180.000", "180.001", "999.999"}
	want := []int{0, 0, 1, 1, 2, 7, 8, 18, 19, 19}
	lines := make([]string, len(durations))
	for index, duration := range durations {
		lines[index] = defaultAccess().with(func(line *accessLine) {
			line.requestTime = duration
			line.requestID = fmt.Sprintf("req-dist-%02d", index)
		}).text()
	}
	body, _, _ := cycleWith(t, lines...)
	expected := make([]int64, 20)
	for _, index := range want {
		expected[index]++
	}
	if len(body.Segments) != 1 || fmt.Sprint(body.Segments[0].Buckets) != fmt.Sprint(expected) || *body.Segments[0].MaxMs != 999999 {
		t.Fatalf("분포의 경계는 이하: %v, 기대 %v", body.Segments[0].Buckets, expected)
	}
}

func TestStatusKeysAndSegmentKeys(t *testing.T) {
	defer failOnPanic(t)
	statuses := []string{"101", "200", "204", "301", "404", "499", "500", "503"}
	lines := make([]string, len(statuses))
	for index, status := range statuses {
		lines[index] = defaultAccess().with(func(line *accessLine) { line.status = status; line.requestID = fmt.Sprintf("req-status-%d", index) }).text()
	}
	lines = append(lines, defaultAccess().with(func(line *accessLine) { line.caller = "admin"; line.requestID = "req-other-caller" }).text())
	body, _, _ := cycleWith(t, lines...)
	if len(body.Segments) != 2 {
		t.Fatalf("열쇠(bucket_start_ms, service, caller, class)마다 묶음 하나: %+v", body.Segments)
	}
	if body.Segments[0].Caller != "admin" || body.Segments[1].Caller != "front" {
		t.Fatalf("묶음은 열쇠 순: %+v", body.Segments)
	}
	want := map[string]int64{"2xx": 2, "3xx": 1, "4xx": 2, "5xx": 2, "other": 1}
	if fmt.Sprint(body.Segments[1].Status) != fmt.Sprint(want) || body.Segments[1].Count != 8 {
		t.Fatalf("상태 다섯 열쇠: %v", body.Segments[1].Status)
	}
	if fmt.Sprint(body.Segments[0].Status) != fmt.Sprint(map[string]int64{"2xx": 1, "3xx": 0, "4xx": 0, "5xx": 0, "other": 0}) {
		t.Fatalf("0 인 열쇠도 낸다: %v", body.Segments[0].Status)
	}
}

func TestValuesAreComputedFromTextNotFloats(t *testing.T) {
	defer failOnPanic(t)
	lines := []string{
		defaultAccess().with(func(line *accessLine) {
			line.ts, line.requestTime, line.upstreamTime, line.requestID = "1764201601.235", "1.005", "1.003, 0.007 : 0.009", "req-values-1"
		}).text(),
		defaultAccess().with(func(line *accessLine) {
			line.ts, line.requestTime, line.upstreamTime, line.upstreamStatus, line.slot, line.requestID = "1764201601.236", "0.007", "", "", "", "req-values-2"
		}).text(),
		defaultAccess().with(func(line *accessLine) {
			line.ts, line.requestTime, line.upstreamTime, line.upstreamStatus, line.requestID = "1764201601.237", "4.350", "-", "-", "req-values-3"
		}).text(),
		defaultAccess().with(func(line *accessLine) {
			line.ts, line.upstreamTime, line.upstreamStatus, line.requestID = "1764201601.238", "0.012, -", "502, 200", "req-values-4"
		}).text(),
	}
	body, item, _ := cycleWith(t, lines...)
	if len(body.Requests) != 4 {
		t.Fatalf("요청별 줄 넷: %d", len(body.Requests))
	}
	first, second, third, fourth := body.Requests[0], body.Requests[1], body.Requests[2], body.Requests[3]
	if first.EndMs != 1764201601235 || first.DurationMs != 1005 || first.UpstreamMs == nil || *first.UpstreamMs != 1019 {
		t.Fatalf("글자에서 소수점을 뺀 정수(1.005 → 1005 · 1.003+0.007+0.009 → 1019): %+v", first)
	}
	if second.DurationMs != 7 || second.UpstreamMs != nil || second.UpstreamStatus != nil || second.Slot != nil {
		t.Fatalf("\"\" 는 null: %+v", second)
	}
	if third.DurationMs != 4350 || third.UpstreamMs != nil || third.UpstreamStatus != nil {
		t.Fatalf("\"-\" 는 null: %+v", third)
	}
	if fourth.UpstreamMs == nil || *fourth.UpstreamMs != 12 || fourth.UpstreamStatus == nil || *fourth.UpstreamStatus != "502, 200" {
		t.Fatalf("- 조각은 건너뛰고 · upstream_status 는 글 그대로: %+v", fourth)
	}
	if !strings.Contains(item.field("body"), `"upstream_ms":null,"upstream_status":null,"slot":null`) {
		t.Fatal("null 칸을 생략하지 않는다")
	}
	if *body.Segments[0].MaxMs != 4350 {
		t.Fatalf("max_ms: %d", *body.Segments[0].MaxMs)
	}
}

func TestRequestsExcludeEmptyAndFilteredPaths(t *testing.T) {
	defer failOnPanic(t)
	paths := map[string]bool{ // 경로 → 요청별 줄에 실리는가
		"/search?q=x": false, "/a%3Fb": false, "/a%3fb": false, "/host/192.0.2.10": false, "/x/2001:db8::7": false,
		"/x/%5B2001%3Adb8%3A%3A1%5D": false, "/v/192%2E0%2E2%2E10": false,
		"/time/12:30:00": true, "/release/1.2.3": true, "/api/doc": true,
	}
	var lines []string
	index := 0
	for path := range paths {
		lines = append(lines, defaultAccess().with(func(line *accessLine) { line.path = path; line.requestID = fmt.Sprintf("req-path-%02d", index) }).text())
		index++
	}
	lines = append(lines,
		defaultAccess().with(func(line *accessLine) { line.method = ""; line.requestID = "req-empty-method" }).text(),
		defaultAccess().with(func(line *accessLine) { line.path = ""; line.requestID = "req-empty-path" }).text(),
		defaultAccess().with(func(line *accessLine) {
			line.method, line.path = "", ""
			line.status = "400"
			line.requestID = "req-bad-line"
		}).text())
	body, _, harness := cycleWith(t, lines...)
	kept := map[string]bool{}
	for _, request := range body.Requests {
		kept[request.Path] = true
		if request.Method == "" || request.Path == "" {
			t.Fatalf("빈 메서드 · 경로는 요청별 줄에 싣지 않는다: %+v", request)
		}
	}
	for path, want := range paths {
		if kept[path] != want {
			t.Fatalf("경로 %q: 실림 %v, 기대 %v", path, kept[path], want)
		}
	}
	if body.Segments[0].Count != int64(len(lines)) || body.RequestsDropped != 0 {
		t.Fatalf("걸러진 줄도 묶음에는 센다 · requests_dropped 가 아니다: %+v dropped %d", body.Segments, body.RequestsDropped)
	}
	if filtered := harness.collector.Health().RequestsFiltered; filtered != 7 {
		t.Fatalf("헬스의 requests_filtered = 경로 필터에 걸린 줄 7: %d", filtered)
	}
}

func TestRequestsAreSortedAndCappedAt100(t *testing.T) {
	defer failOnPanic(t)
	lines := accessLines(101, lineBaseMillis, func(index int, line *accessLine) {
		line.ts = "1764201601.235" // 같은 end_ms — request_id 로 가른다
		line.requestID = fmt.Sprintf("req-%03d", 100-index)
	})
	body, _, _ := cycleWith(t, lines...)
	if len(body.Requests) != 100 || body.RequestsDropped != 1 {
		t.Fatalf("100줄 · 나머지 requests_dropped: %d %d", len(body.Requests), body.RequestsDropped)
	}
	for index, request := range body.Requests {
		if request.RequestID != fmt.Sprintf("req-%03d", index) {
			t.Fatalf("(end_ms, request_id, …) 오름차순 앞에서부터: %d번째 %s", index, request.RequestID)
		}
	}
}

func TestRequestsStopAtBodyLimit(t *testing.T) {
	defer failOnPanic(t)
	lines := accessLines(60, lineBaseMillis, func(index int, line *accessLine) {
		line.path = "/" + strings.Repeat(string(rune('a'+index%26)), 1000)
	})
	body, item, _ := cycleWith(t, lines...)
	encoded := item.field("body")
	if len(encoded) > 32768 {
		t.Fatalf("body 32,768바이트 초과: %d", len(encoded))
	}
	if body.RequestsDropped != int64(60-len(body.Requests)) || len(body.Requests) == 0 || len(body.Requests) == 60 {
		t.Fatalf("담지 못한 줄은 requests_dropped: %d줄 · dropped %d", len(body.Requests), body.RequestsDropped)
	}
	oneRequest, _ := json.Marshal(body.Requests[0])
	if len(encoded)+len(oneRequest)+1 <= 32768 {
		t.Fatalf("한 줄을 더 담을 수 있었는데 멈췄다: body %d · 줄 %d", len(encoded), len(oneRequest))
	}
}

func TestBodyFieldsAreNeverOmittedAndOrdered(t *testing.T) {
	defer failOnPanic(t)
	_, item, _ := cycleWith(t, defaultAccess().with(func(line *accessLine) { line.slot = ""; line.upstreamTime = "" }).text())
	encoded := item.field("body")
	var compacted bytes.Buffer
	if err := json.Compact(&compacted, []byte(encoded)); err != nil || compacted.String() != encoded {
		t.Fatalf("빈칸 없는 JSON: %s", encoded)
	}
	requireExactKeyOrder(t, "body", encoded, []string{"sources", "missing", "segments", "requests", "requests_dropped", "dependencies"})
	var raw struct {
		Sources  []json.RawMessage `json:"sources"`
		Segments []json.RawMessage `json:"segments"`
		Requests []json.RawMessage `json:"requests"`
	}
	json.Unmarshal([]byte(encoded), &raw)
	requireExactKeyOrder(t, "source", string(raw.Sources[0]), []string{"container", "container_id", "from", "to", "lines", "unparsed", "ignored"})
	requireExactKeyOrder(t, "segment", string(raw.Segments[0]), []string{"bucket_start_ms", "service", "caller", "class", "count", "status", "buckets", "max_ms"})
	requireExactKeyOrder(t, "request", string(raw.Requests[0]), []string{"request_id", "service", "caller", "method", "path", "path_truncated",
		"status", "end_ms", "duration_ms", "upstream_ms", "upstream_status", "slot", "bytes_sent"})
	var segment struct {
		Status json.RawMessage `json:"status"`
	}
	json.Unmarshal(raw.Segments[0], &segment)
	requireExactKeyOrder(t, "status", string(segment.Status), []string{"2xx", "3xx", "4xx", "5xx", "other"})
	if !strings.Contains(encoded, `"missing":[]`) || !strings.Contains(encoded, `"dependencies":[]`) || !strings.Contains(encoded, `"requests_dropped":0`) {
		t.Fatalf("빈 배열 · 0 도 낸다: %s", encoded)
	}
	if source := string(raw.Sources[0]); !strings.Contains(source, `"container_id":"`+trafficContainerID(nginx, 0)[:12]+`"`) {
		t.Fatalf("container_id 는 Id 의 앞 12자: %s", source)
	}
}

func TestHtmlCharactersAreNotEscaped(t *testing.T) {
	defer failOnPanic(t)
	_, item, _ := cycleWith(t, defaultAccess().with(func(line *accessLine) { line.path = "/a<b>&c" }).text())
	if !strings.Contains(item.field("body"), `"path":"/a<b>&c"`) {
		t.Fatalf("HTML 이스케이프를 하지 않는다: %s", item.field("body"))
	}
}
