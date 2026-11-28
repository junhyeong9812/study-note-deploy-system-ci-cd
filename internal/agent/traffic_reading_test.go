package agent

// B. 읽기 — 기준: requirement-spec.md(traffic-map) "T5 착수 — 읽기" · "선검증 반영" 3 · 4 · 11.
//
// 정한 계약:
//   - 컨테이너마다 GET /containers/{이름}/json 으로 Id 를 확인하고 그 Id 로 GET /containers/{Id}/logs?stdout=1&stderr=0&timestamps=1&since=<초.9자리>&until=<초.9자리>.
//     since = 읽은 자리의 시각 · until = 지금(벽시계) − 2초. 읽은 자리 (시각, skip) 의 시각보다 앞의 프레임과 그 시각의 앞 skip 개는 건너뛴다.
//   - 파일이 없을 때의 첫 주기는 "지금부터"(자리 = 그때의 벽시계, skip 0 — 도커의 로그를 읽지 않아도 된다) — 빈 항목 · cursor_lost.
//   - 줄바꿈 없이 끝난 프레임은 같은 시각의 다음 프레임과 이어 한 줄. 시각이 다른 프레임이 오면 그 줄은 줄바꿈 없이 끝난 한 줄이다.
//     이은 줄이 64KB(65536바이트 — 시각 · 빈칸을 뺀 내용)를 넘으면 unparsed 한 줄.
//   - 응답의 마지막 프레임이 줄바꿈 없이 끝났으면 그 시각의 묶음 전체를 이번 범위에서 뺀다(to 는 그 앞). 다음 주기에도 같은 프레임 수로
//     줄바꿈 없이 끝나 있으면 unparsed 한 줄로 세고 지나간다.
//   - 상한: 한 번 읽기는 프레임 5,000개 또는 몸 8MB(머리 포함 바이트) 가운데 먼저 닿는 쪽에서 멈추고(같은 시각의 묶음은 가르지 않는다),
//     같은 시각의 묶음이 프레임 256개를 넘으면 256개에서 끊는다 — 삼킨 조각은 unparsed 한 줄 · to = (그 시각, 256). 다음 읽기는 skip 으로 건너
//     나머지 조각을 줄바꿈까지 버린다(세지 않는다).
//   - 기한: 컨테이너 하나의 읽기(Id 확인 포함) readDeadline(10초) · 주기 전체 cycleDeadline(25초). 읽기 기한을 넘긴 컨테이너는 unreadable ·
//     주기 기한에 걸린 컨테이너와 그 뒤의 컨테이너는 그 항목에서 빈 범위(from = to · missing 에 없음).
//   - 응답을 통째로 메모리에 올리지 않는다 — 상한에서 멈추면 연결을 끊는다(가짜 도커가 다 쓰지 못한다).

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

const nginx = "backend-nginx"

// accessLines — count 줄. ts 는 base 초부터 1밀리초씩(소수 셋째 자리).
func accessLines(count int, baseMillis int64, change func(index int, line *accessLine)) []string {
	lines := make([]string, count)
	for index := range lines {
		line := defaultAccess()
		milliseconds := baseMillis + int64(index)
		line.ts = fmt.Sprintf("%d.%03d", milliseconds/1000, milliseconds%1000)
		line.requestID = fmt.Sprintf("req-%06d", index)
		if change != nil {
			change(index, &line)
		}
		lines[index] = line.text()
	}
	return lines
}

const lineBaseMillis = 1764201601235

// startedHarness — 첫 주기(지금부터 · cursor_lost)를 돌린 뒤의 받침.
func startedHarness(t *testing.T, names ...string) *trafficHarness {
	harness := newTrafficHarness(t, names...)
	harness.cycle()
	return harness
}

func TestFirstCycleStartsFromNowWithCursorLost(t *testing.T) {
	defer failOnPanic(t)
	harness := newTrafficHarness(t, nginx)
	harness.docker.write(nginx, trafficWallStart.Add(-10*time.Second), accessLines(3, lineBaseMillis, nil)...) // 지금보다 앞 — 읽지 않는다
	harness.cycle()
	items := harness.stream.items()
	if len(items) != 1 || items[0].field("seq") != "1" {
		t.Fatalf("첫 주기는 순번 1 의 항목 하나: %d", len(items))
	}
	body := decodeTrafficBody(t, items[0])
	if strings.Join(body.Missing, ",") != nginx+":cursor_lost" || totalLines(t, items) != 0 {
		t.Fatalf("파일이 없으면 지금부터 · cursor_lost: %+v", body)
	}
	harness.advance(30 * time.Second)
	harness.cycle()
	if lines := totalLines(t, harness.stream.items()); lines != 0 {
		t.Fatalf("지금보다 앞의 줄을 읽었다: %d", lines)
	}
}

func TestLogsRequestUsesIdAndNineDigitSeconds(t *testing.T) {
	defer failOnPanic(t)
	harness := startedHarness(t, nginx)
	harness.docker.write(nginx, trafficWallStart.Add(5*time.Second), accessLines(2, lineBaseMillis, nil)...)
	harness.advance(30 * time.Second)
	harness.cycle()
	requests := harness.docker.logRequests()
	if len(requests) == 0 {
		t.Fatal("로그를 읽지 않았다")
	}
	last := requests[len(requests)-1]
	if last.path != "/containers/"+trafficContainerID(nginx, 0)+"/logs" {
		t.Fatalf("이름이 아니라 확인한 Id 로 읽는다: %s", last.path)
	}
	want := map[string]string{"stdout": "1", "stderr": "0", "timestamps": "1", "since": "1764201600.000000000", "until": "1764201628.000000000"}
	for name, value := range want {
		if last.query[name] != value {
			t.Fatalf("%s=%q, 기대 %q (%v)", name, last.query[name], value, last.query)
		}
	}
	if lines := totalLines(t, harness.stream.items()); lines != 2 {
		t.Fatalf("두 줄을 읽어야 한다: %d", lines)
	}
}

func TestSameTimestampFramesAreNotCountedTwice(t *testing.T) {
	defer failOnPanic(t)
	harness := startedHarness(t, nginx)
	at := trafficWallStart.Add(5 * time.Second).UnixNano()
	for _, line := range accessLines(3, lineBaseMillis, nil) { // 같은 시각의 줄 셋(도커에선 드물다 — skip 이 쓰이는 자리)
		harness.docker.writeRaw(nginx, at, line+"\n")
	}
	harness.advance(30 * time.Second)
	harness.cycle()
	file := harness.cursorFile()
	if file.Containers[0].Position != (trafficPosition{dockerTimestamp(at), 3}) || !file.Containers[0].Anchored {
		t.Fatalf("읽은 자리는 (마지막 프레임의 시각, 그 시각의 프레임 수) · anchored: %+v", file.Containers[0])
	}
	harness.docker.write(nginx, trafficWallStart.Add(40*time.Second), accessLines(2, lineBaseMillis+100, nil)...)
	harness.advance(30 * time.Second)
	harness.cycle() // since = 그 시각 — 앞의 셋이 다시 오지만 skip 3 으로 건너뛴다
	if lines := totalLines(t, harness.stream.items()); lines != 5 {
		t.Fatalf("같은 줄을 두 번 세거나 빠뜨렸다: 합 %d, 기대 5", lines)
	}
}

func TestLinesAcrossManyCyclesAreCountedExactlyOnce(t *testing.T) {
	defer failOnPanic(t)
	harness := startedHarness(t, nginx, "front-nginx")
	written := 0
	for cycle := 1; cycle <= 6; cycle++ {
		for index := 0; index < 7*cycle; index++ {
			at := trafficWallStart.Add(time.Duration(cycle)*30*time.Second - 3*time.Second + time.Duration(index)*time.Millisecond) // 경계(− 2초) 언저리
			harness.docker.write([]string{nginx, "front-nginx"}[index%2], at, accessLines(1, lineBaseMillis+int64(written), nil)...)
			written++
		}
		harness.advance(30 * time.Second)
		harness.cycle()
	}
	harness.advance(30 * time.Second)
	harness.cycle()
	if lines := totalLines(t, harness.stream.items()); lines != int64(written) {
		t.Fatalf("주기를 넘어 이어 읽은 줄의 합 %d ≠ 찍은 줄 %d", lines, written)
	}
	for _, item := range harness.stream.items() {
		requireObserveAccepts(t, item)
	}
}

func TestSixteenKilobytePiecesAreJoinedIntoOneLine(t *testing.T) {
	defer failOnPanic(t)
	harness := startedHarness(t, nginx)
	long := defaultAccess().with(func(line *accessLine) {
		line.upstreamTime = strings.TrimSuffix(strings.Repeat("0.001, ", 4000), ", ") // 약 28KB — 조각 둘
	})
	harness.docker.write(nginx, trafficWallStart.Add(5*time.Second), long.text())
	harness.advance(30 * time.Second)
	harness.cycle()
	body := decodeTrafficBody(t, harness.item(1))
	if source := body.source(t, nginx); source.Lines != 1 || source.Unparsed != 0 {
		t.Fatalf("16KB 조각을 한 줄로 잇는다: %+v", source)
	}
	if len(body.Requests) != 1 || body.Requests[0].UpstreamMs == nil || *body.Requests[0].UpstreamMs != 4000 {
		t.Fatalf("이은 줄의 upstream_ms = 4000: %+v", body.Requests)
	}
	if position := harness.cursorFile().Containers[0].Position; position.Skip != 2 {
		t.Fatalf("skip 은 프레임의 수(조각 둘): %+v", position)
	}
}

func TestJoinedLineOver64KilobytesIsUnparsed(t *testing.T) {
	defer failOnPanic(t)
	harness := startedHarness(t, nginx)
	harness.docker.write(nginx, trafficWallStart.Add(5*time.Second), `{"v":1,"pad":"`+strings.Repeat("x", 70000)+`"}`)
	harness.docker.write(nginx, trafficWallStart.Add(6*time.Second), accessLines(1, lineBaseMillis, nil)...)
	harness.advance(30 * time.Second)
	harness.cycle()
	if source := decodeTrafficBody(t, harness.item(1)).source(t, nginx); source.Unparsed != 1 || source.Lines != 1 {
		t.Fatalf("64KB 를 넘는 줄은 unparsed 하나: %+v", source)
	}
}

func TestIncompleteLastLineWaitsForItsRest(t *testing.T) {
	defer failOnPanic(t)
	harness := startedHarness(t, nginx)
	line := accessLines(1, lineBaseMillis, nil)[0]
	harness.docker.write(nginx, trafficWallStart.Add(5*time.Second), accessLines(1, lineBaseMillis+1, nil)...)
	at := trafficWallStart.Add(6 * time.Second).UnixNano()
	harness.docker.writeRaw(nginx, at, line[:40]) // 뒤가 아직 오지 않은 조각
	harness.advance(30 * time.Second)
	harness.cycle()
	body := decodeTrafficBody(t, harness.item(1))
	if source := body.source(t, nginx); source.Lines != 1 || source.Unparsed != 0 || source.To.DockerTimestamp == dockerTimestamp(at) {
		t.Fatalf("덜 온 줄은 범위에서 뺀다(to 는 그 앞): %+v", source)
	}
	harness.docker.writeRaw(nginx, at, line[40:]+"\n") // 뒤가 왔다
	harness.advance(30 * time.Second)
	harness.cycle()
	if source := decodeTrafficBody(t, harness.item(2)).source(t, nginx); source.Lines != 1 || source.Unparsed != 0 {
		t.Fatalf("뒤가 오면 한 줄: %+v", source)
	}
}

func TestIncompleteLineThatNeverCompletesIsUnparsedOnce(t *testing.T) {
	defer failOnPanic(t)
	harness := startedHarness(t, nginx)
	at := trafficWallStart.Add(6 * time.Second).UnixNano()
	harness.docker.writeRaw(nginx, at, accessLines(1, lineBaseMillis, nil)[0][:40])
	for range 3 {
		harness.advance(30 * time.Second)
		harness.cycle()
	}
	unparsed := int64(0)
	for _, item := range harness.stream.items() {
		for _, source := range decodeTrafficBody(t, item).Sources {
			unparsed += source.Unparsed
		}
	}
	if unparsed != 1 || harness.cursorFile().Containers[0].Position.DockerTimestamp != dockerTimestamp(at) {
		t.Fatalf("두 주기 내리 그대로면 unparsed 한 줄로 지나간다: %d", unparsed)
	}
}

func TestFrameLimitSplitsIntoMoreEntriesInTheSameCycle(t *testing.T) {
	defer failOnPanic(t)
	harness := startedHarness(t, nginx)
	lines := accessLines(6000, lineBaseMillis, nil)
	for index, line := range lines {
		harness.docker.writeRaw(nginx, trafficWallStart.Add(5*time.Second+time.Duration(index)*time.Microsecond).UnixNano(), line+"\n")
	}
	harness.advance(30 * time.Second)
	harness.cycle()
	items := harness.stream.items()[1:]
	if len(items) != 2 || items[0].field("seq") != "2" || items[1].field("seq") != "3" {
		t.Fatalf("프레임 5,000개에서 끊고 같은 주기에 다음 순번이 잇는다: %d항목", len(items))
	}
	if first := decodeTrafficBody(t, items[0]).source(t, nginx).Lines; first != 5000 {
		t.Fatalf("첫 항목은 프레임 5,000개: %d", first)
	}
	if lines := totalLines(t, items); lines != 6000 {
		t.Fatalf("합 %d", lines)
	}
}

func TestBodyByteLimitStopsTheRead(t *testing.T) {
	defer failOnPanic(t)
	harness := startedHarness(t, nginx)
	for index := range 2100 { // 4KB 줄 2,100개 ≈ 8.6MB — 5,000 프레임보다 8MB 가 먼저
		harness.docker.writeRaw(nginx, trafficWallStart.Add(5*time.Second+time.Duration(index)*time.Microsecond).UnixNano(), strings.Repeat("y", 4000)+"\n")
	}
	harness.advance(30 * time.Second)
	harness.cycle()
	items := harness.stream.items()[1:]
	if len(items) < 2 {
		t.Fatalf("8MB 에서 끊고 다음 순번이 잇는다: %d항목", len(items))
	}
	first := decodeTrafficBody(t, items[0]).source(t, nginx)
	if first.Ignored >= 2100 || first.Ignored*4040 > 8<<20+4040 { // 프레임 하나 = 머리 8 + 시각 31 + 4,001바이트
		t.Fatalf("첫 항목이 8MB 를 넘게 읽었다: %d줄", first.Ignored)
	}
	ignored := int64(0)
	for _, item := range items {
		ignored += decodeTrafficBody(t, item).source(t, nginx).Ignored
	}
	if ignored != 2100 {
		t.Fatalf("합 %d", ignored)
	}
}

func TestSameTimestampGroupOver256FramesIsCutAndSwallowed(t *testing.T) {
	defer failOnPanic(t)
	harness := startedHarness(t, nginx)
	at := trafficWallStart.Add(5 * time.Second).UnixNano()
	harness.docker.writeRaw(nginx, at, strings.Repeat("z", 300*16384)+"\n") // 프레임 300개의 한 줄(≈4.7MB)
	harness.docker.write(nginx, trafficWallStart.Add(7*time.Second), accessLines(1, lineBaseMillis, nil)...)
	for range 2 {
		harness.advance(30 * time.Second)
		harness.cycle()
	}
	unparsed, lines := int64(0), int64(0)
	for _, item := range harness.stream.items() {
		for _, source := range decodeTrafficBody(t, item).Sources {
			unparsed, lines = unparsed+source.Unparsed, lines+source.Lines
		}
	}
	if unparsed != 1 || lines != 1 {
		t.Fatalf("256 프레임에서 끊은 줄은 unparsed 하나 · 나머지 조각은 버리고 다음 줄은 센다: unparsed %d lines %d", unparsed, lines)
	}
	cut := decodeTrafficBody(t, harness.item(1)).source(t, nginx)
	if cut.To != (trafficPosition{dockerTimestamp(at), 256}) {
		t.Fatalf("to = (그 시각, 256): %+v", cut.To)
	}
}

func TestReadDeadlineAndCycleDeadline(t *testing.T) {
	defer failOnPanic(t)
	harness := newTrafficHarness(t, "a-nginx", "b-nginx", "c-nginx")
	harness.collector.readDeadline, harness.collector.cycleDeadline = 250*time.Millisecond, 400*time.Millisecond
	harness.cycle()
	for _, name := range []string{"a-nginx", "b-nginx", "c-nginx"} {
		harness.docker.write(name, trafficWallStart.Add(5*time.Second), accessLines(1, lineBaseMillis, nil)...)
	}
	harness.docker.set("a-nginx", func(container *fakeLogContainer) { container.stallLogs = true })
	harness.docker.set("b-nginx", func(container *fakeLogContainer) { container.stallLogs = true })
	harness.advance(30 * time.Second)
	started := time.Now()
	harness.cycle()
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("기한을 지키지 않았다: %v", elapsed)
	}
	body := decodeTrafficBody(t, harness.item(1))
	if strings.Join(body.Missing, ",") != "a-nginx:unreadable" {
		t.Fatalf("읽기 기한을 넘긴 a 만 unreadable — 주기 기한에 걸린 b · c 는 빈 범위: %v", body.Missing)
	}
	for _, name := range []string{"b-nginx", "c-nginx"} {
		if source := body.source(t, name); source.From != source.To || source.Lines != 0 {
			t.Fatalf("%s 는 빈 범위: %+v", name, source)
		}
	}
	for _, source := range body.Sources {
		if source.Container == "a-nginx" {
			t.Fatal("unreadable 컨테이너는 sources 에서 뺀다")
		}
	}
}

func TestLargeResponseIsStreamedNotLoadedWhole(t *testing.T) {
	defer failOnPanic(t)
	harness := startedHarness(t, nginx)
	for index := range 30000 { // 1KB 줄 3만 개 ≈ 31MB
		harness.docker.writeRaw(nginx, trafficWallStart.Add(5*time.Second+time.Duration(index)*time.Microsecond).UnixNano(), strings.Repeat("w", 1000)+"\n")
	}
	harness.advance(30 * time.Second)
	harness.cycle()
	var first logsRequest
	for _, request := range harness.docker.logRequests() {
		if request.framesWritten > 0 {
			first = request
			break
		}
	}
	if first.framesWritten >= 20000 || !first.clientClosedEarly {
		t.Fatalf("5,000 프레임에서 멈추고 연결을 끊는다 — 가짜 도커가 쓴 프레임 %d / 30000 · 끊김 %v", first.framesWritten, first.clientClosedEarly)
	}
	if ignored := decodeTrafficBody(t, harness.item(1)).source(t, nginx).Ignored; ignored != 5000 {
		t.Fatalf("첫 항목 5,000줄: %d", ignored)
	}
}
