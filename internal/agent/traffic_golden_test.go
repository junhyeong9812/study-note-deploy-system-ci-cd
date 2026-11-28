package agent

// H. 관측 서버의 파서가 받는 항목인가 — 골든 파일 testdata/traffic/*.json.
//
// 정한 계약: 파일 하나 = 스트림 항목 하나 = 스트림 칸을 차례대로 담은 JSON 객체 한 줄(+ 줄바꿈):
//   {"kind":"traffic","v":"1","server":"ci-agent-158","generation":"…","seq":"…","at":"…","body":"<body 의 글자 그대로>"}
// 정해진 입력(가짜 도커 · 가짜 시계 · 정해진 세대)에서 수집기는 늘 이 글자를 낸다. 메인이 이 파일들을 관측 서버의 실제 파서(TrafficEntryParser)에 넣어 본다.
// 파일은 TRAFFIC_GOLDEN_UPDATE=1 일 때만 다시 쓴다 — 구현이 이 글자에 맞춰야 한다(구현에 맞춰 파일을 고치지 않는다).

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func goldenJSON(item trafficItem) []byte {
	var buffer bytes.Buffer
	buffer.WriteString("{")
	for index, field := range item.fields {
		if index > 0 {
			buffer.WriteString(",")
		}
		name, _ := json.Marshal(field.Name)
		value, _ := json.Marshal(field.Value)
		buffer.Write(name)
		buffer.WriteString(":")
		buffer.Write(value)
	}
	buffer.WriteString("}\n")
	return buffer.Bytes()
}

func requireGolden(t *testing.T, name string, item trafficItem) {
	t.Helper()
	requireObserveAccepts(t, item)
	path := filepath.Join("testdata", "traffic", name+".json")
	produced := goldenJSON(item)
	if os.Getenv("TRAFFIC_GOLDEN_UPDATE") == "1" {
		os.MkdirAll(filepath.Dir(path), 0o755)
		if err := os.WriteFile(path, produced, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	golden, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("골든 파일을 읽지 못했다: %v", err)
	}
	if !bytes.Equal(golden, produced) {
		t.Fatalf("%s: 항목의 글자가 골든 파일과 다르다\n got %s\nwant %s", name, produced, golden)
	}
}

func TestGoldenOrdinaryEntry(t *testing.T) {
	defer failOnPanic(t)
	harness := startedHarness(t, nginx)
	harness.docker.write(nginx, trafficWallStart.Add(5*time.Second),
		"nginx: worker process started",
		defaultAccess().text(),
		defaultAccess().with(func(line *accessLine) {
			line.ts, line.requestID, line.status, line.requestTime, line.upstreamTime, line.upstreamStatus = "1764201602.010", "req-second", "502", "1.005", "0.500, 0.505", "502, 502"
		}).text(),
		defaultAccess().with(func(line *accessLine) {
			line.caller, line.path, line.requestID, line.upstreamTime, line.slot = "alert", "/actuator/health", "req-health", "", ""
		}).text(),
		defaultAccess().with(func(line *accessLine) {
			line.service, line.caller, line.method, line.path, line.requestID, line.requestTime = "ci-agent", "ci-master", "POST", "/agent/ci", "req-stream", "93.120"
		}).text(),
		`{"v":1,"broken"`)
	harness.advance(30 * time.Second)
	harness.cycle()
	requireGolden(t, "ordinary", harness.item(1))
}

func TestGoldenEmptyEntries(t *testing.T) {
	defer failOnPanic(t)
	harness := startedHarness(t, nginx, "front-nginx")
	requireGolden(t, "first_cursor_lost", harness.item(0))
	harness.advance(30 * time.Second)
	harness.cycle()
	requireGolden(t, "empty", harness.item(1))
}

func TestGoldenCrowdedEntry(t *testing.T) {
	defer failOnPanic(t)
	harness := startedHarness(t, nginx)
	writeLines(harness, nginx, 5*time.Second, accessLines(150, lineBaseMillis, func(index int, line *accessLine) {
		line.path = fmt.Sprintf("/api/doc/%03d/%s", index, strings.Repeat("s", 200))
	}))
	harness.advance(30 * time.Second)
	harness.cycle()
	item := harness.item(1)
	if body := decodeTrafficBody(t, item); body.RequestsDropped == 0 || len(body.Requests)+int(body.RequestsDropped) != 150 {
		t.Fatalf("몰려서 요청별 줄이 잘린 항목이어야 한다: %d + %d", len(body.Requests), body.RequestsDropped)
	}
	requireGolden(t, "crowded", item)
}

func TestGoldenEntryWithMissing(t *testing.T) {
	defer failOnPanic(t)
	harness := startedHarness(t, "a-nginx", "b-nginx", "c-nginx")
	harness.docker.write("a-nginx", trafficWallStart.Add(5*time.Second), accessLines(1, lineBaseMillis, nil)...)
	harness.docker.set("b-nginx", func(container *fakeLogContainer) { container.logsStatus = 500 })
	harness.docker.recreate("c-nginx", 1)
	harness.docker.write("c-nginx", trafficWallStart.Add(-time.Minute), accessLines(1, lineBaseMillis+7, func(_ int, line *accessLine) { line.service = "front" })...)
	harness.advance(30 * time.Second)
	harness.cycle()
	requireGolden(t, "missing", harness.item(1))
}

func TestGoldenLongPathEntry(t *testing.T) {
	defer failOnPanic(t)
	harness := startedHarness(t, nginx)
	harness.docker.write(nginx, trafficWallStart.Add(5*time.Second),
		defaultAccess().with(func(line *accessLine) {
			line.path, line.pathTruncated, line.method = "/"+strings.Repeat("%E2%9C%93", 113)+"xxxxxx", true, "PROPFIND" // 1024자
		}).text(),
		defaultAccess().with(func(line *accessLine) { line.path, line.requestID = "/문서/<제목>&\"따옴표\"", "req-unicode" }).text())
	harness.advance(30 * time.Second)
	harness.cycle()
	requireGolden(t, "long_path", harness.item(1))
}
