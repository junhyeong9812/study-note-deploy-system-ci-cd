package agent

// 컨테이너별 값 — 기준: requirement-spec.md(2026-11-24 resource-metrics) §2.1 · §2.3 · §10.1 · §10.2 · §11.1.
// 정한 세부(명세가 정하지 않은 것):
//   - 목록은 GET /containers/json?all=1(멈춘 컨테이너도 — 상태 · 재시작 횟수를 보려고). 이름은 앞의 "/" 를 뗀다.
//   - 이 프로젝트 = 라벨 com.docker.compose.project 가 "study-note-deploy-system-" 로 시작 + METRICS_EXTRA_CONTAINERS 의 이름(정확히 같은 것).
//     그 밖의 컨테이너는 싣지 않을 뿐 아니라 조회(inspect · stats)하지도 않는다.
//   - cpu_pct = Δcpu_stats.cpu_usage.total_usage(나노초) / Δ단조 시계(나노초) × 100 (코어 하나 = 100). 기준은 컨테이너 ID 별.
//   - mem_used = memory_stats.usage − (total_inactive_file 이 있으면 그것(cgroup v1), 없으면 inactive_file(v2)) — docker stats 와 같은 계산.
//   - restarts = inspect 의 RestartCount, state = 목록의 State. running 이 아니면 cpu_pct · mem_used 는 null.
//   - 컨테이너 하나의 조회가 실패하면 그 컨테이너의 그 값만 null 이다(missing 에 넣지 않는다). 목록이 실패하면 containers: [] + missing 에 containers.
//   - 50개 초과 → 이름순 앞 50개 + containers_truncated. body 가 64KB 를 넘으면 이름순 뒤 컨테이너부터 빼고 containers_truncated.

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

var containerKeys = []string{"name", "id", "project", "cpu_pct", "mem_used", "restarts", "state"}

// mixedContainers — 이 프로젝트의 컨테이너 넷(하나는 멈춤 · 하나는 설정으로 더한 이름)과 다른 컨테이너 넷.
func mixedContainers() []*fakeContainer {
	front := projectContainer("a1", "front-app-green", "front")
	front.totalUsage, front.memoryUsage, front.inactiveFile = 1000000000, 120000000, 1000000
	backend := projectContainer("b2", "backend-app-blue", "backend")
	backend.totalUsage, backend.memoryUsage, backend.inactiveFile, backend.cgroupV1, backend.restarts = 5000000000, 50000000, 2000000, true, 3
	relay := &fakeContainer{id: containerID("c3"), name: "claude-bridge-relay", state: "running", totalUsage: 1, memoryUsage: 3000000}
	observe := projectContainer("d4", "observe-app-blue", "observe")
	observe.state, observe.restarts = "exited", 5

	foreignBank := &fakeContainer{id: containerID("f9a"), name: "jun-bank-api", state: "running", totalUsage: 987654321,
		memoryUsage: 777777777, labels: map[string]string{composeProjectLabelForTest: "jun-bank"}}
	foreignLookalike := &fakeContainer{id: containerID("f9b"), name: "study-note-deploy-system-impostor", state: "running",
		memoryUsage: 666666666, labels: map[string]string{composeProjectLabelForTest: "other-project"}}
	foreignNoDash := &fakeContainer{id: containerID("f9c"), name: "almost-ours", state: "running",
		memoryUsage: 555555555, labels: map[string]string{composeProjectLabelForTest: "study-note-deploy-system"}}
	foreignBare := &fakeContainer{id: containerID("f9d"), name: "random-tool", state: "running", memoryUsage: 444444444}
	return []*fakeContainer{foreignBank, observe, front, foreignLookalike, relay, foreignNoDash, backend, foreignBare}
}

var foreignTexts = []string{"jun-bank", "impostor", "almost-ours", "random-tool", "987654321", "777777777", "666666666", "555555555", "444444444",
	containerID("f9a")[:12], containerID("f9b")[:12], containerID("f9c")[:12], containerID("f9d")[:12]}

// 컨테이너 안의 값 중 싣지 않는 것 — 환경 변수 · 명령줄 · 라벨 · 주소 · 마운트 · 이미지(§2.1 "컨테이너 이름과 숫자만").
var secretTexts = []string{"ENV_SECRET_VALUE", "COMMAND_SECRET_VALUE", "LABEL_SECRET_VALUE", "172.18.0.55", "MOUNT_SECRET_PATH", "example/", "entrypoint"}

func newContainerHarness(t *testing.T) *resourceHarness {
	settings := defaultResourceSettings()
	settings.ExtraContainers = []string{"claude-bridge-relay"}
	harness := newResourceHarness(t, settings)
	harness.docker.set(func(docker *fakeDocker) { docker.containers = mixedContainers() })
	return harness
}

func containerNames(containers []map[string]any) []string {
	names := []string{}
	for _, container := range containers {
		name, _ := container["name"].(string)
		names = append(names, name)
	}
	return names
}

func TestResourceContainersOnlyThisProjectAndExtraNames(t *testing.T) {
	harness := newContainerHarness(t)
	entry := harness.collect()
	containers := listOf(t, entry, "containers")
	if got := strings.Join(containerNames(containers), ","); got != "backend-app-blue,claude-bridge-relay,front-app-green,observe-app-blue" {
		t.Fatalf("이 프로젝트의 컨테이너만 이름순으로: %s", got)
	}
	for _, container := range containers {
		requireKeys(t, "컨테이너 "+fmt.Sprint(container["name"]), container, containerKeys...)
	}
	front := findContainer(t, containers, "front-app-green")
	if front["id"] != containerID("a1")[:12] || front["project"] != "study-note-deploy-system-front" || front["state"] != "running" {
		t.Fatalf("front 의 이름표가 다르다: %v", front)
	}
	if relay := findContainer(t, containers, "claude-bridge-relay"); relay["project"] != "" {
		t.Fatalf("compose 프로젝트가 없는 더한 이름은 project \"\": %v", relay)
	}
	text := entry.text()
	for _, forbidden := range append(append([]string{}, foreignTexts...), secretTexts...) {
		if strings.Contains(text, forbidden) {
			t.Fatalf("스트림 항목에 실리면 안 되는 %q 가 실렸다:\n%s", forbidden, text)
		}
	}
	for _, request := range harness.docker.recorded() {
		for _, foreign := range []*fakeContainer{mixedContainers()[0], mixedContainers()[3], mixedContainers()[5], mixedContainers()[7]} {
			if strings.Contains(request.path, foreign.id[:12]) || strings.Contains(request.path, foreign.name) {
				t.Fatalf("이 프로젝트가 아닌 컨테이너를 조회했다: %s %s", request.method, request.path)
			}
		}
	}
	requireMissing(t, entry, "컨테이너 정상")
}

func TestResourceContainerCPUFromPreviousSamplePerID(t *testing.T) {
	harness := newContainerHarness(t)
	first := listOf(t, harness.collect(), "containers")
	requireNull(t, "첫 수집의 front cpu_pct", findContainer(t, first, "front-app-green")["cpu_pct"])
	requireNull(t, "첫 수집의 backend cpu_pct", findContainer(t, first, "backend-app-blue")["cpu_pct"])

	harness.clock.advance(30 * time.Second)
	harness.docker.set(func(docker *fakeDocker) {
		docker.find("front-app-green").totalUsage += 210000000    // 0.21초 / 30초 → 0.7
		docker.find("backend-app-blue").totalUsage += 60000000000 // 60초 / 30초 → 코어 둘 = 200
	})
	containers := listOf(t, harness.collect(), "containers")
	requireNumber(t, "front cpu_pct", findContainer(t, containers, "front-app-green")["cpu_pct"], 0.7, 0.06)
	requireNumber(t, "backend cpu_pct(코어 하나 = 100)", findContainer(t, containers, "backend-app-blue")["cpu_pct"], 200, 0.06)
}

func TestResourceContainerCPUIgnoresWallClockAndNullsAfterLongGap(t *testing.T) {
	harness := newContainerHarness(t)
	harness.collect()
	harness.clock.advance(30 * time.Second)
	harness.clock.shiftWall(-10 * time.Minute)
	harness.docker.set(func(docker *fakeDocker) { docker.find("front-app-green").totalUsage += 3000000000 })
	requireNumber(t, "벽시계가 뒤로 가도 단조 간격으로", findContainer(t, listOf(t, harness.collect(), "containers"), "front-app-green")["cpu_pct"], 10, 0.06)

	harness.clock.advance(3 * time.Minute)
	harness.docker.set(func(docker *fakeDocker) { docker.find("front-app-green").totalUsage += 3000000000 })
	requireNull(t, "간격 2분 초과", findContainer(t, listOf(t, harness.collect(), "containers"), "front-app-green")["cpu_pct"])
}

func TestResourceRecreatedContainerStartsWithNullCPU(t *testing.T) {
	harness := newContainerHarness(t)
	harness.collect()

	harness.clock.advance(30 * time.Second)
	harness.docker.set(func(docker *fakeDocker) {
		recreated := projectContainer("a2", "front-app-green", "front") // 배포 — 같은 이름, 새 ID
		recreated.totalUsage, recreated.memoryUsage = 50000000000, 90000000
		for index, container := range docker.containers {
			if container.name == "front-app-green" {
				docker.containers[index] = recreated
			}
		}
		docker.find("backend-app-blue").totalUsage = 1000          // 같은 ID 인데 누적 값이 줄었다(제자리 재시작)
		docker.find("claude-bridge-relay").totalUsage += 300000000 // 1%
	})
	containers := listOf(t, harness.collect(), "containers")
	front := findContainer(t, containers, "front-app-green")
	if front["id"] != containerID("a2")[:12] {
		t.Fatalf("새 ID 로 실려야 한다: %v", front)
	}
	requireNull(t, "새 컨테이너(같은 이름 · 다른 ID)의 첫 cpu_pct", front["cpu_pct"])
	requireNull(t, "누적 값이 줄어든 컨테이너의 cpu_pct", findContainer(t, containers, "backend-app-blue")["cpu_pct"])
	requireNumber(t, "다른 컨테이너는 그대로", findContainer(t, containers, "claude-bridge-relay")["cpu_pct"], 1, 0.06)
	requireNumber(t, "새 컨테이너도 메모리는 바로", front["mem_used"], 90000000, 0)

	harness.clock.advance(30 * time.Second)
	harness.docker.set(func(docker *fakeDocker) {
		docker.find("front-app-green").totalUsage += 3000000000
		docker.find("backend-app-blue").totalUsage += 15000000000
	})
	containers = listOf(t, harness.collect(), "containers")
	requireNumber(t, "새 컨테이너의 둘째 cpu_pct", findContainer(t, containers, "front-app-green")["cpu_pct"], 10, 0.06)
	requireNumber(t, "재시작한 컨테이너의 다음 cpu_pct", findContainer(t, containers, "backend-app-blue")["cpu_pct"], 50, 0.06)
}

func TestResourceContainerMemorySubtractsInactiveFileCache(t *testing.T) {
	containers := listOf(t, newContainerHarness(t).collect(), "containers")
	requireNumber(t, "cgroup v2: usage − inactive_file", findContainer(t, containers, "front-app-green")["mem_used"], 119000000, 0)
	requireNumber(t, "cgroup v1: usage − total_inactive_file", findContainer(t, containers, "backend-app-blue")["mem_used"], 48000000, 0)
}

func TestResourceContainerRestartsAndStateIncludingStopped(t *testing.T) {
	containers := listOf(t, newContainerHarness(t).collect(), "containers")
	requireNumber(t, "backend restarts", findContainer(t, containers, "backend-app-blue")["restarts"], 3, 0)
	requireNumber(t, "front restarts", findContainer(t, containers, "front-app-green")["restarts"], 0, 0)
	observe := findContainer(t, containers, "observe-app-blue") // 멈춘 컨테이너도 싣는다(all=1)
	if observe["state"] != "exited" {
		t.Fatalf("멈춘 컨테이너의 state: %v", observe)
	}
	requireNumber(t, "멈춘 컨테이너의 restarts", observe["restarts"], 5, 0)
	requireNull(t, "멈춘 컨테이너의 cpu_pct", observe["cpu_pct"])
	requireNull(t, "멈춘 컨테이너의 mem_used", observe["mem_used"])
}

func TestResourceContainersTruncatedToFiftyByName(t *testing.T) {
	harness := newResourceHarness(t, defaultResourceSettings())
	harness.docker.set(func(docker *fakeDocker) {
		for index := 59; index >= 0; index-- { // 이름의 거꾸로 — 정렬은 수집기가 한다
			docker.containers = append(docker.containers, projectContainer(fmt.Sprintf("e%02d", index), fmt.Sprintf("svc-%02d", index), "svc"))
		}
	})
	entry := harness.collect()
	names := containerNames(listOf(t, entry, "containers"))
	if len(names) != 50 || names[0] != "svc-00" || names[49] != "svc-49" {
		t.Fatalf("이름순 앞 50개여야 한다: %d %v", len(names), names)
	}
	if strings.Contains(entry.text(), "svc-5") {
		t.Fatal("51번째 이후 컨테이너가 실렸다")
	}
	requireMissing(t, entry, "50개 초과", missingContainersTruncated)
}

func TestResourceContainersExactlyFiftyAreNotTruncated(t *testing.T) {
	harness := newResourceHarness(t, defaultResourceSettings())
	harness.docker.set(func(docker *fakeDocker) {
		for index := 0; index < 50; index++ {
			docker.containers = append(docker.containers, projectContainer(fmt.Sprintf("e%02d", index), fmt.Sprintf("svc-%02d", index), "svc"))
		}
	})
	entry := harness.collect()
	if names := containerNames(listOf(t, entry, "containers")); len(names) != 50 {
		t.Fatalf("50개는 모두 싣는다: %d", len(names))
	}
	requireMissing(t, entry, "정확히 50개")
}

func TestResourceBodyStaysWithinSixtyFourKilobytes(t *testing.T) {
	harness := newResourceHarness(t, defaultResourceSettings())
	harness.docker.set(func(docker *fakeDocker) {
		for index := 49; index >= 0; index-- {
			name := fmt.Sprintf("long-%02d-", index) + strings.Repeat("x", 2000)
			docker.containers = append(docker.containers, projectContainer(fmt.Sprintf("e%02d", index), name, "long"))
		}
	})
	entry := harness.collect()
	if size := len(entry.field("body")); size > 64<<10 {
		t.Fatalf("body 가 64KB 를 넘었다: %d", size)
	}
	names := containerNames(listOf(t, entry, "containers"))
	if len(names) == 0 || len(names) >= 50 {
		t.Fatalf("64KB 에 맞게 일부만 실어야 한다: %d", len(names))
	}
	for index, name := range names {
		if !strings.HasPrefix(name, fmt.Sprintf("long-%02d-", index)) {
			t.Fatalf("이름순 앞에서부터 남겨야 한다: %d번째 %q", index, name[:8])
		}
	}
	requireMissing(t, entry, "64KB 초과", missingContainersTruncated)
	requireHostLevels(t, "64KB 를 줄여도 host 는 그대로", hostOf(t, entry), 0.11)
}

func TestResourceDockerListFailureLeavesHostValues(t *testing.T) {
	harness := newContainerHarness(t)
	harness.docker.set(func(docker *fakeDocker) { docker.listStatus = 500 })
	entry := harness.collect()
	if containers := listOf(t, entry, "containers"); len(containers) != 0 {
		t.Fatalf("목록이 실패하면 containers 는 []: %v", containers)
	}
	requireMissing(t, entry, "도커 목록 실패", missingContainers)
	requireHostLevels(t, "도커가 실패해도 host 는 그대로", hostOf(t, entry), 0.11)
}

func TestResourceDockerUnreachableLeavesHostValues(t *testing.T) {
	harness := newContainerHarness(t)
	harness.docker.server.Close() // 소켓에 닿지 못한다
	entry := harness.collect()
	if containers := listOf(t, entry, "containers"); len(containers) != 0 {
		t.Fatalf("도커에 닿지 못하면 containers 는 []: %v", containers)
	}
	requireMissing(t, entry, "도커에 닿지 못함", missingContainers)
	requireHostLevels(t, "도커에 닿지 못해도 host 는 그대로", hostOf(t, entry), 0.11)
}

func TestResourceOneContainerFailureNullsOnlyThatContainer(t *testing.T) {
	harness := newContainerHarness(t)
	harness.docker.set(func(docker *fakeDocker) {
		docker.find("backend-app-blue").statsStatus = 500
		docker.find("backend-app-blue").inspectStatus = 500
	})
	entry := harness.collect()
	containers := listOf(t, entry, "containers")
	backend := findContainer(t, containers, "backend-app-blue")
	requireNull(t, "통계 조회가 실패한 컨테이너의 mem_used", backend["mem_used"])
	requireNull(t, "조회가 실패한 컨테이너의 restarts", backend["restarts"])
	if backend["state"] != "running" {
		t.Fatalf("목록의 상태는 그대로: %v", backend)
	}
	requireNumber(t, "다른 컨테이너는 그대로", findContainer(t, containers, "front-app-green")["mem_used"], 119000000, 0)
	requireMissing(t, entry, "컨테이너 하나의 실패는 갈래 전체의 결측이 아니다")
}
