package agent

// F. 경우 — 기준: 규약 §4.4 의 경우표 · requirement-spec.md(traffic-map) "T5 착수 — 먼저 적고 보낸다"의 경우 · "선검증 반영" 2 · 7 · 9.
//
// 정한 계약:
//   - replaced: 이름의 Id 가 파일의 Id 와 다르면 since=0("0.000000000")부터 읽고(자리 (1970-01-01T00:00:00.000000000Z, 0) · anchored 아님)
//     missing 에 <이름>:replaced. sources 의 container_id 는 새 Id 의 앞 12자.
//   - rotated: 자리가 anchored 일 때만 — 자리의 시각을 가진 프레임이 skip 개보다 적게 오거나 · 첫 프레임이 자리보다 뒤거나 · 빈 응답.
//     받은 프레임을 처음부터 다 세고(건너뛰지 않는다) sources.from = 실제로 읽기 시작한 자리((첫 프레임의 시각, 0) — 빈 응답이면 자리 그대로),
//     missing 에 <이름>:rotated. 빈 응답이었으면 그 뒤 자리는 anchored 가 아니다.
//   - anchored 가 아닌 자리(지금부터 · since=0)에서 더 뒤의 줄부터 와도 rotated 가 아니다.
//   - unreadable: Id 확인의 404 · 오류 · /logs 의 404 · 500 · 501 · 기한 · 머리가 깨진 프레임(흐름이 1 이 아니거나 둘째~넷째 바이트가 0 이 아님) ·
//     프레임 가운데서 끊김 · 시각을 읽을 수 없는 프레임 → 그 항목에서 그 컨테이너를 빼고(sources 에 없음) 자리를 움직이지 않는다 · <이름>:unreadable.
//   - clock_behind: 자리의 시각이 지금 − 2초보다 뒤면 읽지 않는다(sources 에 없음 · 자리 그대로) · <이름>:clock_behind · 헬스 clock_behind 를 센다.
//   - 받은 프레임의 시각이 앞 프레임보다 작으면 그 주기 그 컨테이너는 unreadable · 헬스 time_reversals 를 센다.
//   - at = 그 항목의 범위를 다 읽은 때(마지막 읽기를 마친 뒤 · body 를 만들기 직전)의 벽시계 epoch ms.

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestReplacedContainerIsReadFromTheStart(t *testing.T) {
	defer failOnPanic(t)
	harness := startedHarness(t, nginx)
	harness.docker.write(nginx, trafficWallStart.Add(5*time.Second), accessLines(2, lineBaseMillis, nil)...)
	harness.advance(30 * time.Second)
	harness.cycle()
	harness.docker.recreate(nginx, 1)
	harness.docker.write(nginx, trafficWallStart.Add(-time.Hour), accessLines(3, lineBaseMillis+10, nil)...) // 새 컨테이너의 줄 — 시각이 앞이어도 읽는다
	harness.advance(30 * time.Second)
	harness.cycle()
	requests := harness.docker.logRequests()
	if last := requests[len(requests)-1]; last.query["since"] != "0.000000000" || last.path != "/containers/"+trafficContainerID(nginx, 1)+"/logs" {
		t.Fatalf("새 컨테이너는 처음부터 새 Id 로: %s %v", last.path, last.query)
	}
	body := decodeTrafficBody(t, harness.item(2))
	source := body.source(t, nginx)
	if strings.Join(body.Missing, ",") != nginx+":replaced" || source.Lines != 3 || source.ContainerID != trafficContainerID(nginx, 1)[:12] ||
		source.From != (trafficPosition{"1970-01-01T00:00:00.000000000Z", 0}) {
		t.Fatalf("replaced · 새 컨테이너의 줄 전부: %v %+v", body.Missing, source)
	}
	if file := harness.cursorFile(); file.Containers[0].ID != trafficContainerID(nginx, 1) {
		t.Fatalf("파일의 Id 를 바꾼다: %+v", file.Containers[0])
	}
}

func TestRotationIsDetectedOnlyForAnchoredPositions(t *testing.T) {
	defer failOnPanic(t)
	harness := startedHarness(t, nginx)
	harness.docker.write(nginx, trafficWallStart.Add(5*time.Second), accessLines(2, lineBaseMillis, nil)...)
	harness.advance(30 * time.Second)
	harness.cycle() // 자리 = 읽은 마지막 줄(anchored)
	harness.docker.write(nginx, trafficWallStart.Add(40*time.Second), accessLines(3, lineBaseMillis+10, nil)...)
	harness.docker.rotate(nginx, trafficWallStart.Add(38*time.Second)) // 자리의 줄까지 사라졌다
	harness.advance(30 * time.Second)
	harness.cycle()
	body := decodeTrafficBody(t, harness.item(2))
	source := body.source(t, nginx)
	if strings.Join(body.Missing, ",") != nginx+":rotated" || source.Lines != 3 ||
		source.From != (trafficPosition{dockerTimestamp(trafficWallStart.Add(40 * time.Second).UnixNano()), 0}) {
		t.Fatalf("자리보다 뒤의 줄부터 오면 rotated · from = 실제 시작: %v %+v", body.Missing, source)
	}
}

func TestRotationWhenFewerFramesAtThePositionTimestamp(t *testing.T) {
	defer failOnPanic(t)
	harness := startedHarness(t, nginx)
	at := trafficWallStart.Add(5 * time.Second).UnixNano()
	for _, line := range accessLines(3, lineBaseMillis, nil) {
		harness.docker.writeRaw(nginx, at, line+"\n")
	}
	harness.advance(30 * time.Second)
	harness.cycle() // 자리 (at, 3)
	harness.docker.set(nginx, func(container *fakeLogContainer) { container.lines = container.lines[2:] })
	harness.advance(30 * time.Second)
	harness.cycle()
	if body := decodeTrafficBody(t, harness.item(2)); strings.Join(body.Missing, ",") != nginx+":rotated" {
		t.Fatalf("자리의 시각을 가진 프레임이 skip 보다 적으면 rotated: %v", body.Missing)
	}
}

func TestEmptyResponseAtAnchoredPositionIsRotationOnce(t *testing.T) {
	defer failOnPanic(t)
	harness := startedHarness(t, nginx)
	harness.docker.write(nginx, trafficWallStart.Add(5*time.Second), accessLines(1, lineBaseMillis, nil)...)
	harness.advance(30 * time.Second)
	harness.cycle()
	harness.docker.rotate(nginx, trafficWallStart.Add(time.Hour)) // 다 사라졌다
	harness.advance(30 * time.Second)
	harness.cycle()
	harness.advance(30 * time.Second)
	harness.cycle()
	items := harness.stream.items()
	if strings.Join(decodeTrafficBody(t, items[2]).Missing, ",") != nginx+":rotated" || len(decodeTrafficBody(t, items[3]).Missing) != 0 {
		t.Fatalf("빈 응답인데 anchored 면 rotated — 그 뒤 자리는 anchored 가 아니라 다시 세지 않는다: %v %v",
			decodeTrafficBody(t, items[2]).Missing, decodeTrafficBody(t, items[3]).Missing)
	}
	if harness.cursorFile().Containers[0].Anchored {
		t.Fatal("빈 응답으로 rotated 를 본 뒤의 자리는 anchored 가 아니다")
	}
}

func TestUnanchoredPositionsAreNotRotation(t *testing.T) {
	defer failOnPanic(t)
	harness := startedHarness(t, nginx) // 지금부터 — anchored 아님
	harness.docker.write(nginx, trafficWallStart.Add(20*time.Second), accessLines(2, lineBaseMillis, nil)...)
	harness.advance(30 * time.Second)
	harness.cycle()
	if body := decodeTrafficBody(t, harness.item(1)); len(body.Missing) != 0 || body.source(t, nginx).Lines != 2 {
		t.Fatalf("지금부터의 자리보다 뒤의 줄은 rotated 가 아니다: %v", body.Missing)
	}
	harness.docker.recreate(nginx, 1)
	harness.advance(30 * time.Second)
	harness.cycle()
	harness.docker.write(nginx, trafficWallStart.Add(85*time.Second), accessLines(1, lineBaseMillis+5, nil)...)
	harness.advance(30 * time.Second)
	harness.cycle()
	if body := decodeTrafficBody(t, harness.item(3)); len(body.Missing) != 0 || body.source(t, nginx).Lines != 1 {
		t.Fatalf("처음부터(since=0)의 자리 뒤의 줄도 rotated 가 아니다: %v", body.Missing)
	}
}

func TestUnreadableContainersKeepTheirPosition(t *testing.T) {
	defer failOnPanic(t)
	cases := map[string]func(container *fakeLogContainer){
		"Id 확인 404":  func(container *fakeLogContainer) { container.inspectFail = 404 },
		"Id 확인 500":  func(container *fakeLogContainer) { container.inspectFail = 500 },
		"logs 404":   func(container *fakeLogContainer) { container.logsStatus = 404 },
		"logs 500":   func(container *fakeLogContainer) { container.logsStatus = 500 },
		"logs 501":   func(container *fakeLogContainer) { container.logsStatus = 501 },
		"기한":         func(container *fakeLogContainer) { container.stallLogs = true },
		"깨진 머리":      func(container *fakeLogContainer) { container.brokenHeader = true },
		"프레임 가운데 끊김": func(container *fakeLogContainer) { container.cutMidFrame = true },
	}
	for name, breakIt := range cases {
		t.Run(name, func(t *testing.T) {
			defer failOnPanic(t)
			harness := startedHarness(t, nginx, "front-nginx")
			harness.collector.readDeadline = 300 * time.Millisecond
			harness.docker.write(nginx, trafficWallStart.Add(5*time.Second), accessLines(2, lineBaseMillis, nil)...)
			harness.docker.write("front-nginx", trafficWallStart.Add(5*time.Second), accessLines(1, lineBaseMillis+5, nil)...)
			before := harness.cursorFile().Containers[0].Position
			harness.docker.set(nginx, breakIt)
			harness.advance(30 * time.Second)
			harness.cycle()
			body := decodeTrafficBody(t, harness.item(1))
			if strings.Join(body.Missing, ",") != nginx+":unreadable" || len(body.Sources) != 1 || body.Sources[0].Container != "front-nginx" {
				t.Fatalf("읽지 못한 컨테이너는 빼고 unreadable: %v %+v", body.Missing, body.Sources)
			}
			if position := harness.cursorFile().Containers[0].Position; position != before {
				t.Fatalf("읽지 못한 컨테이너의 자리는 그대로: %+v → %+v", before, position)
			}
			harness.docker.set(nginx, func(container *fakeLogContainer) {
				container.inspectFail, container.logsStatus, container.stallLogs, container.brokenHeader, container.cutMidFrame = 0, 0, false, false, false
			})
			harness.advance(30 * time.Second)
			harness.cycle()
			if lines := totalLines(t, harness.stream.items()); lines != 3 {
				t.Fatalf("다음 주기가 같은 자리부터 읽어 잃지 않는다: %d", lines)
			}
		})
	}
}

func TestClockBehindSkipsTheContainer(t *testing.T) {
	defer failOnPanic(t)
	harness := startedHarness(t, nginx)
	harness.docker.write(nginx, trafficWallStart.Add(25*time.Second), accessLines(1, lineBaseMillis, nil)...)
	harness.advance(30 * time.Second)
	harness.cycle() // 자리 = 25초
	calls := len(harness.docker.logRequests())
	harness.clock.shiftWall(-35 * time.Second) // 지금 = −5초 → 지금 − 2초 = −7초 < 자리 25초: 시계가 뒤로 뛰었다
	harness.cycle()
	body := decodeTrafficBody(t, harness.item(2))
	if strings.Join(body.Missing, ",") != nginx+":clock_behind" || len(body.Sources) != 0 || len(harness.docker.logRequests()) != calls {
		t.Fatalf("자리가 지금 − 2초보다 뒤면 읽지 않고 clock_behind: %v %+v", body.Missing, body.Sources)
	}
	if harness.collector.Health().ClockBehind != 1 || harness.cursorFile().Containers[0].Position.DockerTimestamp != dockerTimestamp(trafficWallStart.Add(25*time.Second).UnixNano()) {
		t.Fatal("헬스에 세고 자리는 그대로")
	}
}

func TestFrameTimestampGoingBackwardsMakesItUnreadable(t *testing.T) {
	defer failOnPanic(t)
	harness := startedHarness(t, nginx)
	harness.docker.writeRaw(nginx, trafficWallStart.Add(6*time.Second).UnixNano(), accessLines(1, lineBaseMillis, nil)[0]+"\n")
	harness.docker.writeRaw(nginx, trafficWallStart.Add(5*time.Second).UnixNano(), accessLines(1, lineBaseMillis+1, nil)[0]+"\n")
	harness.advance(30 * time.Second)
	harness.cycle()
	body := decodeTrafficBody(t, harness.item(1))
	if strings.Join(body.Missing, ",") != nginx+":unreadable" || harness.collector.Health().TimeReversals != 1 {
		t.Fatalf("받은 프레임의 시각이 뒤로 가면 그 주기는 unreadable · 헬스에 센다: %v %d", body.Missing, harness.collector.Health().TimeReversals)
	}
}

func TestAtIsTheTimeTheRangeWasRead(t *testing.T) {
	defer failOnPanic(t)
	harness := startedHarness(t, nginx)
	harness.docker.write(nginx, trafficWallStart.Add(5*time.Second), accessLines(1, lineBaseMillis, nil)...)
	harness.advance(30 * time.Second)
	readStarted := harness.clock.wallNow()
	baseline := len(harness.docker.logRequests())
	harness.collector.wallNow = func() time.Time {
		now := harness.clock.wallNow()
		if len(harness.docker.logRequests()) > baseline {
			return now.Add(1500 * time.Millisecond) // 읽기를 마친 뒤의 시각
		}
		return now
	}
	harness.cycle()
	item := harness.item(1)
	if at, _ := strconv.ParseInt(item.field("at"), 10, 64); at != readStarted.Add(1500*time.Millisecond).UnixMilli() {
		t.Fatalf("at = 다 읽은 때: %d (주기 시작 %d)", at, readStarted.UnixMilli())
	}
	requests := harness.docker.logRequests()
	if until := requests[len(requests)-1].query["until"]; until != "1764201628.000000000" {
		t.Fatalf("until 은 읽기 전의 지금 − 2초: %s", until)
	}
}
