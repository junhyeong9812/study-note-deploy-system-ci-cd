package agent

// D. 범위를 한 번에 정한다 — 기준: requirement-spec.md(traffic-map) "선검증 반영" 5 · 6 · 10 · 11.
//
// 정한 계약:
//   - 컨테이너를 이름 순으로 읽으며 줄을 더한다. 항목의 묶음(segments)이 60개를 넘게 되는 줄 앞에서 그 컨테이너의 범위를 끊고,
//     그 뒤 컨테이너는 그 항목에서 빈 범위(from = to). 다음 순번이 끊긴 자리부터 같은 주기 안에서 잇는다 — 주기당 항목 3개까지, 남으면 다음 주기.
//   - 빈 항목도 순번을 쓴다(주기마다 하나).
//   - 줄이 없는 항목(가장 긴 이름 16개 · 모두 unreadable)이 28KB(28,672바이트) 아래.
//   - 한 항목의 줄은 프레임 5,000 × 컨테이너 16 = 80,000 을 넘지 않는다(관측 서버의 세는 수 상한 10억보다 훨씬 작다).

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"
)

// distinctCellLines — 줄마다 다른 30초 칸(묶음 하나 = 줄 하나).
func distinctCellLines(count int, startCell int64) []string {
	lines := make([]string, count)
	for index := range lines {
		lines[index] = defaultAccess().with(func(line *accessLine) {
			line.ts = fmt.Sprintf("%d.000", 1764201600+(startCell+int64(index))*30)
			line.requestID = fmt.Sprintf("req-cell-%04d", startCell+int64(index))
		}).text()
	}
	return lines
}

func writeLines(harness *trafficHarness, name string, offset time.Duration, lines []string) {
	for index, line := range lines {
		harness.docker.writeRaw(name, trafficWallStart.Add(offset+time.Duration(index)*time.Millisecond).UnixNano(), line+"\n")
	}
}

func TestSegmentLimitCutsTheRangeAndLaterContainersWait(t *testing.T) {
	defer failOnPanic(t)
	harness := startedHarness(t, "b-nginx", "a-nginx") // 설정의 차례가 아니라 이름 순으로 읽는다
	writeLines(harness, "a-nginx", 5*time.Second, distinctCellLines(70, 0))
	writeLines(harness, "b-nginx", 5*time.Second, distinctCellLines(5, 0)) // 첫 항목에 이미 있는 묶음 — 읽었다면 묶음을 늘리지 않고 실렸을 줄
	harness.advance(30 * time.Second)
	harness.cycle()
	items := harness.stream.items()[1:]
	if len(items) != 2 {
		t.Fatalf("끊긴 자리를 같은 주기에 다음 순번이 잇는다: %d항목", len(items))
	}
	first, second := decodeTrafficBody(t, items[0]), decodeTrafficBody(t, items[1])
	if len(first.Segments) != 60 || first.source(t, "a-nginx").Lines != 60 {
		t.Fatalf("묶음 60개에서 끊는다: %d묶음 · a %d줄", len(first.Segments), first.source(t, "a-nginx").Lines)
	}
	if source := first.source(t, "b-nginx"); source.From != source.To || source.Lines != 0 {
		t.Fatalf("끊긴 뒤의 컨테이너는 빈 범위: %+v", source)
	}
	if first.Sources[0].Container != "a-nginx" {
		t.Fatalf("sources 는 이름 순: %+v", first.Sources)
	}
	if second.source(t, "a-nginx").Lines != 10 || second.source(t, "b-nginx").Lines != 5 || second.source(t, "a-nginx").From != first.source(t, "a-nginx").To {
		t.Fatalf("다음 순번이 끊긴 자리부터: %+v", second.Sources)
	}
	if containers := harness.cursorFile().Containers; len(containers) != 2 || containers[0].Name != "a-nginx" || containers[1].Name != "b-nginx" {
		t.Fatalf("파일의 컨테이너도 이름 순: %+v", harness.cursorFile().Containers)
	}
	for _, item := range items {
		requireObserveAccepts(t, item)
	}
}

func TestAtMostThreeEntriesPerCycle(t *testing.T) {
	defer failOnPanic(t)
	harness := startedHarness(t, nginx)
	writeLines(harness, nginx, 5*time.Second, distinctCellLines(200, 0))
	harness.advance(30 * time.Second)
	harness.cycle()
	if items := harness.stream.items()[1:]; len(items) != 3 {
		t.Fatalf("주기당 항목 3개까지: %d", len(items))
	}
	harness.advance(30 * time.Second)
	harness.cycle()
	items := harness.stream.items()[1:]
	if len(items) != 4 || decodeTrafficBody(t, items[3]).source(t, nginx).Lines != 20 || totalLines(t, items) != 200 {
		t.Fatalf("남은 줄은 다음 주기: %d항목 · 합 %d", len(items), totalLines(t, items))
	}
	for index, item := range harness.stream.items() {
		if item.field("seq") != strconv.Itoa(index+1) {
			t.Fatalf("순번이 이어진다: %d번째 %s", index, item.field("seq"))
		}
	}
}

func TestEmptyEntriesStillUseSequenceNumbers(t *testing.T) {
	defer failOnPanic(t)
	harness := startedHarness(t, nginx)
	for range 3 {
		harness.advance(30 * time.Second)
		harness.cycle()
	}
	items := harness.stream.items()
	if len(items) != 4 || items[3].field("seq") != "4" || totalLines(t, items) != 0 {
		t.Fatalf("줄이 없어도 주기마다 빈 항목 하나: %d", len(items))
	}
	for _, item := range items {
		requireObserveAccepts(t, item)
	}
}

func TestLongestNamesAllUnreadableStayUnder28Kilobytes(t *testing.T) {
	defer failOnPanic(t)
	names := make([]string, 16)
	for index := range names {
		names[index] = strings.Repeat("n", 126) + fmt.Sprintf("%02d", index) // 128자
	}
	harness := newTrafficHarness(t)
	harness.settings = trafficSettingsFor(harness.settings.CursorPath, names...)
	harness.restart() // 도커에 없는 이름 — 모두 unreadable
	harness.cycle()
	harness.advance(30 * time.Second)
	harness.cycle()
	for _, item := range harness.stream.items() {
		if size := len(item.field("body")); size >= 28*1024 {
			t.Fatalf("줄이 없는 항목이 28KB 를 넘었다: %d", size)
		}
		requireObserveAccepts(t, item)
	}
	if missing := decodeTrafficBody(t, harness.item(1)).Missing; len(missing) != 16 || !strings.HasSuffix(missing[0], ":unreadable") {
		t.Fatalf("16개 모두 unreadable: %v", missing)
	}
}

func TestOneEntryNeverCarriesMoreThan80000Lines(t *testing.T) {
	defer failOnPanic(t)
	if 5000*16 != 80000 || 80000 > 1_000_000_000 {
		t.Fatal("상수의 계산")
	}
	harness := startedHarness(t, "a-nginx", "b-nginx")
	for _, name := range []string{"a-nginx", "b-nginx"} {
		writeLines(harness, name, 5*time.Second, accessLines(6000, lineBaseMillis, nil))
	}
	harness.advance(30 * time.Second)
	harness.cycle()
	for _, item := range harness.stream.items() {
		for _, source := range decodeTrafficBody(t, item).Sources {
			if source.Lines+source.Unparsed+source.Ignored > 5000 {
				t.Fatalf("컨테이너 하나가 한 항목에 5,000줄을 넘었다: %+v", source)
			}
		}
	}
	if lines := totalLines(t, harness.stream.items()); lines != 12000 {
		t.Fatalf("합 %d", lines)
	}
}
