package agent

// 통신 수집 시험의 받침 — 가짜 도커(접속 기록을 내는 /logs) · 가짜 스트림 · 접속 기록 줄 · 관측 서버의 검사를 다시 구현한 도우미.
//
// 가짜 도커의 /logs 는 실제(Docker 29.1.3 · json-file — scratchpad/t5-spike)처럼 군다:
//   - GET /containers/{이름 또는 Id}/json → {"Id": <64자>, "Name": "/<이름>"} · 없으면 404.
//   - GET /containers/{Id}/logs — **Id 로만**(이름으로 부르면 404 — 수집기가 이름 → Id 를 먼저 확인하는지 본다).
//     stdout=1 · stderr=0 · timestamps=1 이 아니면 400. since · until 은 `초.9자리`만(아니면 500 — 실제는 RFC3339 에 500) · 경계를 포함(≥ · ≤).
//     응답은 8바이트 머리([흐름 1, 0, 0, 0, 길이 빅엔디언]) + `<RFC3339 9자리>Z <내용>` 의 프레임. 16KB(16384바이트)를 넘는 줄은 같은 시각의
//     16KB 조각들로(마지막 조각만 줄바꿈). 흘려 보낸다(프레임마다 Flush) — 부른 쪽이 끊으면 거기서 멈추고 쓴 프레임 수를 남긴다.
//   - 지정한 동작: 상태(404 · 500 · 501) · 붙잡기(부른 쪽이 끊을 때까지) · 머리가 깨진 프레임 · 프레임 가운데서 끊김.

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/junhyeong9812/study-note-deploy-system-ci-cd/internal/shared"
)

// trafficWallStart — 시험의 벽시계 처음(2025-11-27T00:00:00Z = 1764201600 — 접속 기록 줄의 ts 와 같은 날).
var trafficWallStart = time.Date(2025, 11, 27, 0, 0, 0, 0, time.UTC)

const trafficServer = "ci-agent-158"

// ===== 접속 기록 줄 =====

// accessLine — 규약 §3.1 의 14칸 줄(nginx 의 차례). 값은 JSON 글자 그대로 넣는다(수는 따옴표 없이).
type accessLine struct {
	ts, requestID, caller, service, method, path string
	pathTruncated                                bool
	status                                       string
	requestTime, upstreamTime, upstreamStatus    string
	slot                                         string
	bytesSent                                    string
}

func defaultAccess() accessLine {
	return accessLine{ts: "1764201601.235", requestID: "req-3f9a0c1b2d4e", caller: "front", service: "backend", method: "GET",
		path: "/api/doc", status: "200", requestTime: "0.041", upstreamTime: "0.040", upstreamStatus: "200", slot: "blue", bytesSent: "5120"}
}

func quoteJSON(text string) string {
	encoded, _ := json.Marshal(text)
	return string(encoded)
}

// text — 줄 하나(줄바꿈 없이).
func (line accessLine) text() string {
	return `{"v":1,"ts":` + line.ts + `,"request_id":` + quoteJSON(line.requestID) + `,"caller":` + quoteJSON(line.caller) +
		`,"service":` + quoteJSON(line.service) + `,"method":` + quoteJSON(line.method) + `,"path":` + quoteJSON(line.path) +
		`,"path_truncated":` + strconv.FormatBool(line.pathTruncated) + `,"status":` + line.status + `,"request_time":` + line.requestTime +
		`,"upstream_time":` + quoteJSON(line.upstreamTime) + `,"upstream_status":` + quoteJSON(line.upstreamStatus) +
		`,"slot":` + quoteJSON(line.slot) + `,"bytes_sent":` + line.bytesSent + `}`
}

func (line accessLine) with(change func(line *accessLine)) accessLine {
	change(&line)
	return line
}

// ===== 가짜 도커 =====

type fakeLogLine struct {
	at      int64  // 도커 시각(epoch 나노초)
	content string // 줄의 내용(줄바꿈 포함 여부는 그대로 — 끝에 줄바꿈이 없으면 덜 온 줄)
}

type fakeLogContainer struct {
	name, id     string
	lines        []fakeLogLine
	logsStatus   int  // 0 이 아니면 /logs 가 이 상태로 답한다
	inspectFail  int  // 0 이 아니면 /json 이 이 상태로 답한다
	stallLogs    bool // 부른 쪽이 끊을 때까지 붙잡는다
	brokenHeader bool // 첫 프레임의 머리를 깬다(두 번째 바이트를 7 로)
	cutMidFrame  bool // 첫 프레임의 가운데서 응답을 끊는다
}

type logsRequest struct {
	path              string
	query             map[string]string
	framesWritten     int
	clientClosedEarly bool
}

type fakeLogDocker struct {
	mutex      sync.Mutex
	containers []*fakeLogContainer
	requests   []logsRequest
	inspects   []string
	server     *httptest.Server
}

func newFakeLogDocker(t *testing.T) *fakeLogDocker {
	docker := &fakeLogDocker{}
	docker.server = httptest.NewServer(http.HandlerFunc(docker.serve))
	t.Cleanup(docker.server.Close)
	return docker
}

func (docker *fakeLogDocker) api() *dockerAPI {
	return &dockerAPI{baseURL: docker.server.URL, client: docker.server.Client()}
}

// containerID — 이름에서 정한 64자 16진(시험마다 같다). 다시 만들면 generation 을 올린다.
func trafficContainerID(name string, generation int) string {
	sum := 0
	for _, character := range name {
		sum = sum*31 + int(character)
	}
	return fmt.Sprintf("%016x%016x%016x%016x", uint64(sum)+uint64(generation)*7919, uint64(generation)+1, uint64(len(name)), uint64(sum)^0xabcdef)
}

func (docker *fakeLogDocker) add(name string) *fakeLogContainer {
	docker.mutex.Lock()
	defer docker.mutex.Unlock()
	container := &fakeLogContainer{name: name, id: trafficContainerID(name, 0)}
	docker.containers = append(docker.containers, container)
	return container
}

// write — 그 컨테이너에 줄을 찍는다(도커 시각 at — 벽시계에서 나노초). content 에 줄바꿈을 붙인다.
func (docker *fakeLogDocker) write(name string, at time.Time, contents ...string) {
	docker.mutex.Lock()
	defer docker.mutex.Unlock()
	container := docker.byName(name)
	for index, content := range contents {
		container.lines = append(container.lines, fakeLogLine{at: at.UnixNano() + int64(index)*1000, content: content + "\n"})
	}
}

// writeRaw — 줄바꿈을 붙이지 않고 그대로(덜 온 줄 · 같은 시각의 줄).
func (docker *fakeLogDocker) writeRaw(name string, atNanoseconds int64, content string) {
	docker.mutex.Lock()
	defer docker.mutex.Unlock()
	container := docker.byName(name)
	container.lines = append(container.lines, fakeLogLine{at: atNanoseconds, content: content})
}

func (docker *fakeLogDocker) set(name string, change func(container *fakeLogContainer)) {
	docker.mutex.Lock()
	defer docker.mutex.Unlock()
	change(docker.byName(name))
}

// recreate — 같은 이름으로 다시 만든다(Id 가 바뀌고 로그가 빈다).
func (docker *fakeLogDocker) recreate(name string, generation int) {
	docker.set(name, func(container *fakeLogContainer) {
		container.id, container.lines = trafficContainerID(name, generation), nil
	})
}

// rotate — 시각이 before 보다 앞인 줄을 지운다(로그 돌림).
func (docker *fakeLogDocker) rotate(name string, before time.Time) {
	docker.set(name, func(container *fakeLogContainer) {
		kept := container.lines[:0]
		for _, line := range container.lines {
			if line.at >= before.UnixNano() {
				kept = append(kept, line)
			}
		}
		container.lines = kept
	})
}

func (docker *fakeLogDocker) byName(name string) *fakeLogContainer {
	for _, container := range docker.containers {
		if container.name == name {
			return container
		}
	}
	panic("가짜 도커에 없는 이름: " + name)
}

func (docker *fakeLogDocker) logRequests() []logsRequest {
	docker.mutex.Lock()
	defer docker.mutex.Unlock()
	return append([]logsRequest(nil), docker.requests...)
}

func (docker *fakeLogDocker) callCount() int {
	docker.mutex.Lock()
	defer docker.mutex.Unlock()
	return len(docker.requests) + len(docker.inspects)
}

var (
	fakeInspectPath = regexp.MustCompile(`^/containers/([^/]+)/json$`)
	fakeLogsPath    = regexp.MustCompile(`^/containers/([^/]+)/logs$`)
	nineDigitSecond = regexp.MustCompile(`^[0-9]+\.[0-9]{9}$`)
)

func parseNineDigitSecond(text string) (int64, bool) {
	if !nineDigitSecond.MatchString(text) {
		return 0, false
	}
	seconds, fraction, _ := strings.Cut(text, ".")
	whole, _ := strconv.ParseInt(seconds, 10, 64)
	nanos, _ := strconv.ParseInt(fraction, 10, 64)
	return whole*1_000_000_000 + nanos, true
}

func dockerTimestamp(nanoseconds int64) string {
	return time.Unix(0, nanoseconds).UTC().Format("2006-01-02T15:04:05.000000000Z")
}

func (docker *fakeLogDocker) serve(writer http.ResponseWriter, request *http.Request) {
	path := dockerVersionPrefix.ReplaceAllString(request.URL.Path, "")
	if match := fakeInspectPath.FindStringSubmatch(path); match != nil && request.Method == http.MethodGet {
		docker.mutex.Lock()
		docker.inspects = append(docker.inspects, match[1])
		var found *fakeLogContainer
		for _, container := range docker.containers {
			if container.name == match[1] || container.id == match[1] {
				found = container
			}
		}
		docker.mutex.Unlock()
		if found == nil {
			writeDockerJSON(writer, http.StatusNotFound, map[string]string{"message": "No such container: " + match[1]})
			return
		}
		if found.inspectFail != 0 {
			writeDockerJSON(writer, found.inspectFail, map[string]string{"message": "inspect failed"})
			return
		}
		writeDockerJSON(writer, http.StatusOK, map[string]any{"Id": found.id, "Name": "/" + found.name, "State": map[string]any{"Running": true}})
		return
	}
	match := fakeLogsPath.FindStringSubmatch(path)
	if match == nil || request.Method != http.MethodGet {
		writeDockerJSON(writer, http.StatusNotFound, map[string]string{"message": "page not found"})
		return
	}
	query := map[string]string{}
	for key, values := range request.URL.Query() {
		query[key] = strings.Join(values, ",")
	}
	record := logsRequest{path: path, query: query}
	docker.mutex.Lock()
	var container *fakeLogContainer
	for _, candidate := range docker.containers {
		if candidate.id == match[1] { // Id 로만
			container = candidate
		}
	}
	var lines []fakeLogLine
	var status int
	var stall, broken, cut bool
	if container != nil {
		lines = append(lines, container.lines...)
		status, stall, broken, cut = container.logsStatus, container.stallLogs, container.brokenHeader, container.cutMidFrame
	}
	docker.mutex.Unlock()
	finish := func() {
		docker.mutex.Lock()
		docker.requests = append(docker.requests, record)
		docker.mutex.Unlock()
	}
	defer finish()
	if container == nil {
		writeDockerJSON(writer, http.StatusNotFound, map[string]string{"message": "No such container: " + match[1]})
		return
	}
	if query["stdout"] != "1" || query["stderr"] != "0" || query["timestamps"] != "1" {
		writeDockerJSON(writer, http.StatusBadRequest, map[string]string{"message": "want stdout=1&stderr=0&timestamps=1"})
		return
	}
	since, sinceOK := parseNineDigitSecond(query["since"])
	until, untilOK := parseNineDigitSecond(query["until"])
	if !sinceOK || !untilOK {
		writeDockerJSON(writer, http.StatusInternalServerError, map[string]string{"message": "invalid value for since/until"})
		return
	}
	if status != 0 {
		writeDockerJSON(writer, status, map[string]string{"message": "logs failed"})
		return
	}
	if stall {
		select {
		case <-request.Context().Done():
		case <-time.After(5 * time.Second):
		}
		return
	}
	writer.Header().Set("Content-Type", "application/vnd.docker.multiplexed-stream")
	writer.WriteHeader(http.StatusOK)
	flusher, _ := writer.(http.Flusher)
	first := true
	for _, line := range lines {
		if line.at < since || line.at > until {
			continue
		}
		for _, piece := range splitSixteenKilobytes(line.content) {
			payload := []byte(dockerTimestamp(line.at) + " " + piece)
			header := make([]byte, 8)
			header[0] = 1
			binary.BigEndian.PutUint32(header[4:], uint32(len(payload)))
			if first && broken {
				header[1] = 7
			}
			frame := append(header, payload...)
			if first && cut {
				frame = frame[:8+len(payload)/2]
			}
			if _, err := writer.Write(frame); err != nil {
				record.clientClosedEarly = true
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
			if request.Context().Err() != nil {
				record.clientClosedEarly = true
				return
			}
			record.framesWritten++
			if first && cut {
				return
			}
			first = false
		}
	}
}

// splitSixteenKilobytes — 도커처럼 16384바이트 조각으로(줄바꿈은 마지막 조각에).
func splitSixteenKilobytes(content string) []string {
	const piece = 16384
	if len(content) <= piece {
		return []string{content}
	}
	var pieces []string
	for len(content) > piece {
		pieces = append(pieces, content[:piece])
		content = content[piece:]
	}
	if content != "" {
		pieces = append(pieces, content)
	}
	return pieces
}

// ===== 가짜 스트림(통신) =====

type trafficItem struct {
	stream    string
	maxLength int
	fields    []shared.StreamField
}

func (item trafficItem) field(name string) string {
	for _, field := range item.fields {
		if field.Name == name {
			return field.Value
		}
	}
	return ""
}

type fakeTrafficStream struct {
	mutex    sync.Mutex
	attempts []trafficItem
	sent     []trafficItem
	failNext int
	panicNow bool
}

func (stream *fakeTrafficStream) Append(ctx context.Context, name string, maxLength int, fields []shared.StreamField) error {
	item := trafficItem{stream: name, maxLength: maxLength, fields: append([]shared.StreamField(nil), fields...)}
	stream.mutex.Lock()
	defer stream.mutex.Unlock()
	stream.attempts = append(stream.attempts, item)
	if stream.panicNow {
		stream.panicNow = false
		panic("가짜 통신 스트림 패닉")
	}
	if stream.failNext > 0 {
		stream.failNext--
		return fmt.Errorf("가짜 Redis 실패")
	}
	stream.sent = append(stream.sent, item)
	return nil
}

func (stream *fakeTrafficStream) items() []trafficItem {
	stream.mutex.Lock()
	defer stream.mutex.Unlock()
	return append([]trafficItem(nil), stream.sent...)
}

func (stream *fakeTrafficStream) set(change func(stream *fakeTrafficStream)) {
	stream.mutex.Lock()
	defer stream.mutex.Unlock()
	change(stream)
}

// ===== 받침 묶음 =====

type trafficHarness struct {
	t          *testing.T
	agent      *Agent
	collector  *TrafficCollector
	settings   TrafficSettings
	docker     *fakeLogDocker
	stream     *fakeTrafficStream
	clock      *fakeClock
	logs       *logRecorder
	generation int
	// 읽은 자리 파일의 쓰기를 끼어든다 — 돌려주는 오류가 nil 이 아니면 쓰지 않는다(죽음을 흉내 낸다)
	cursorWriteFault func(content []byte) error
	directorySyncs   []string
	mutex            sync.Mutex
}

func trafficSettingsFor(cursorPath string, names ...string) TrafficSettings {
	return TrafficSettings{RedisAddress: "127.0.0.1:6379", Stream: "traffic", Server: trafficServer, Containers: names, CursorPath: cursorPath}
}

// newTrafficHarness — 이름마다 가짜 컨테이너를 만들고, 수집기의 이음매를 모두 가짜로 바꾼다(돌리지 않는다).
func newTrafficHarness(t *testing.T, names ...string) *trafficHarness {
	t.Helper()
	harness := &trafficHarness{t: t, agent: newPipelineAgent(t, &recordedCommands{}), docker: newFakeLogDocker(t),
		stream: &fakeTrafficStream{}, clock: &fakeClock{monotonic: 100 * time.Second, wall: trafficWallStart}, logs: &logRecorder{}}
	for _, name := range names {
		harness.docker.add(name)
	}
	harness.settings = trafficSettingsFor(filepath.Join(t.TempDir(), "traffic-cursor.json"), names...)
	harness.collector = harness.newCollector()
	return harness
}

// newCollector — 같은 설정 · 같은 가짜로 새 수집기(다시 시작을 흉내 낸다 — 메모리는 비고 파일만 남는다).
func (harness *trafficHarness) newCollector() *TrafficCollector {
	collector := harness.agent.newTrafficCollector(harness.settings)
	if collector == nil {
		harness.t.Fatal("newTrafficCollector 가 nil 을 돌려줬다")
	}
	collector.docker = harness.docker.api()
	collector.stream = harness.stream
	collector.wallNow = harness.clock.wallNow
	collector.logf = harness.logs.log
	collector.newGeneration = func() string {
		harness.mutex.Lock()
		defer harness.mutex.Unlock()
		harness.generation++
		return fmt.Sprintf("%016x", 0xa0000000+harness.generation)
	}
	collector.syncDirectory = func(directory string) error {
		harness.mutex.Lock()
		harness.directorySyncs = append(harness.directorySyncs, directory)
		harness.mutex.Unlock()
		return nil
	}
	collector.writeCursor = func(path string, content []byte) error {
		harness.mutex.Lock()
		fault := harness.cursorWriteFault
		harness.mutex.Unlock()
		if fault != nil {
			if err := fault(content); err != nil {
				return err
			}
		}
		return writeCursorAtomically(path, content, collector.syncDirectory)
	}
	return collector
}

// item — 스트림에 간 index 번째 항목(없으면 실패 — 시험 프로세스가 패닉으로 죽지 않게).
func (harness *trafficHarness) item(index int) trafficItem {
	harness.t.Helper()
	items := harness.stream.items()
	if index >= len(items) {
		harness.t.Fatalf("스트림에 %d번째 항목이 없다(%d개)", index, len(items))
	}
	return items[index]
}

// failOnPanic — 시험 안의 패닉을 실패로(뼈대에서 다른 시험의 결과를 가리지 않게).
func failOnPanic(t *testing.T) {
	if recovered := recover(); recovered != nil {
		t.Fatalf("시험이 패닉했다: %v", recovered)
	}
}

func (harness *trafficHarness) restart() {
	harness.collector = harness.newCollector()
}

func (harness *trafficHarness) cycle() {
	harness.t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		harness.collector.runCycle(context.Background())
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		harness.t.Fatal("주기가 30초 안에 끝나지 않았다")
	}
}

// advance — 벽시계를 움직인다(주기 사이).
func (harness *trafficHarness) advance(elapsed time.Duration) { harness.clock.advance(elapsed) }

func (harness *trafficHarness) setCursorWriteFault(fault func(content []byte) error) {
	harness.mutex.Lock()
	defer harness.mutex.Unlock()
	harness.cursorWriteFault = fault
}

func (harness *trafficHarness) cursorFile() trafficCursorFile {
	harness.t.Helper()
	content, err := os.ReadFile(harness.settings.CursorPath)
	if err != nil {
		harness.t.Fatalf("읽은 자리의 파일을 읽지 못했다: %v", err)
	}
	var file trafficCursorFile
	if err := json.Unmarshal(content, &file); err != nil {
		harness.t.Fatalf("읽은 자리의 파일이 JSON 이 아니다: %v\n%s", err, content)
	}
	if len(harness.settings.Containers) > 0 && len(file.Containers) != len(harness.settings.Containers) {
		harness.t.Fatalf("읽은 자리의 파일에 컨테이너가 %d개(기대 %d)", len(file.Containers), len(harness.settings.Containers))
	}
	return file
}

// ===== 읽은 자리의 파일(시험이 읽는 꼴 — traffic_cursor_test.go 머리의 계약) =====

type trafficPosition struct {
	DockerTimestamp string `json:"docker_ts"`
	Skip            int64  `json:"skip"`
}

type trafficCursorContainer struct {
	Name     string          `json:"name"`
	ID       string          `json:"id"`
	Position trafficPosition `json:"position"`
	Anchored bool            `json:"anchored"`
}

type trafficCursorField struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type trafficCursorLast struct {
	Seq     int64                `json:"seq"`
	At      int64                `json:"at"`
	State   string               `json:"state"`
	Sources []json.RawMessage    `json:"sources"`
	Fields  []trafficCursorField `json:"fields"`
}

type trafficCursorFile struct {
	Version    int                      `json:"version"`
	Generation string                   `json:"generation"`
	Containers []trafficCursorContainer `json:"containers"`
	Last       *trafficCursorLast       `json:"last"`
}

// ===== 항목 · body =====

type trafficSourceBody struct {
	Container   string          `json:"container"`
	ContainerID string          `json:"container_id"`
	From        trafficPosition `json:"from"`
	To          trafficPosition `json:"to"`
	Lines       int64           `json:"lines"`
	Unparsed    int64           `json:"unparsed"`
	Ignored     int64           `json:"ignored"`
}

type trafficSegmentBody struct {
	BucketStartMs int64            `json:"bucket_start_ms"`
	Service       string           `json:"service"`
	Caller        string           `json:"caller"`
	Class         string           `json:"class"`
	Count         int64            `json:"count"`
	Status        map[string]int64 `json:"status"`
	Buckets       []int64          `json:"buckets"`
	MaxMs         *int64           `json:"max_ms"`
}

type trafficRequestBody struct {
	RequestID      string  `json:"request_id"`
	Service        string  `json:"service"`
	Caller         string  `json:"caller"`
	Method         string  `json:"method"`
	Path           string  `json:"path"`
	PathTruncated  bool    `json:"path_truncated"`
	Status         int64   `json:"status"`
	EndMs          int64   `json:"end_ms"`
	DurationMs     int64   `json:"duration_ms"`
	UpstreamMs     *int64  `json:"upstream_ms"`
	UpstreamStatus *string `json:"upstream_status"`
	Slot           *string `json:"slot"`
	BytesSent      int64   `json:"bytes_sent"`
}

type trafficBody struct {
	Sources         []trafficSourceBody  `json:"sources"`
	Missing         []string             `json:"missing"`
	Segments        []trafficSegmentBody `json:"segments"`
	Requests        []trafficRequestBody `json:"requests"`
	RequestsDropped int64                `json:"requests_dropped"`
	Dependencies    []json.RawMessage    `json:"dependencies"`
}

func decodeTrafficBody(t *testing.T, item trafficItem) trafficBody {
	t.Helper()
	var body trafficBody
	if err := json.Unmarshal([]byte(item.field("body")), &body); err != nil {
		t.Fatalf("body 가 JSON 이 아니다: %v\n%s", err, item.field("body"))
	}
	return body
}

func (body trafficBody) source(t *testing.T, container string) trafficSourceBody {
	t.Helper()
	for _, source := range body.Sources {
		if source.Container == container {
			return source
		}
	}
	t.Fatalf("sources 에 %s 가 없다: %+v", container, body.Sources)
	return trafficSourceBody{}
}

func totalLines(t *testing.T, items []trafficItem) int64 {
	total := int64(0)
	for _, item := range items {
		for _, source := range decodeTrafficBody(t, item).Sources {
			total += source.Lines
		}
	}
	return total
}

// ===== 관측 서버의 검사를 다시 구현한 도우미(계약의 잇기 — H) =====
// 정본: observe-traffic src/main/kotlin/xyz/junproject/observe/traffic/TrafficEntry.kt
//   :167 SERVER_NAME · :168 GENERATION · :169 SEQUENCE · :171 NAME · :172 REQUEST_ID · :173 METHOD · :174 MISSING · :176 AT ·
//   :210 빠진 칸(빈 글자 포함) · :211 v · :220 body 32768 UTF-8 바이트 · :221 JSON(뒤에 더 붙은 글자 거절) ·
//   :230-238 body 꼴(sources 객체 배열 · dependencies 배열 · missing 꼴 · segments · 열쇠 겹침 없음 · requests ≤ 100) ·
//   :249-264 묶음(bucket_start_ms 30000 의 배수 · class · count 1~10억 · status 다섯 열쇠 합 = count · buckets 20개 · request 면 합 = count, 아니면 0 과 null) ·
//   :271-289 요청별 줄 · :296-300 경로 필터(이 도우미는 걸러질 줄이 없는지 본다 — 에이전트가 먼저 거른다) · :310-315 글자(제어 문자 · 길이 128) ·
//   :320-323 0 이상 정수 · :329 세는 수 10억.

var (
	observeServerName = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)
	observeGeneration = regexp.MustCompile(`^[0-9a-f]{16}$`)
	observeSequence   = regexp.MustCompile(`^[1-9][0-9]{0,17}$`)
	observeName       = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
	observeRequestID  = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	observeMethod     = regexp.MustCompile(`^[A-Z_-]{0,32}$`)
	observeMissing    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}:[a-z_]{1,32}$`)
	observeAt         = regexp.MustCompile(`^[0-9]{1,15}$`)
	observeEncodedQ   = regexp.MustCompile(`%3[Ff]`)
	observeAddresses  = []*regexp.Regexp{
		regexp.MustCompile(`(^|[^0-9])[0-9]{1,3}(\.[0-9]{1,3}){3}([^0-9]|$)`),
		regexp.MustCompile(`(^|[^0-9A-Fa-f])[0-9A-Fa-f]{1,4}(:[0-9A-Fa-f]{1,4}){7}([^0-9A-Fa-f]|$)`),
		regexp.MustCompile(`(^|[^0-9A-Za-z])[0-9A-Fa-f]{1,4}::|::[0-9A-Fa-f]{1,4}([^0-9A-Za-z]|$)`),
		regexp.MustCompile(`\[[^\]]*:[^\]]*]`),
	}
)

// observePathAllowed — TrafficEntry.kt:296-300 와 같은 판정.
func observePathAllowed(path string) bool {
	if strings.Contains(path, "?") || observeEncodedQ.MatchString(path) {
		return false
	}
	decoded := path
	for encoded, plain := range map[string]string{"%3[Aa]": ":", "%2[Ee]": ".", "%5[Bb]": "[", "%5[Dd]": "]"} {
		decoded = regexp.MustCompile(encoded).ReplaceAllString(decoded, plain)
	}
	for _, shape := range observeAddresses {
		if shape.MatchString(decoded) {
			return false
		}
	}
	return true
}

// requireObserveAccepts — 관측 서버의 해석이 이 항목을 받는다(버림 0 · 그 줄만 버리는 요청별 줄 0).
func requireObserveAccepts(t *testing.T, item trafficItem) {
	t.Helper()
	fail := func(format string, arguments ...any) {
		t.Helper()
		t.Fatalf("관측 서버가 이 항목을 버린다: "+format, arguments...)
	}
	if item.field("kind") != "traffic" {
		fail("kind %q", item.field("kind"))
	}
	for _, name := range []string{"v", "server", "generation", "seq", "at", "body"} {
		if item.field(name) == "" {
			fail("빠진 칸 %s", name)
		}
	}
	if item.field("v") != "1" || !observeServerName.MatchString(item.field("server")) || !observeGeneration.MatchString(item.field("generation")) ||
		!observeSequence.MatchString(item.field("seq")) || !observeAt.MatchString(item.field("at")) || item.field("at") == strings.Repeat("0", len(item.field("at"))) {
		fail("스트림 칸의 꼴: %v", item.fields)
	}
	body := item.field("body")
	if len(body) > 32768 || !utf8.ValidString(body) {
		fail("body %d바이트", len(body))
	}
	decoder := json.NewDecoder(strings.NewReader(body))
	decoder.UseNumber()
	var tree map[string]any
	if err := decoder.Decode(&tree); err != nil {
		fail("body JSON: %v", err)
	}
	if _, err := decoder.Token(); err != io.EOF {
		fail("body 뒤에 더 붙은 글자")
	}
	array := func(node map[string]any, name string) []any {
		value, present := node[name]
		list, isArray := value.([]any)
		if !present || !isArray {
			fail("%s 가 배열이 아니다", name)
		}
		return list
	}
	integer := func(value any, low int64, high *big.Int) int64 {
		number, ok := value.(json.Number)
		if !ok || strings.ContainsAny(string(number), ".eE") {
			fail("정수가 아니다: %v", value)
		}
		parsed, err := strconv.ParseInt(string(number), 10, 64)
		if err != nil || parsed < low || (high != nil && big.NewInt(parsed).Cmp(high) > 0) {
			fail("정수의 범위: %v", value)
		}
		return parsed
	}
	billion := big.NewInt(1_000_000_000)
	text := func(value any, limit int) string {
		textValue, ok := value.(string)
		if !ok || len([]rune(textValue)) > limit {
			fail("글자가 아니거나 길다: %v", value)
		}
		for _, character := range textValue {
			if character < ' ' || character == 0x7f {
				fail("제어 문자: %q", textValue)
			}
		}
		return textValue
	}
	for _, source := range array(tree, "sources") {
		if _, ok := source.(map[string]any); !ok {
			fail("sources 의 원소가 객체가 아니다")
		}
	}
	array(tree, "dependencies")
	for _, missing := range array(tree, "missing") {
		if !observeMissing.MatchString(text(missing, 1<<30)) {
			fail("missing 꼴: %v", missing)
		}
	}
	keys := map[string]bool{}
	for _, raw := range array(tree, "segments") {
		segment, ok := raw.(map[string]any)
		if !ok {
			fail("묶음이 객체가 아니다")
		}
		start := integer(segment["bucket_start_ms"], 1, nil)
		if start%30000 != 0 {
			fail("bucket_start_ms 가 30000 의 배수가 아니다: %d", start)
		}
		class := text(segment["class"], 128)
		if class != "request" && class != "health" && class != "stream" {
			fail("class %q", class)
		}
		service, caller := text(segment["service"], 128), text(segment["caller"], 128)
		if !observeName.MatchString(service) || !observeName.MatchString(caller) {
			fail("service · caller 꼴: %s %s", service, caller)
		}
		key := fmt.Sprint(start, service, caller, class)
		if keys[key] {
			fail("묶음의 열쇠가 겹친다: %s", key)
		}
		keys[key] = true
		count := integer(segment["count"], 1, billion)
		status, ok := segment["status"].(map[string]any)
		if !ok {
			fail("status 가 객체가 아니다")
		}
		sum := int64(0)
		for _, name := range []string{"2xx", "3xx", "4xx", "5xx", "other"} {
			sum += integer(status[name], 0, billion)
		}
		if sum != count {
			fail("status 의 합 %d ≠ count %d", sum, count)
		}
		buckets := array(segment, "buckets")
		if len(buckets) != 20 {
			fail("buckets %d개", len(buckets))
		}
		bucketSum := int64(0)
		for _, bucket := range buckets {
			bucketSum += integer(bucket, 0, billion)
		}
		maxValue, present := segment["max_ms"]
		if !present {
			fail("max_ms 칸이 없다")
		}
		if class == "request" {
			if bucketSum != count {
				fail("분포의 합 %d ≠ count %d", bucketSum, count)
			}
			integer(maxValue, 0, nil)
		} else if bucketSum != 0 || maxValue != nil {
			fail("request 가 아닌 묶음의 분포 · max_ms")
		}
	}
	requests := array(tree, "requests")
	if len(requests) > 100 {
		fail("요청별 줄 %d", len(requests))
	}
	for _, raw := range requests {
		request, ok := raw.(map[string]any)
		if !ok {
			fail("요청별 줄이 객체가 아니다")
		}
		for _, name := range []string{"request_id", "service", "caller", "method", "path", "path_truncated", "status", "end_ms", "duration_ms",
			"upstream_ms", "upstream_status", "slot", "bytes_sent"} {
			if _, present := request[name]; !present {
				fail("요청별 줄에 %s 가 없다", name)
			}
		}
		path := text(request["path"], 1024)
		if !observePathAllowed(path) {
			fail("관측 서버가 그 줄만 버릴 경로: %q", path)
		}
		if _, ok := request["path_truncated"].(bool); !ok {
			fail("path_truncated 가 참 · 거짓이 아니다")
		}
		if !observeRequestID.MatchString(text(request["request_id"], 128)) || !observeName.MatchString(text(request["service"], 128)) ||
			!observeName.MatchString(text(request["caller"], 128)) || !observeMethod.MatchString(text(request["method"], 128)) {
			fail("요청별 줄의 꼴: %v", request)
		}
		integer(request["status"], 0, big.NewInt(999))
		integer(request["end_ms"], 1, nil)
		integer(request["duration_ms"], 0, nil)
		integer(request["bytes_sent"], 0, nil)
		if request["upstream_ms"] != nil {
			integer(request["upstream_ms"], 0, nil)
		}
		for _, name := range []string{"upstream_status", "slot"} {
			if request[name] != nil {
				text(request[name], 128)
			}
		}
	}
	integer(tree["requests_dropped"], 0, billion)
}

// requireExactKeyOrder — JSON 객체의 칸 차례가 want 와 같다(글자로 본다).
func requireExactKeyOrder(t *testing.T, label, object string, want []string) {
	t.Helper()
	decoder := json.NewDecoder(strings.NewReader(object))
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		t.Fatalf("%s: 객체가 아니다", label)
	}
	var keys []string
	for decoder.More() {
		token, _ := decoder.Token()
		keys = append(keys, token.(string))
		var skip json.RawMessage
		_ = decoder.Decode(&skip)
	}
	if strings.Join(keys, ",") != strings.Join(want, ",") {
		t.Fatalf("%s 의 칸 차례: %v, 기대 %v", label, keys, want)
	}
}
