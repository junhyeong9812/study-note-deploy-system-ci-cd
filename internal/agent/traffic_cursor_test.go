package agent

// E. 먼저 적고 보낸다 — 기준: 규약 §4.4 · requirement-spec.md(traffic-map) "T5 착수 — 먼저 적고 보낸다" · "선검증 반영" 1 · 9.
//
// 정한 계약 — 읽은 자리의 파일(TRAFFIC_CURSOR_PATH, JSON):
//   {"version":1,"generation":"<소문자 16진 16자>",
//    "containers":[{"name","id"(64자 — 아직 모르면 ""),"position":{"docker_ts":"<RFC3339 9자리>Z","skip":<정수>},"anchored":<참 · 거짓>}](이름 순),
//    "last":{"seq":<정수>,"at":<epoch ms>,"state":"written"|"sent",
//            "sources":[{"container","id","from":{…},"to":{…},"anchored"}],   // sent 로 바꿀 때 자리를 옮길 값
//            "fields":[{"name":"kind","value":"traffic"},{"name":"v",…},{"name":"server",…},{"name":"generation",…},{"name":"seq",…},{"name":"at",…},{"name":"body",…}]}}
//   - 차례: 범위를 정하고 항목을 다 만든 뒤 state written 으로 적는다 → XADD traffic MAXLEN ~ 5000 → 성공하면 state sent · 자리를 옮겨 적는다.
//   - written 이 남아 있으면 도커를 부르지 않고 fields 의 글자 그대로 다시 보낸다(주기마다 한 번 — 즉시 재시도 · 백오프 없음). 그 주기에 새 항목은 없다.
//   - XADD 가 실패하면 자리를 옮기지 않는다. sent 를 적지 못하면(죽음) 다시 시작한 수집기가 같은 순번을 같은 바이트로 다시 보낸다.
//   - 파일이 없거나 · JSON 이 아니거나 · 꼴이 틀리거나 · version 이 1 이 아니면: 새 세대 · seq 1 · 컨테이너마다 지금부터 · missing 에 <이름>:cursor_lost.
//     있던 파일은 <경로>.corrupt-<그때의 epoch ms> 로 옮긴다(없던 경우는 옮길 것이 없다).
//   - 쓰기: writeCursorAtomically — 같은 폴더의 임시 파일 → fsync → 이름 바꾸기 → 부모 폴더 fsync. 임시 파일은 남기지 않는다.

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestCursorFileRecordsGenerationPositionsAndLastFields(t *testing.T) {
	defer failOnPanic(t)
	harness := startedHarness(t, nginx, "front-nginx")
	harness.docker.write(nginx, trafficWallStart.Add(5*time.Second), accessLines(2, lineBaseMillis, nil)...)
	harness.advance(30 * time.Second)
	harness.cycle()
	file := harness.cursorFile()
	item := harness.item(1)
	if file.Version != 1 || !regexp.MustCompile(`^[0-9a-f]{16}$`).MatchString(file.Generation) || file.Generation != item.field("generation") {
		t.Fatalf("판 · 세대: %+v", file)
	}
	if len(file.Containers) != 2 || file.Containers[0].Name != nginx || file.Containers[0].ID != trafficContainerID(nginx, 0) || !file.Containers[0].Anchored ||
		file.Containers[0].Position.DockerTimestamp != dockerTimestamp(trafficWallStart.Add(5*time.Second+time.Microsecond).UnixNano()) || file.Containers[0].Position.Skip != 1 {
		t.Fatalf("컨테이너마다 이름 · Id · 다음 읽을 자리 · anchored: %+v", file.Containers)
	}
	if file.Containers[1].Anchored {
		t.Fatalf("줄을 읽지 않은 컨테이너의 자리는 anchored 가 아니다: %+v", file.Containers[1])
	}
	if file.Last == nil || file.Last.State != "sent" || strconv.FormatInt(file.Last.Seq, 10) != item.field("seq") ||
		strconv.FormatInt(file.Last.At, 10) != item.field("at") || len(file.Last.Fields) != 7 {
		t.Fatalf("마지막 항목: %+v", file.Last)
	}
	for index, field := range file.Last.Fields {
		if field.Name != item.fields[index].Name || field.Value != item.fields[index].Value {
			t.Fatalf("파일의 스트림 칸이 보낸 것과 글자까지 같다: %d번째 %s", index, field.Name)
		}
	}
	if names := []string{item.fields[0].Name, item.fields[1].Name, item.fields[2].Name, item.fields[3].Name, item.fields[4].Name, item.fields[5].Name, item.fields[6].Name}; strings.Join(names, ",") != "kind,v,server,generation,seq,at,body" {
		t.Fatalf("스트림 칸의 차례: %v", names)
	}
	if item.stream != "traffic" || item.maxLength != 5000 || item.field("server") != trafficServer {
		t.Fatalf("XADD traffic MAXLEN ~ 5000: %s %d", item.stream, item.maxLength)
	}
	syncs := len(harness.directorySyncs)
	if syncs == 0 || harness.directorySyncs[0] != filepath.Dir(harness.settings.CursorPath) {
		t.Fatalf("부모 폴더를 fsync 한다: %v", harness.directorySyncs)
	}
}

func TestWriteCursorAtomicallyRenamesBeforeSyncingDirectory(t *testing.T) {
	defer failOnPanic(t)
	directory := t.TempDir()
	path := filepath.Join(directory, "traffic-cursor.json")
	os.WriteFile(path, []byte(`{"old":true}`), 0o600)
	synced := false
	err := writeCursorAtomically(path, []byte(`{"new":true}`), func(syncedDirectory string) error {
		content, _ := os.ReadFile(path)
		entries, _ := os.ReadDir(directory)
		if syncedDirectory != directory || string(content) != `{"new":true}` || len(entries) != 1 {
			t.Fatalf("폴더를 fsync 할 때는 이미 이름을 바꿨고 임시 파일이 없다: %s %q %d", syncedDirectory, content, len(entries))
		}
		synced = true
		return nil
	})
	if err != nil || !synced {
		t.Fatalf("쓰기: %v · 폴더 fsync %v", err, synced)
	}
	if err := writeCursorAtomically(path, []byte(`{}`), func(string) error { return errors.New("fsync 실패") }); err == nil {
		t.Fatal("폴더 fsync 가 실패하면 오류")
	}
	if err := writeCursorAtomically(filepath.Join(directory, "missing", "cursor.json"), []byte(`{}`), func(string) error { return nil }); err == nil {
		t.Fatal("폴더가 없으면 오류")
	}
}

func TestWrittenEntryIsResentVerbatimWithoutDocker(t *testing.T) {
	defer failOnPanic(t)
	harness := startedHarness(t, nginx)
	harness.docker.write(nginx, trafficWallStart.Add(5*time.Second), accessLines(3, lineBaseMillis, nil)...)
	before := harness.cursorFile().Containers
	harness.stream.set(func(stream *fakeTrafficStream) { stream.failNext = 2 })
	harness.advance(30 * time.Second)
	harness.cycle()
	file := harness.cursorFile()
	if file.Last.State != "written" || file.Containers[0].Position != before[0].Position {
		t.Fatalf("XADD 가 실패하면 written 그대로 · 자리를 옮기지 않는다: %+v", file)
	}
	failed := harness.stream.attempts[len(harness.stream.attempts)-1]
	harness.docker.write(nginx, trafficWallStart.Add(40*time.Second), accessLines(2, lineBaseMillis+100, nil)...)
	calls := harness.docker.callCount()
	attempts := len(harness.stream.attempts)
	harness.advance(30 * time.Second)
	harness.cycle() // 두 번째 실패 — 한 주기에 한 번만 다시 보낸다
	if len(harness.stream.attempts) != attempts+1 || harness.docker.callCount() != calls {
		t.Fatalf("주기마다 한 번 · 도커를 부르지 않는다: 시도 %d → %d · 도커 %d → %d", attempts, len(harness.stream.attempts), calls, harness.docker.callCount())
	}
	harness.advance(30 * time.Second)
	harness.cycle() // 성공
	if harness.docker.callCount() != calls {
		t.Fatal("다시 보내는 주기에 도커를 불렀다")
	}
	resent := harness.item(1)
	for index := range failed.fields {
		if resent.fields[index] != failed.fields[index] {
			t.Fatalf("다시 보낸 칸이 처음 것과 바이트까지 같다: %s", failed.fields[index].Name)
		}
	}
	if harness.cursorFile().Last.State != "sent" || decodeTrafficBody(t, resent).source(t, nginx).Lines != 3 {
		t.Fatal("성공하면 sent")
	}
	harness.advance(30 * time.Second)
	harness.cycle()
	if items := harness.stream.items(); len(items) != 3 || items[2].field("seq") != "3" || decodeTrafficBody(t, items[2]).source(t, nginx).Lines != 2 {
		t.Fatalf("그다음 주기가 새 줄을 다음 순번으로: %d", len(items))
	}
}

// crashScenario — 죽음을 흉내 낸 뒤 다시 시작한 수집기로 주기를 돌려, 보낸 항목들을 순번마다 모은다.
func distinctBySequence(t *testing.T, items []trafficItem) map[string]trafficItem {
	t.Helper()
	bySequence := map[string]trafficItem{}
	for _, item := range items {
		if earlier, seen := bySequence[item.field("seq")]; seen {
			for index := range item.fields {
				if earlier.fields[index] != item.fields[index] {
					t.Fatalf("같은 순번 %s 가 다른 바이트로 갔다(%s)", item.field("seq"), item.fields[index].Name)
				}
			}
		}
		bySequence[item.field("seq")] = item
	}
	for sequence := 1; sequence <= len(bySequence); sequence++ {
		if _, present := bySequence[strconv.Itoa(sequence)]; !present {
			t.Fatalf("순번 %d 가 비었다: %v", sequence, len(bySequence))
		}
	}
	return bySequence
}

func TestCrashesAtEachStepNeitherDuplicateNorLoseLines(t *testing.T) {
	defer failOnPanic(t)
	for _, step := range []string{"범위를 적기 전", "written 뒤 XADD 전", "XADD 뒤 sent 전"} {
		t.Run(step, func(t *testing.T) {
			defer failOnPanic(t)
			harness := startedHarness(t, nginx)
			harness.docker.write(nginx, trafficWallStart.Add(5*time.Second), accessLines(4, lineBaseMillis, nil)...)
			harness.advance(30 * time.Second)
			switch step {
			case "범위를 적기 전":
				harness.setCursorWriteFault(func(content []byte) error {
					if strings.Contains(string(content), `"state":"written"`) {
						return errors.New("죽음")
					}
					return nil
				})
			case "written 뒤 XADD 전":
				harness.stream.set(func(stream *fakeTrafficStream) { stream.failNext = 1 })
			case "XADD 뒤 sent 전":
				harness.setCursorWriteFault(func(content []byte) error {
					if strings.Contains(string(content), `"state":"sent"`) {
						return errors.New("죽음")
					}
					return nil
				})
			}
			harness.cycle()
			harness.setCursorWriteFault(nil)
			harness.restart()
			harness.docker.write(nginx, trafficWallStart.Add(35*time.Second), accessLines(3, lineBaseMillis+100, nil)...)
			for range 3 {
				harness.advance(30 * time.Second)
				harness.cycle()
			}
			distinct := distinctBySequence(t, harness.stream.items())
			total := int64(0)
			for _, item := range distinct {
				total += decodeTrafficBody(t, item).source(t, nginx).Lines
				requireObserveAccepts(t, item)
			}
			if total != 7 {
				t.Fatalf("순번마다 한 번 센 줄의 합 %d ≠ 7", total)
			}
		})
	}
}

func TestLostCursorStartsNewGenerationFromNow(t *testing.T) {
	defer failOnPanic(t)
	for _, content := range []string{"", "not json", `{"version":2,"generation":"0123456789abcdef","containers":[],"last":null}`,
		`{"version":1,"generation":"XYZ","containers":[],"last":null}`} {
		harness := newTrafficHarness(t, nginx)
		if content != "" {
			os.WriteFile(harness.settings.CursorPath, []byte(content), 0o600)
		}
		harness.docker.write(nginx, trafficWallStart.Add(-5*time.Second), accessLines(2, lineBaseMillis, nil)...)
		harness.cycle()
		harness.advance(30 * time.Second)
		harness.cycle()
		items := harness.stream.items()
		first := decodeTrafficBody(t, items[0])
		if items[0].field("seq") != "1" || items[0].field("generation") != "00000000a0000001" || strings.Join(first.Missing, ",") != nginx+":cursor_lost" {
			t.Fatalf("%q: 새 세대 · 순번 1 · cursor_lost: %v %v", content, items[0].fields[:5], first.Missing)
		}
		if totalLines(t, items) != 0 {
			t.Fatalf("%q: 지금부터 — 앞의 줄을 읽었다", content)
		}
		corrupt, _ := filepath.Glob(harness.settings.CursorPath + ".corrupt-*")
		if content == "" && len(corrupt) != 0 {
			t.Fatal("없던 파일은 옮길 것이 없다")
		}
		if content != "" {
			if len(corrupt) != 1 || !regexp.MustCompile(`\.corrupt-[0-9]{13}$`).MatchString(corrupt[0]) {
				t.Fatalf("%q: 틀린 파일을 .corrupt-<epoch ms> 로 옮긴다: %v", content, corrupt)
			}
			if kept, _ := os.ReadFile(corrupt[0]); string(kept) != content {
				t.Fatalf("옮긴 파일의 내용이 그대로: %q", kept)
			}
		}
	}
}

func TestDeletedCursorFileGetsANewGeneration(t *testing.T) {
	defer failOnPanic(t)
	harness := startedHarness(t, nginx)
	firstGeneration := harness.item(0).field("generation")
	os.Remove(harness.settings.CursorPath)
	harness.restart()
	harness.advance(30 * time.Second)
	harness.cycle()
	item := harness.item(1)
	if item.field("generation") == firstGeneration || item.field("seq") != "1" {
		t.Fatalf("파일을 잃으면 새 세대의 순번 1: %v", item.fields[:5])
	}
}
