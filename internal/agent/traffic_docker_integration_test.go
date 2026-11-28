package agent

// I. 실제 Docker 로 — TRAFFIC_DOCKER_TEST=1 일 때만 돈다(기본 go test ./... 에서는 건너뛴다 — 건너뜀으로 센다).
// 네 이름의 컨테이너(t5-test-a … d · alpine · --log-opt max-size=20k)에 docker exec 로 PID 1 의 표준 출력(/proc/1/fd/1)에 줄을 찍고,
// 실제 도커 소켓을 읽는 수집기(가짜 스트림 · 실제 벽시계 · 임시 폴더의 읽은 자리 파일)를 돌린다.
// 본다: 찍은 줄 수 = 보낸 항목(순번마다 한 번)의 lines 합 · 죽였다 살림 · 돌림(rotated) · 다시 만듦(replaced) · 몰림(5만 줄) · 16KB 넘는 줄.
// 끝나면 컨테이너를 이름으로 지우고(prune 하지 않는다) 0개를 확인한다.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var dockerTestContainers = []string{"t5-test-a", "t5-test-b", "t5-test-c", "t5-test-d"}

func dockerCommand(t *testing.T, input string, arguments ...string) string {
	t.Helper()
	command := exec.Command("docker", arguments...)
	if input != "" {
		command.Stdin = strings.NewReader(input)
	}
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("docker %v: %v\n%s", arguments, err, output)
	}
	return string(output)
}

func startDockerTestContainer(t *testing.T, name, maxSize string) {
	t.Helper()
	exec.Command("docker", "rm", "-f", "-v", name).Run()
	dockerCommand(t, "", "run", "-d", "--name", name, "--log-driver", "json-file", "--log-opt", "max-size="+maxSize, "--log-opt", "max-file=2",
		"alpine:3.20", "sleep", "3600")
}

// printToContainer — 줄들을 그 컨테이너의 PID 1 표준 출력으로(도커 로그에 남는다).
func printToContainer(t *testing.T, name string, lines []string) {
	t.Helper()
	for start := 0; start < len(lines); start += 5000 {
		end := min(start+5000, len(lines))
		dockerCommand(t, strings.Join(lines[start:end], "\n")+"\n", "exec", "-i", name, "sh", "-c", "cat > /proc/1/fd/1")
	}
}

func TestTrafficCollectorWithRealDocker(t *testing.T) {
	defer failOnPanic(t)
	if os.Getenv("TRAFFIC_DOCKER_TEST") != "1" {
		t.Skip("실제 Docker 통합 시험 — TRAFFIC_DOCKER_TEST=1 로 켠다")
	}
	t.Cleanup(func() {
		for _, name := range dockerTestContainers {
			exec.Command("docker", "rm", "-f", "-v", name).Run()
		}
		output, _ := exec.Command("docker", "ps", "-a", "--filter", "name=t5-test-", "--format", "{{.Names}}").Output()
		if strings.TrimSpace(string(output)) != "" {
			t.Errorf("시험 컨테이너가 남았다: %s", output)
		}
	})
	for _, name := range dockerTestContainers {
		maxSize := "20k"
		if name == "t5-test-a" {
			maxSize = "50m" // 몰림 · 이어 읽기의 합을 볼 컨테이너는 돌리지 않는다
		}
		startDockerTestContainer(t, name, maxSize)
	}
	stream := &fakeTrafficStream{}
	cursorPath := filepath.Join(t.TempDir(), "traffic-cursor.json")
	settings := TrafficSettings{RedisAddress: "127.0.0.1:6379", Stream: "traffic", Server: trafficServer, Containers: dockerTestContainers, CursorPath: cursorPath}
	agent := newPipelineAgent(t, &recordedCommands{})
	var fault func(content []byte) error
	newCollector := func() *TrafficCollector {
		collector := agent.newTrafficCollector(settings)
		collector.docker = newDockerAPI(dockerSocketPath)
		collector.stream = stream
		collector.logf = func(string, string) {}
		inner := collector.writeCursor
		collector.writeCursor = func(path string, content []byte) error {
			if fault != nil {
				if err := fault(content); err != nil {
					return err
				}
			}
			return inner(path, content)
		}
		return collector
	}
	collector := newCollector()
	cycle := func() {
		time.Sleep(2500 * time.Millisecond) // until = 지금 − 2초 — 찍은 줄이 범위에 들게
		collector.runCycle(context.Background())
	}
	printed := map[string]int64{}
	printLines := func(name string, count int, base int64) {
		printToContainer(t, name, accessLines(count, base, nil))
		printed[name] += int64(count)
	}
	collector.runCycle(context.Background()) // 지금부터

	printLines("t5-test-a", 300, lineBaseMillis)
	printLines("t5-test-b", 40, lineBaseMillis)
	cycle()

	// 죽였다 살림: written 뒤 · XADD 전에 / XADD 뒤 · sent 전에
	printLines("t5-test-a", 120, lineBaseMillis+1000)
	stream.set(func(stream *fakeTrafficStream) { stream.failNext = 1 })
	cycle()
	collector = newCollector()
	cycle()
	printLines("t5-test-a", 80, lineBaseMillis+2000)
	fault = func(content []byte) error {
		if strings.Contains(string(content), `"state":"sent"`) {
			return errors.New("죽음")
		}
		return nil
	}
	cycle()
	fault = nil
	collector = newCollector()
	cycle()

	// 몰림 — 5만 줄 · 16KB 넘는 줄(ignored 하나로 이어진다)
	printLines("t5-test-a", 50000, lineBaseMillis+10000)
	printToContainer(t, "t5-test-a", []string{strings.Repeat("L", 40000)})
	for range 8 {
		cycle()
	}

	// 다시 만듦 · 돌림
	dockerCommand(t, "", "rm", "-f", "-v", "t5-test-d")
	startDockerTestContainer(t, "t5-test-d", "20k")
	printLines("t5-test-d", 30, lineBaseMillis)
	printLines("t5-test-c", 5, lineBaseMillis)
	cycle()
	printToContainer(t, "t5-test-c", accessLines(400, lineBaseMillis+500, nil)) // 20KB 를 넘겨 돌린다 — 잃는 줄은 셀 수 없다
	cycle()
	cycle()

	distinct := distinctBySequence(t, stream.items())
	lines := map[string]int64{}
	ignored := int64(0)
	missing := map[string]bool{}
	for _, item := range distinct {
		requireObserveAccepts(t, item)
		body := decodeTrafficBody(t, item)
		for _, source := range body.Sources {
			lines[source.Container] += source.Lines
			if source.Container == "t5-test-a" {
				ignored += source.Ignored
			}
		}
		for _, entry := range body.Missing {
			missing[entry] = true
		}
	}
	for _, name := range []string{"t5-test-a", "t5-test-b", "t5-test-d"} {
		if lines[name] != printed[name] {
			t.Fatalf("%s: 보낸 lines 의 합 %d ≠ 찍은 줄 %d", name, lines[name], printed[name])
		}
	}
	if ignored != 1 {
		t.Fatalf("16KB 를 넘는 줄은 조각을 이어 한 줄: ignored %d", ignored)
	}
	if !missing["t5-test-d:replaced"] || !missing["t5-test-c:rotated"] {
		t.Fatalf("다시 만듦 · 돌림이 missing 에: %v", missing)
	}
	fmt.Fprintf(os.Stderr, "실제 Docker: 항목 %d · a %d줄 · b %d · d %d · missing %v\n", len(distinct), lines["t5-test-a"], lines["t5-test-b"], lines["t5-test-d"], missing)
}
