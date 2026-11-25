package agent

// 자원 지표 테스트의 받침 — 가짜 /proc 받침 파일 · 가짜 도커(HTTP 서버) · 가짜 시계 · 가짜 스트림 · 로그 기록.
// 기준: docs/cross-cutting/plans/2026-11-24/resource-metrics/requirement-spec.md §2.1 · §2.3 · §2.5 · §10.1 · §10.2 · §11.1.
// 구현을 보지 않고 명세에서 쓴 계약 테스트다. /proc 의 꼴은 실제 리눅스에서 따왔고 값은 계산하기 쉬운 숫자로 바꿨다.

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/junhyeong9812/study-note-deploy-system-ci-cd/internal/shared"
)

const (
	resourceWait     = 2 * time.Second // 수집 하나를 기다리는 상한(실제 구현은 밀리초 안에 끝난다)
	resourceTickWait = time.Second     // 수집기가 주기를 받기를 기다리는 상한
	resourceServer   = "home-9"
)

var resourceWallStart = time.Date(2026, 11, 24, 0, 0, 0, 0, time.UTC)

// ===== 가짜 /proc =====

type networkCounter struct {
	name   string
	rx, tx uint64 // 누적 바이트
}

type diskCounter struct {
	major, minor                int
	name                        string
	sectorsRead, sectorsWritten uint64 // 누적 섹터(512 바이트)
}

type procState struct {
	cpu            [10]uint64 // user nice system idle iowait irq softirq steal guest guest_nice
	load           string     // /proc/loadavg 한 줄
	memTotalKB     uint64
	memAvailableKB uint64
	network        []networkCounter
	disks          []diskCounter
}

// baseProc — 첫 표본. 아래 nextProc 과의 차이로 기대값을 정한다(간격 30초).
func baseProc() procState {
	return procState{
		cpu:            [10]uint64{1000, 0, 1000, 7000, 500, 0, 0, 500, 0, 0},
		load:           "0.11 0.20 0.30 1/200 12345",
		memTotalKB:     1000000,
		memAvailableKB: 250000,
		network: []networkCounter{
			{"lo", 5000000000000, 5000000000000},
			{"eno1", 1000, 2000},
			{"wlo1", 12345678901, 500},
			{"veth1a2b3c4", 1000000, 1000000},
			{"br-0a1b2c3d4e5f", 1000000, 1000000},
			{"docker0", 1000000, 1000000},
			{"cni0", 1000000, 1000000},
			{"flannel.1", 1000000, 1000000},
		},
		disks: []diskCounter{
			{7, 0, "loop0", 1000, 1000},
			{259, 0, "nvme0n1", 1000, 5000},
			{259, 1, "nvme0n1p1", 900, 4000},
			{259, 2, "nvme0n1p2", 100, 1000},
			{8, 0, "sda", 300, 300},
			{8, 1, "sda1", 300, 300},
			{1, 0, "ram0", 1000, 1000},
			{253, 0, "dm-0", 900, 4000},
			{9, 0, "md0", 900, 4000},
			{11, 0, "sr0", 1000, 0},
			{252, 0, "zram0", 1000, 1000},
		},
	}
}

// nextProc — baseProc 30초 뒤. 기대값:
//
//	cpu: Δ(user..steal) 합 1500, Δidle 500 + Δiowait 50 → 바쁨 950 → 63.33%
//	     (iowait 를 바쁨으로 세면 66.67, steal 을 빼면 46.67, guest 를 합에 더하면 72.5 — 모두 틀린 값)
//	net: eno1 rx +30000 · tx +90000, wlo1 rx +6000 · tx +12000 → rx 1200 B/s, tx 3400 B/s (다른 장치는 크게 움직인다)
//	io : nvme0n1 읽기 +600 · 쓰기 +3000 섹터, sda 읽기 +60 → 읽기 (660×512)/30 = 11264 B/s, 쓰기 (3000×512)/30 = 51200 B/s
//	     (파티션 · dm · md 는 부모와 같은 만큼 움직여 겹쳐 세면 두 배가 된다)
func nextProc() procState {
	state := baseProc()
	state.cpu = [10]uint64{1600, 0, 1100, 7500, 550, 0, 0, 750, 500, 0}
	state.load = "0.42 0.20 0.30 2/201 12399"
	for index := range state.network {
		counter := &state.network[index]
		switch counter.name {
		case "eno1":
			counter.rx, counter.tx = counter.rx+30000, counter.tx+90000
		case "wlo1":
			counter.rx, counter.tx = counter.rx+6000, counter.tx+12000
		default:
			counter.rx, counter.tx = counter.rx+3000000000, counter.tx+3000000000
		}
	}
	for index := range state.disks {
		counter := &state.disks[index]
		switch counter.name {
		case "nvme0n1", "nvme0n1p1", "dm-0", "md0":
			counter.sectorsRead, counter.sectorsWritten = counter.sectorsRead+600, counter.sectorsWritten+3000
		case "sda", "sda1":
			counter.sectorsRead += 60
		case "nvme0n1p2":
		default:
			counter.sectorsRead, counter.sectorsWritten = counter.sectorsRead+90000000, counter.sectorsWritten+90000000
		}
	}
	return state
}

// writeProc — procRoot 아래에 stat · loadavg · meminfo · net/dev · diskstats 를 실제 꼴로 쓴다.
func writeProc(t *testing.T, root string, state procState) {
	t.Helper()
	cpu := state.cpu
	var stat strings.Builder
	fmt.Fprintf(&stat, "cpu  %d %d %d %d %d %d %d %d %d %d\n", cpu[0], cpu[1], cpu[2], cpu[3], cpu[4], cpu[5], cpu[6], cpu[7], cpu[8], cpu[9])
	fmt.Fprintf(&stat, "cpu0 %d %d %d %d %d %d %d %d %d %d\n", cpu[0]/2, cpu[1]/2, cpu[2]/2, cpu[3]/2, cpu[4]/2, cpu[5]/2, cpu[6]/2, cpu[7]/2, cpu[8]/2, cpu[9]/2)
	fmt.Fprintf(&stat, "cpu1 %d %d %d %d %d %d %d %d %d %d\n", cpu[0]/2, cpu[1]/2, cpu[2]/2, cpu[3]/2, cpu[4]/2, cpu[5]/2, cpu[6]/2, cpu[7]/2, cpu[8]/2, cpu[9]/2)
	stat.WriteString("intr 8858133849 0 6 0 0 0 0 0 0 0 335 0\nctxt 15510017331\nbtime 1794897827\nprocesses 26747605\n" +
		"procs_running 2\nprocs_blocked 0\nsoftirq 1000 1 2 3 4 5 6 7 8 9\n")

	var meminfo strings.Builder
	fmt.Fprintf(&meminfo, "MemTotal:       %8d kB\nMemFree:        %8d kB\nMemAvailable:   %8d kB\n", state.memTotalKB, state.memAvailableKB/2, state.memAvailableKB)
	meminfo.WriteString("Buffers:          40888 kB\nCached:          300000 kB\nSwapCached:            0 kB\nActive:          400000 kB\n" +
		"Inactive:        100000 kB\nSwapTotal:       2097148 kB\nSwapFree:        2097148 kB\n")

	var network strings.Builder
	network.WriteString("Inter-|   Receive                                                |  Transmit\n" +
		" face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed\n")
	for _, counter := range state.network {
		// 커널과 같은 폭(장치 이름 6칸 + ':' + 바이트 8칸) — 큰 값은 ':' 에 붙는다(실제 꼴)
		fmt.Fprintf(&network, "%6s:%8d %7d %4d %4d %4d %5d %10d %9d %8d %7d %4d %4d %4d %5d %7d %10d\n",
			counter.name, counter.rx, 4242, 0, 0, 0, 0, 0, 0, counter.tx, 4343, 0, 0, 0, 0, 0, 0)
	}

	var diskstats strings.Builder
	for _, counter := range state.disks {
		fmt.Fprintf(&diskstats, "%4d %7d %s %d %d %d %d %d %d %d %d %d %d %d %d %d %d %d %d %d\n",
			counter.major, counter.minor, counter.name, 11, 0, counter.sectorsRead, 7, 13, 0, counter.sectorsWritten, 9, 0, 20, 30, 0, 0, 0, 0, 0, 0)
	}

	files := map[string]string{
		"stat": stat.String(), "loadavg": state.load + "\n", "meminfo": meminfo.String(),
		filepath.Join("net", "dev"): network.String(), "diskstats": diskstats.String(),
	}
	for name, content := range files {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// ===== 가짜 시계 =====

type fakeClock struct {
	mutex     sync.Mutex
	monotonic time.Duration
	wall      time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{monotonic: 100 * time.Second, wall: resourceWallStart}
}

func (clock *fakeClock) monotonicNow() time.Duration {
	clock.mutex.Lock()
	defer clock.mutex.Unlock()
	return clock.monotonic
}

func (clock *fakeClock) wallNow() time.Time {
	clock.mutex.Lock()
	defer clock.mutex.Unlock()
	return clock.wall
}

// advance — 두 시계를 함께 움직인다.
func (clock *fakeClock) advance(elapsed time.Duration) {
	clock.mutex.Lock()
	defer clock.mutex.Unlock()
	clock.monotonic += elapsed
	clock.wall = clock.wall.Add(elapsed)
}

// shiftWall — 벽시계만 움직인다(NTP 보정 · 손으로 고친 시계).
func (clock *fakeClock) shiftWall(shift time.Duration) {
	clock.mutex.Lock()
	defer clock.mutex.Unlock()
	clock.wall = clock.wall.Add(shift)
}

// ===== 가짜 스트림 =====

type resourceEntry struct {
	stream      string
	maxLength   int
	fields      []shared.StreamField
	hasDeadline bool
}

type fakeStream struct {
	mutex     sync.Mutex
	entries   chan resourceEntry // 성공한 항목
	failNext  int                // 앞으로 이만큼 실패
	stall     bool               // ctx 가 끝날 때까지 붙잡는다
	panicNext bool
	calls     int
	attempts  []resourceEntry // 모든 호출(실패 포함)
	released  chan time.Duration
}

func newFakeStream() *fakeStream {
	return &fakeStream{entries: make(chan resourceEntry, 256), released: make(chan time.Duration, 16)}
}

func (stream *fakeStream) Append(ctx context.Context, name string, maxLength int, fields []shared.StreamField) error {
	_, hasDeadline := ctx.Deadline()
	entry := resourceEntry{stream: name, maxLength: maxLength, fields: append([]shared.StreamField(nil), fields...), hasDeadline: hasDeadline}
	stream.mutex.Lock()
	stream.calls++
	stream.attempts = append(stream.attempts, entry)
	fail := stream.failNext > 0
	if fail {
		stream.failNext--
	}
	stall, panicNow := stream.stall, stream.panicNext
	stream.panicNext = false
	stream.mutex.Unlock()
	if panicNow {
		panic("가짜 스트림 패닉")
	}
	if stall {
		startedAt := time.Now()
		select {
		case <-ctx.Done():
			stream.released <- time.Since(startedAt)
			return ctx.Err()
		case <-time.After(5 * time.Second):
			return errors.New("멈춘 스트림이 기한에 풀려나지 않았다")
		}
	}
	if fail {
		return errors.New("가짜 Redis 실패")
	}
	select {
	case stream.entries <- entry:
	default:
	}
	return nil
}

func (stream *fakeStream) set(change func(stream *fakeStream)) {
	stream.mutex.Lock()
	defer stream.mutex.Unlock()
	change(stream)
}

// ===== 로그 기록 =====

type logLine struct{ message, level string }

type logRecorder struct {
	mutex sync.Mutex
	lines []logLine
}

func (recorder *logRecorder) log(message, level string) {
	recorder.mutex.Lock()
	defer recorder.mutex.Unlock()
	recorder.lines = append(recorder.lines, logLine{message, level})
}

func (recorder *logRecorder) snapshot() []logLine {
	recorder.mutex.Lock()
	defer recorder.mutex.Unlock()
	return append([]logLine(nil), recorder.lines...)
}

// ===== 가짜 도커(엔진 API — HTTP) =====

type fakeContainer struct {
	id            string // 64자 16진수
	name          string
	labels        map[string]string
	state         string
	restarts      int
	totalUsage    uint64 // cpu_stats.cpu_usage.total_usage (나노초 누적)
	memoryUsage   uint64
	inactiveFile  uint64
	cgroupV1      bool // memory_stats.stats 에 total_inactive_file(v1) — 아니면 inactive_file 만(v2)
	statsStatus   int  // 0 이 아니면 통계 조회가 이 상태로 실패
	inspectStatus int
}

type dockerRequest struct {
	method, path, rawQuery string
	body                   []byte
}

type stallRelease struct {
	path     string
	byClient bool // 부르는 쪽이 끊었다(기한) — 아니면 5초 안전장치
	after    time.Duration
}

type fakeDocker struct {
	mutex            sync.Mutex
	containers       []*fakeContainer
	listStatus       int
	stallList        bool
	stallStats       bool
	stallExecStart   bool
	execCreateStatus int
	gpuOutput        string
	gpuExitCode      int
	// --- GPU 용 실행(exec) — 도커 29.1 실측 꼴(§13) ---
	gpuContainer        string               // GPU 컨테이너 이름(기본 "llm-ollama") — 조회하면 ExecIDs 를 준다
	gpuContainerInspect string               // 그 컨테이너 조회의 동작: "" 정상 · "500" · "404" · "stall" · "drop"
	gpuContainerState   string               // "" 도는 중 · "exited"(꺼짐) · "paused"(일시정지) · "restarting" — 조회는 200(실측)
	execs               map[string]*fakeExec // 실행 ID → 실행
	execOrder           []string             // 만든 순서
	execCount           int                  // 만든 실행 수 — ID 는 exec-1, exec-2, …
	execStartStatus     int                  // 0 이 아니면 시작이 이 상태로 실패(실행은 시작 안 함으로 남는다)
	holdCreateResponse  bool                 // 실행을 만들고 응답은 부른 쪽이 끊을 때까지 보류(만들기 응답 유실)
	execRunningCount    int                  // 출력이 끝난 뒤 처음 이만큼의 상태 조회는 Running:true(종료 처리가 늦음)
	execStayRunning     bool                 // 출력이 끝난 뒤에도 끝내 Running:true
	slowStart           time.Duration        // 0 이 아니면 명령이 이만큼(실제 시간) 걸린 뒤 끝난다 — 부른 쪽이 먼저 끊어도 끝난다
	requests            []dockerRequest
	releases            chan stallRelease
	server              *httptest.Server
}

var dockerVersionPrefix = regexp.MustCompile(`^/v[0-9]+\.[0-9]+`)

func newFakeDocker(t *testing.T) *fakeDocker {
	docker := &fakeDocker{releases: make(chan stallRelease, 64), gpuContainer: "llm-ollama", execs: map[string]*fakeExec{}}
	docker.server = httptest.NewServer(http.HandlerFunc(docker.serve))
	t.Cleanup(docker.server.Close)
	return docker
}

func (docker *fakeDocker) api() *dockerAPI {
	return &dockerAPI{baseURL: docker.server.URL, client: docker.server.Client()}
}

func (docker *fakeDocker) set(change func(docker *fakeDocker)) {
	docker.mutex.Lock()
	defer docker.mutex.Unlock()
	change(docker)
}

func (docker *fakeDocker) recorded() []dockerRequest {
	docker.mutex.Lock()
	defer docker.mutex.Unlock()
	return append([]dockerRequest(nil), docker.requests...)
}

// find — 도커처럼 전체 ID · 이름 · ID 앞자리(12자 이상)로 찾는다.
func (docker *fakeDocker) find(key string) *fakeContainer {
	for _, container := range docker.containers {
		if container.id == key || container.name == key || (len(key) >= 12 && strings.HasPrefix(container.id, key)) {
			return container
		}
	}
	return nil
}

func writeDockerJSON(writer http.ResponseWriter, status int, body any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(body)
}

func (docker *fakeDocker) stall(request *http.Request, path string) {
	startedAt := time.Now()
	release := stallRelease{path: path}
	select {
	case <-request.Context().Done():
		release.byClient = true
	case <-time.After(5 * time.Second):
	}
	release.after = time.Since(startedAt)
	select {
	case docker.releases <- release:
	default:
	}
}

var (
	dockerInspectPath    = regexp.MustCompile(`^/containers/([^/]+)/json$`)
	dockerStatsPath      = regexp.MustCompile(`^/containers/([^/]+)/stats$`)
	dockerExecCreatePath = regexp.MustCompile(`^/containers/([^/]+)/exec$`)
	dockerExecStartPath  = regexp.MustCompile(`^/exec/([^/]+)/start$`)
	dockerExecInspect    = regexp.MustCompile(`^/exec/([^/]+)/json$`)
)

func (docker *fakeDocker) serve(writer http.ResponseWriter, request *http.Request) {
	body, _ := io.ReadAll(request.Body)
	path := dockerVersionPrefix.ReplaceAllString(request.URL.Path, "")
	docker.mutex.Lock()
	docker.requests = append(docker.requests, dockerRequest{request.Method, path, request.URL.RawQuery, body})
	docker.mutex.Unlock()
	switch {
	case request.Method == http.MethodGet && path == "/containers/json":
		docker.serveList(writer, request, path)
	case request.Method == http.MethodGet && dockerInspectPath.MatchString(path):
		docker.serveInspect(writer, request, path, dockerInspectPath.FindStringSubmatch(path)[1])
	case request.Method == http.MethodGet && dockerStatsPath.MatchString(path):
		docker.serveStats(writer, request, path, dockerStatsPath.FindStringSubmatch(path)[1])
	case request.Method == http.MethodPost && dockerExecCreatePath.MatchString(path):
		docker.serveExecCreate(writer, request, path, body, dockerExecCreatePath.FindStringSubmatch(path)[1])
	case request.Method == http.MethodPost && dockerExecStartPath.MatchString(path):
		docker.serveExecStart(writer, request, path, dockerExecStartPath.FindStringSubmatch(path)[1])
	case request.Method == http.MethodGet && dockerExecInspect.MatchString(path):
		docker.serveExecInspect(writer, request, path, dockerExecInspect.FindStringSubmatch(path)[1])
	default:
		writeDockerJSON(writer, http.StatusNotFound, map[string]string{"message": "page not found"})
	}
}

func (docker *fakeDocker) serveList(writer http.ResponseWriter, request *http.Request, path string) {
	docker.mutex.Lock()
	status, stall := docker.listStatus, docker.stallList
	all := request.URL.Query().Get("all")
	items := []map[string]any{}
	for _, container := range docker.containers {
		if all != "1" && all != "true" && container.state != "running" { // 도커처럼 all 없이는 실행 중인 것만
			continue
		}
		labels := map[string]string{"com.example.secret-label": "LABEL_SECRET_VALUE"}
		for key, value := range container.labels {
			labels[key] = value
		}
		items = append(items, map[string]any{
			"Id": container.id, "Names": []string{"/" + container.name}, "Image": "example/" + container.name + ":latest",
			"ImageID": "sha256:" + strings.Repeat("e", 64), "Command": "/entrypoint.sh --token=COMMAND_SECRET_VALUE",
			"Created": 1763900000, "Ports": []any{}, "Labels": labels, "State": container.state, "Status": "Up 2 hours",
			"HostConfig":      map[string]any{"NetworkMode": "default"},
			"NetworkSettings": map[string]any{"Networks": map[string]any{"bridge": map[string]any{"IPAddress": "172.18.0.55"}}},
			"Mounts":          []any{map[string]any{"Source": "/srv/MOUNT_SECRET_PATH", "Destination": "/data"}},
		})
	}
	docker.mutex.Unlock()
	if stall {
		docker.stall(request, path)
		return
	}
	if status != 0 {
		writeDockerJSON(writer, status, map[string]string{"message": "fake list failure"})
		return
	}
	writeDockerJSON(writer, http.StatusOK, items)
}

func (docker *fakeDocker) serveInspect(writer http.ResponseWriter, request *http.Request, path, key string) {
	docker.mutex.Lock()
	if docker.gpuContainer != "" && key == docker.gpuContainer && docker.find(key) == nil {
		behaviour, containerState := docker.gpuContainerInspect, docker.gpuContainerState
		var execIDs []string // 실행이 없으면 null
		for _, id := range docker.execOrder {
			if containerState == "exited" {
				break // 꺼진 컨테이너는 ExecIDs:null(실측)
			}
			if state := docker.execs[id].state; state == "created" || state == "running" { // 끝난 실행은 빠진다
				execIDs = append(execIDs, id)
			}
		}
		docker.mutex.Unlock()
		if respondTrouble(writer, behaviour, func() { docker.stall(request, path) }) {
			return
		}
		writeDockerJSON(writer, http.StatusOK, map[string]any{
			"Id": containerID("99"), "Name": "/" + key, "RestartCount": 0, "ExecIDs": execIDs,
			"State":  gpuContainerStateBody(containerState),
			"Config": map[string]any{"Env": []string{"OLLAMA_TOKEN=ENV_SECRET_VALUE"}, "Cmd": []string{"serve"}},
		})
		return
	}
	defer docker.mutex.Unlock()
	container := docker.find(key)
	if container == nil {
		writeDockerJSON(writer, http.StatusNotFound, map[string]string{"message": "No such container: " + key})
		return
	}
	if container.inspectStatus != 0 {
		writeDockerJSON(writer, container.inspectStatus, map[string]string{"message": "fake inspect failure"})
		return
	}
	writeDockerJSON(writer, http.StatusOK, map[string]any{
		"Id": container.id, "Name": "/" + container.name, "RestartCount": container.restarts,
		"Path": "/entrypoint.sh", "Args": []string{"--token=COMMAND_SECRET_VALUE"},
		"State": map[string]any{"Status": container.state, "Running": container.state == "running", "Restarting": false, "Pid": 4242, "ExitCode": 0},
		"Config": map[string]any{"Env": []string{"DATABASE_URL=postgres://user:ENV_SECRET_VALUE@10.0.0.9/db"},
			"Cmd": []string{"--token=COMMAND_SECRET_VALUE"}, "Labels": container.labels, "Image": "example/" + container.name + ":latest"},
		"NetworkSettings": map[string]any{"IPAddress": "172.18.0.55"},
	})
}

func (docker *fakeDocker) serveStats(writer http.ResponseWriter, request *http.Request, path, key string) {
	docker.mutex.Lock()
	container := docker.find(key)
	stall := docker.stallStats
	var body map[string]any
	status := 0
	switch {
	case container == nil:
		status = http.StatusNotFound
	case container.statsStatus != 0:
		status = container.statsStatus
	case request.URL.Query().Get("stream") != "false" && request.URL.Query().Get("stream") != "0":
		status = http.StatusBadRequest // 흐름으로 받으면 끝나지 않는다 — 한 번짜리만 받는다
	default:
		memoryStats := map[string]any{"inactive_file": container.inactiveFile, "active_file": 7, "anon": 11}
		if container.cgroupV1 { // v1 은 total_inactive_file 이 기준이다(inactive_file 은 계층을 뺀 다른 값)
			memoryStats = map[string]any{"total_inactive_file": container.inactiveFile, "inactive_file": container.inactiveFile / 4, "cache": 99}
		}
		body = map[string]any{
			"read": "2026-11-24T00:00:00.000000000Z", "preread": "0001-01-01T00:00:00Z", "name": "/" + container.name, "id": container.id,
			"cpu_stats": map[string]any{"cpu_usage": map[string]any{"total_usage": container.totalUsage, "usage_in_kernelmode": 1, "usage_in_usermode": 1},
				"system_cpu_usage": 424242424242, "online_cpus": 8},
			"precpu_stats": map[string]any{"cpu_usage": map[string]any{"total_usage": 0}, "system_cpu_usage": 0}, // 한 번짜리는 직전 표본이 없다(§10.1)
			"memory_stats": map[string]any{"usage": container.memoryUsage, "limit": 8000000000, "stats": memoryStats},
			"networks":     map[string]any{"eth0": map[string]any{"rx_bytes": 1, "tx_bytes": 1}},
		}
	}
	docker.mutex.Unlock()
	if stall {
		docker.stall(request, path)
		return
	}
	if status != 0 {
		writeDockerJSON(writer, status, map[string]string{"message": "fake stats failure"})
		return
	}
	writeDockerJSON(writer, http.StatusOK, body)
}

// fakeExec — 도커의 실행 하나. 상태는 created(시작 안 함) → running → exited.
type fakeExec struct {
	id              string
	entrypoint      string
	arguments       []string
	state           string
	exitCode        int
	settleRemaining int    // 끝난 뒤 Running:true 로 답할 남은 조회 수
	inspect         string // 상태 조회의 동작: "" 정상 · "404" · "500" · "stall" · "drop"
	processConfig   string // "" 정상 · "missing"(칸 없음) · "malformed"(꼴이 다름) · "no-arguments"(인자 칸 없음) · "null-arguments"
}

// gpuQueryArguments — 고정 조회 명령의 인자(실행 조회의 ProcessConfig.arguments 꼴).
var gpuQueryArguments = []string{"--query-gpu=index,utilization.gpu,memory.used,memory.total,temperature.gpu", "--format=csv,noheader,nounits"}

// addExec — 이미 있는 실행을 심는다(agent 재시작 · 사람이 연 셸 등). set 안에서 부른다.
func (docker *fakeDocker) addExec(id, state, entrypoint string, arguments ...string) *fakeExec {
	exec := &fakeExec{id: id, state: state, entrypoint: entrypoint, arguments: arguments}
	docker.execs[id] = exec
	docker.execOrder = append(docker.execOrder, id)
	return exec
}

// respondTrouble — 조회의 고장 동작. 답했으면 true.
func respondTrouble(writer http.ResponseWriter, behaviour string, stall func()) bool {
	switch behaviour {
	case "404":
		writeDockerJSON(writer, http.StatusNotFound, map[string]string{"message": "No such container or exec instance"})
	case "500":
		writeDockerJSON(writer, http.StatusInternalServerError, map[string]string{"message": "fake failure"})
	case "stall":
		stall()
	case "drop":
		if hijacker, ok := writer.(http.Hijacker); ok {
			if connection, _, err := hijacker.Hijack(); err == nil {
				connection.Close()
			}
		}
	default:
		return false
	}
	return true
}

func (docker *fakeDocker) serveExecCreate(writer http.ResponseWriter, request *http.Request, path string, body []byte, key string) {
	docker.mutex.Lock()
	status, hold := docker.execCreateStatus, docker.holdCreateResponse
	if status != 0 {
		docker.mutex.Unlock()
		writeDockerJSON(writer, status, map[string]string{"message": "container " + key + " is not running"})
		return
	}
	if state := docker.gpuContainerState; state != "" && key == docker.gpuContainer { // 돌고 있지 않은 컨테이너 — 409, 실행 기록 없음(실측)
		docker.mutex.Unlock()
		writeDockerJSON(writer, http.StatusConflict, map[string]string{"message": "container " + key + " is not running (" + state + ")"})
		return
	}
	var create struct{ Cmd []string }
	_ = json.Unmarshal(body, &create)
	docker.execCount++
	id := fmt.Sprintf("exec-%d", docker.execCount)
	exec := docker.addExec(id, "created", "", nil...)
	if len(create.Cmd) > 0 {
		exec.entrypoint, exec.arguments = create.Cmd[0], create.Cmd[1:]
	}
	docker.mutex.Unlock()
	if hold { // 실행은 생겼지만 응답은 오지 않는다
		docker.stall(request, path)
		return
	}
	writeDockerJSON(writer, http.StatusCreated, map[string]string{"Id": id})
}

// serveExecInspect — 실행의 상태(도커 29.1 꼴). 시작 안 함 = Running:false · ExitCode:null · Pid:0.
func (docker *fakeDocker) serveExecInspect(writer http.ResponseWriter, request *http.Request, path, id string) {
	docker.mutex.Lock()
	exec := docker.execs[id]
	if exec == nil {
		docker.mutex.Unlock()
		writeDockerJSON(writer, http.StatusNotFound, map[string]string{"message": "No such exec instance: " + id})
		return
	}
	behaviour := exec.inspect
	running := exec.state == "running"
	if exec.state == "exited" && exec.settleRemaining > 0 {
		exec.settleRemaining--
		running = true
	}
	body := map[string]any{"ID": id, "ContainerID": containerID("99"), "Running": running, "ExitCode": nil, "Pid": 4343,
		"OpenStdout": true, "OpenStderr": true, "CanRemove": false}
	switch {
	case exec.state == "created":
		body["Pid"] = 0
	case exec.state == "exited" && !running:
		body["ExitCode"] = exec.exitCode
	}
	arguments := exec.arguments
	if arguments == nil {
		arguments = []string{} // 인자가 없는 명령에도 빈 배열(실측)
	}
	switch exec.processConfig {
	case "no-arguments": // 인자 칸 자체가 없음
		body["ProcessConfig"] = map[string]any{"tty": false, "entrypoint": exec.entrypoint, "privileged": false, "user": ""}
	case "null-arguments":
		body["ProcessConfig"] = map[string]any{"tty": false, "entrypoint": exec.entrypoint, "arguments": nil, "privileged": false, "user": ""}
	case "missing":
	case "malformed":
		body["ProcessConfig"] = map[string]any{"entrypoint": 42, "arguments": "not-a-list"}
	default:
		body["ProcessConfig"] = map[string]any{"tty": false, "entrypoint": exec.entrypoint, "arguments": arguments, "privileged": false, "user": ""}
	}
	docker.mutex.Unlock()
	if respondTrouble(writer, behaviour, func() { docker.stall(request, path) }) {
		return
	}
	writeDockerJSON(writer, http.StatusOK, body)
}

// serveExecStart — Tty 없는 exec 의 출력은 다중화된 흐름이다: 조각마다 [흐름(1=stdout), 0, 0, 0, 길이(빅엔디언 4바이트)] + 내용.
// 부른 쪽이 연결만 끊으면 실행은 Running:true 로 남는다(실측).
func (docker *fakeDocker) serveExecStart(writer http.ResponseWriter, request *http.Request, path, id string) {
	docker.mutex.Lock()
	exec := docker.execs[id]
	if exec == nil {
		docker.mutex.Unlock()
		writeDockerJSON(writer, http.StatusNotFound, map[string]string{"message": "No such exec instance: " + id})
		return
	}
	if docker.gpuContainerState != "" { // 돌고 있지 않은 컨테이너의 실행은 시작되지 않는다(409)
		docker.mutex.Unlock()
		writeDockerJSON(writer, http.StatusConflict, map[string]string{"message": "container is not running"})
		return
	}
	if docker.execStartStatus != 0 {
		status := docker.execStartStatus
		docker.mutex.Unlock()
		writeDockerJSON(writer, status, map[string]string{"message": "fake exec start failure"})
		return
	}
	if exec.state != "created" {
		docker.mutex.Unlock()
		writeDockerJSON(writer, http.StatusConflict, map[string]string{"message": "exec " + id + " has already been started"})
		return
	}
	exec.state = "running"
	output, stall, slow, stayRunning, runningCount, exitCode :=
		docker.gpuOutput, docker.stallExecStart, docker.slowStart, docker.execStayRunning, docker.execRunningCount, docker.gpuExitCode
	docker.mutex.Unlock()
	finish := func() {
		docker.mutex.Lock()
		defer docker.mutex.Unlock()
		if stayRunning {
			return
		}
		exec.state, exec.exitCode, exec.settleRemaining = "exited", exitCode, runningCount
	}
	if stall {
		docker.stall(request, path) // 끊긴 뒤에도 도는 중으로 남는다
		return
	}
	if slow > 0 {
		startedAt := time.Now()
		select {
		case <-request.Context().Done():
			go func() { // 부른 쪽이 끊어도 명령은 끝까지 돈다
				time.Sleep(slow - time.Since(startedAt))
				finish()
			}()
			return
		case <-time.After(slow):
		}
	}
	writer.Header().Set("Content-Type", "application/vnd.docker.multiplexed-stream")
	writer.WriteHeader(http.StatusOK)
	for _, piece := range splitInTwo(output) { // 줄 중간에서 조각이 나뉘어도 이어 읽어야 한다
		header := make([]byte, 8)
		header[0] = 1
		binary.BigEndian.PutUint32(header[4:], uint32(len(piece)))
		_, _ = writer.Write(append(header, piece...))
	}
	finish()
}

func splitInTwo(output string) []string {
	if len(output) < 2 {
		return []string{output}
	}
	return []string{output[:len(output)/2], output[len(output)/2:]}
}

// ===== 받침 묶음 =====

type resourceHarness struct {
	t         *testing.T
	agent     *Agent
	collector *ResourceCollector
	procRoot  string
	clock     *fakeClock
	docker    *fakeDocker
	stream    *fakeStream
	logs      *logRecorder
	ticks     chan time.Time
	started   bool

	statfsMutex sync.Mutex
	statfsPaths []string
	statfsHook  func(path string, stat *syscall.Statfs_t) error // nil 이면 기본 값
}

func defaultResourceSettings() ResourceMetricsSettings {
	return ResourceMetricsSettings{RedisAddress: "127.0.0.1:6379", Stream: "metrics", Server: resourceServer}
}

// newResourceHarness — 배포 경로가 붙은 agent 에서 수집기를 만들고 이음매를 모두 가짜로 바꾼다. 아직 돌리지 않는다.
func newResourceHarness(t *testing.T, settings ResourceMetricsSettings) *resourceHarness {
	t.Helper()
	agent := newPipelineAgent(t, &recordedCommands{})
	harness := &resourceHarness{t: t, agent: agent, procRoot: t.TempDir(), clock: newFakeClock(), docker: newFakeDocker(t),
		stream: newFakeStream(), logs: &logRecorder{}, ticks: make(chan time.Time)}
	writeProc(t, harness.procRoot, baseProc())
	collector := agent.newResourceCollector(settings)
	if collector == nil {
		t.Fatal("newResourceCollector 가 nil 을 돌려줬다")
	}
	collector.procRoot = harness.procRoot
	collector.diskPath = "/"
	collector.statfs = harness.statfs
	collector.docker = harness.docker.api()
	collector.stream = harness.stream
	collector.monotonicNow = harness.clock.monotonicNow
	collector.wallNow = harness.clock.wallNow
	collector.logf = harness.logs.log
	harness.collector = collector
	return harness
}

// statfs — 기본: 블록 1000 · 빈 블록 250 · 블록 4096 바이트 → 전체 4096000, 사용 3072000.
func (harness *resourceHarness) statfs(path string, stat *syscall.Statfs_t) error {
	harness.statfsMutex.Lock()
	harness.statfsPaths = append(harness.statfsPaths, path)
	hook := harness.statfsHook
	harness.statfsMutex.Unlock()
	if hook != nil {
		return hook(path, stat)
	}
	stat.Blocks, stat.Bfree, stat.Bavail, stat.Bsize = 1000, 250, 200, 4096
	return nil
}

func (harness *resourceHarness) setStatfs(hook func(path string, stat *syscall.Statfs_t) error) {
	harness.statfsMutex.Lock()
	defer harness.statfsMutex.Unlock()
	harness.statfsHook = hook
}

func (harness *resourceHarness) writeProc(state procState) {
	writeProc(harness.t, harness.procRoot, state)
}

func (harness *resourceHarness) start() {
	if harness.started {
		return
	}
	harness.started = true
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		harness.collector.Run(ctx, harness.ticks)
		close(done)
	}()
	harness.t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(resourceWait):
			harness.t.Errorf("ctx 가 끝났는데 Run 이 돌아오지 않는다")
		}
	})
}

// tick — 주기 하나를 보낸다(수집기가 받을 때까지).
func (harness *resourceHarness) tick() {
	harness.t.Helper()
	harness.start()
	select {
	case harness.ticks <- harness.clock.wallNow():
	case <-time.After(resourceTickWait):
		harness.t.Fatalf("수집기가 주기(tick)를 받지 않는다 (stats %+v)", harness.collector.Stats())
	}
}

// collectOnce — 주기 하나를 보내고 그 수집이 실제로 끝날 때(InFlight 가 풀릴 때)까지 기다린다.
func (harness *resourceHarness) collectOnce() ResourceCollectorStats {
	harness.t.Helper()
	before := harness.collector.Stats()
	if before.InFlight {
		harness.t.Fatalf("앞 수집이 아직 돌고 있다 (stats %+v)", before)
	}
	harness.tick()
	return harness.waitStats("수집 하나가 끝남", func(stats ResourceCollectorStats) bool {
		return stats.Collections > before.Collections && !stats.InFlight
	})
}

// collect — 수집 하나를 돌리고 스트림에 들어간 항목을 돌려준다.
func (harness *resourceHarness) collect() resourceEntry {
	harness.t.Helper()
	harness.collectOnce()
	select {
	case entry := <-harness.stream.entries:
		return entry
	case <-time.After(resourceWait):
		harness.t.Fatalf("수집은 끝났는데 스트림 항목이 없다 (stats %+v)", harness.collector.Stats())
	}
	return resourceEntry{}
}

// waitStats — 조건이 맞을 때까지 기다린다.
func (harness *resourceHarness) waitStats(situation string, condition func(ResourceCollectorStats) bool) ResourceCollectorStats {
	harness.t.Helper()
	deadline := time.Now().Add(resourceWait)
	for {
		stats := harness.collector.Stats()
		if condition(stats) {
			return stats
		}
		if time.Now().After(deadline) {
			harness.t.Fatalf("%s: 기다린 상태가 오지 않았다 (stats %+v)", situation, stats)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// ===== 항목 읽기 =====

func (entry resourceEntry) field(name string) string {
	for _, field := range entry.fields {
		if field.Name == name {
			return field.Value
		}
	}
	return ""
}

// text — 항목 전체(칸 이름 · 값)를 한 글자열로 — "나가지 않는다"를 본문 전체에서 찾는다.
func (entry resourceEntry) text() string {
	var builder strings.Builder
	builder.WriteString(entry.stream)
	for _, field := range entry.fields {
		builder.WriteString("\n" + field.Name + "=" + field.Value)
	}
	return builder.String()
}

func (entry resourceEntry) body(t *testing.T) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal([]byte(entry.field("body")), &body); err != nil {
		t.Fatalf("body 가 JSON 객체가 아니다: %v %q", err, entry.field("body"))
	}
	return body
}

// member — 칸이 있어야 한다(null 이어도 칸은 있다).
func member(t *testing.T, object map[string]any, key, situation string) any {
	t.Helper()
	value, present := object[key]
	if !present {
		t.Fatalf("%s: 칸 %q 가 없다: %v", situation, key, object)
	}
	return value
}

func hostOf(t *testing.T, entry resourceEntry) map[string]any {
	t.Helper()
	host, ok := member(t, entry.body(t), "host", "body").(map[string]any)
	if !ok {
		t.Fatalf("host 가 객체가 아니다: %s", entry.field("body"))
	}
	return host
}

func listOf(t *testing.T, entry resourceEntry, key string) []map[string]any {
	t.Helper()
	raw, ok := member(t, entry.body(t), key, "body").([]any)
	if !ok {
		t.Fatalf("%s 가 배열이 아니다(null 이면 안 된다): %s", key, entry.field("body"))
	}
	objects := make([]map[string]any, 0, len(raw))
	for _, element := range raw {
		object, ok := element.(map[string]any)
		if !ok {
			t.Fatalf("%s 의 원소가 객체가 아니다: %v", key, element)
		}
		objects = append(objects, object)
	}
	return objects
}

func missingOf(t *testing.T, entry resourceEntry) []string {
	t.Helper()
	raw, ok := member(t, entry.body(t), "missing", "body").([]any)
	if !ok {
		t.Fatalf("missing 이 배열이 아니다(null 이면 안 된다): %s", entry.field("body"))
	}
	names := []string{}
	for _, element := range raw {
		name, ok := element.(string)
		if !ok {
			t.Fatalf("missing 의 원소가 글자가 아니다: %v", element)
		}
		names = append(names, name)
	}
	return names
}

func hasMissing(t *testing.T, entry resourceEntry, name string) bool {
	t.Helper()
	for _, missing := range missingOf(t, entry) {
		if missing == name {
			return true
		}
	}
	return false
}

func requireMissing(t *testing.T, entry resourceEntry, situation string, want ...string) {
	t.Helper()
	got := missingOf(t, entry)
	if strings.Join(sortedCopy(got), ",") != strings.Join(sortedCopy(want), ",") {
		t.Fatalf("%s: missing 이 %v 여야 한다: %v", situation, want, got)
	}
}

func sortedCopy(values []string) []string {
	copied := append([]string{}, values...)
	for left := 0; left < len(copied); left++ {
		for right := left + 1; right < len(copied); right++ {
			if copied[right] < copied[left] {
				copied[left], copied[right] = copied[right], copied[left]
			}
		}
	}
	return copied
}

// requireNumber — 소수 반올림(백분율 한 자리 · 바이트 정수)을 허용하는 tolerance 안에서 같은가.
func requireNumber(t *testing.T, situation string, value any, want, tolerance float64) {
	t.Helper()
	number, ok := value.(float64)
	if !ok {
		t.Fatalf("%s: 숫자여야 한다(%v 근처): %#v", situation, want, value)
	}
	if math.Abs(number-want) > tolerance {
		t.Fatalf("%s: %v 여야 한다: %v", situation, want, number)
	}
}

func requireNull(t *testing.T, situation string, value any) {
	t.Helper()
	if value != nil {
		t.Fatalf("%s: null 이어야 한다: %#v", situation, value)
	}
}

func requireKeys(t *testing.T, situation string, object map[string]any, keys ...string) {
	t.Helper()
	var got []string
	for key := range object {
		got = append(got, key)
	}
	if strings.Join(sortedCopy(got), ",") != strings.Join(sortedCopy(keys), ",") {
		t.Fatalf("%s: 칸이 %v 여야 한다: %v", situation, sortedCopy(keys), sortedCopy(got))
	}
}

// ===== 컨테이너 만들기 =====

func containerID(seed string) string {
	return (seed + strings.Repeat("0", 64))[:64]
}

func projectContainer(seed, name, service string) *fakeContainer {
	return &fakeContainer{id: containerID(seed), name: name, state: "running",
		labels: map[string]string{composeProjectLabelForTest: "study-note-deploy-system-" + service, "com.docker.compose.service": name}}
}

const composeProjectLabelForTest = "com.docker.compose.project"

func findContainer(t *testing.T, containers []map[string]any, name string) map[string]any {
	t.Helper()
	for _, container := range containers {
		if container["name"] == name {
			return container
		}
	}
	t.Fatalf("컨테이너 %q 가 없다: %v", name, containers)
	return nil
}

// gpuContainerStateBody — 컨테이너 조회의 State(도커 29.1 실측 꼴).
func gpuContainerStateBody(state string) map[string]any {
	switch state {
	case "exited":
		return map[string]any{"Status": "exited", "Running": false, "Paused": false, "Restarting": false, "Pid": 0, "ExitCode": 0}
	case "paused":
		return map[string]any{"Status": "paused", "Running": true, "Paused": true, "Restarting": false, "Pid": 4242}
	case "restarting":
		return map[string]any{"Status": "restarting", "Running": true, "Paused": false, "Restarting": true, "Pid": 4242}
	}
	return map[string]any{"Status": "running", "Running": true, "Paused": false, "Restarting": false, "Pid": 4242}
}
