package agent

// 자원 지표 수집 — 30초마다 서버 전체 · 이 프로젝트의 컨테이너 · GPU 를 읽어 Redis 스트림에 항목 하나로 보낸다.
// 계약: docs/cross-cutting/plans/2026-11-24/resource-metrics/requirement-spec.md §2.1 · §2.3 · §2.5 · §10.1 · §10.2 · §11.1
// (뒤의 절이 앞의 절보다 우선).
//
// 불변식
//   - 배포를 막지 않는다: busy 잠금을 잡지 않는다. 수집의 어떤 실패·패닉도 agent 를 죽이지 않고 다음 주기가 돈다.
//   - 읽기만: 호스트에 쓰지 않는다. 도커에는 GET 조회와, GPU 용으로 설정된 컨테이너 하나 안의 고정 조회 명령(gpuQueryCommand) 실행뿐.
//     이 프로젝트가 아닌 컨테이너는 조회하지도 싣지도 않는다. 환경 변수 · 명령줄 · 라벨은 싣지 않는다.
//   - 한 번의 수집 = 전체 deadline(10초). 도커 조회는 dockerDeadline, GPU 명령은 gpuDeadline 안에서, Redis 전송은 남은 기한 안에서 —
//     기한은 호출마다 ctx 로 전달한다. 도커·GPU 가 멈춰도 나머지 갈래는 그 수집에서 나간다.
//   - 겹침 방지: 앞 수집이 실제로 끝나지 않았으면 다음 주기는 시작하지 않고 Skipped 를 센다.
//   - 사용률 · 초당 값은 앞 표본과의 차이다. 앞 표본은 장치별 · 방향별 · 컨테이너 ID 별. 간격은 단조 시계(monotonicNow)로 잰다.
//     첫 수집 · 기준이 바뀜(새 장치 · 새 컨테이너 ID) · 누적 값이 줄어듦 · 간격이 rateGapLimit 초과 → 그 몫만 빼고 기준을 새로 잡는다.
//     합칠 몫이 하나도 없으면 그 값은 null.
//   - Redis 에 넣지 못하면 SendFailures 를 센다. 로그는 연속 실패의 첫 번에 1줄(warning), 회복한 첫 성공에 1줄(info — 그동안의 실패 횟수).

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/junhyeong9812/study-note-deploy-system-ci-cd/internal/shared"
)

const (
	resourceMetricsInterval    = 30 * time.Second // 수집 주기(StartResourceMetrics 의 ticker)
	resourceCollectionDeadline = 10 * time.Second // 한 번의 수집 전체
	resourceDockerDeadline     = 4 * time.Second  // 도커 조회 전체(목록 · 컨테이너별 조회)
	resourceGPUDeadline        = 2 * time.Second  // GPU 명령(만들기 · 시작 · 종료 코드 조회)
	resourceRateGapLimit       = 2 * time.Minute  // 앞 표본과 이보다 멀면 차이 값은 null
	resourceContainerLimit     = 50               // 넘으면 이름순 앞 50개 + missing 에 containers_truncated
	resourceBodyByteLimit      = 64 << 10         // body JSON 상한 — 넘으면 뒤(이름순) 컨테이너부터 빼고 containers_truncated
	resourceStreamMaxLength    = 20000            // XADD … MAXLEN ~ 20000
	resourceStreamKind         = "resource"       // 칸 kind — 같은 길을 다른 종류가 쓸 수 있게
	resourceStreamVersion      = "1"              // 칸 v
	resourceProjectPrefix      = "study-note-deploy-system-"
	composeProjectLabel        = "com.docker.compose.project"
	defaultMetricsStream       = "metrics"
)

// gpuQueryCommand — GPU 용 컨테이너 안에서 실행하는 유일한 명령. 줄마다 "번호, 사용률, 메모리 사용(MiB), 메모리 전체(MiB), 온도".
var gpuQueryCommand = []string{"nvidia-smi",
	"--query-gpu=index,utilization.gpu,memory.used,memory.total,temperature.gpu", "--format=csv,noheader,nounits"}

// ResourceMetricsSettings — 환경 변수에서 읽는 수집 설정.
type ResourceMetricsSettings struct {
	RedisAddress    string   // LOG_REDIS_URL 의 host:port — 비면 수집하지 않는다
	Stream          string   // METRICS_STREAM, 기본 "metrics"
	Server          string   // SERVER_NAME — 없으면 로그와 같은 규칙("ci-" + MODE)
	ExtraContainers []string // METRICS_EXTRA_CONTAINERS — 쉼표로 나눈 컨테이너 이름(앞뒤 공백 제거, 빈 칸 버림). compose 프로젝트가 없는 이 프로젝트의 컨테이너
	GPUContainer    string   // METRICS_GPU_CONTAINER — 비면 GPU 를 모으지 않는다(gpus: [], missing 에 넣지 않음)
}

func LoadResourceMetricsSettings() ResourceMetricsSettings {
	stream := os.Getenv("METRICS_STREAM")
	if stream == "" {
		stream = defaultMetricsStream
	}
	server := os.Getenv("SERVER_NAME")
	if server == "" {
		server = "ci-" + os.Getenv("MODE")
	}
	var extraContainers []string
	for _, name := range strings.Split(os.Getenv("METRICS_EXTRA_CONTAINERS"), ",") {
		if name = strings.TrimSpace(name); name != "" {
			extraContainers = append(extraContainers, name)
		}
	}
	return ResourceMetricsSettings{RedisAddress: shared.RedisAddress(os.Getenv("LOG_REDIS_URL")), Stream: stream, Server: server,
		ExtraContainers: extraContainers, GPUContainer: strings.TrimSpace(os.Getenv("METRICS_GPU_CONTAINER"))}
}

// ResourceCollectorStats — 수집기의 누적 횟수와 지금 상태.
type ResourceCollectorStats struct {
	Collections  int64 // 시작한 수집 — 시작할 때 센다
	Sent         int64 // 스트림에 넣은 항목
	SendFailures int64 // 스트림에 넣지 못한 항목
	Skipped      int64 // 앞 수집이 끝나지 않아 건너뛴 주기
	Panics       int64 // 수집 · 전송 중 되살린 패닉
	GPUSkipped   int64 // 앞 GPU 실행이 남아 있을 수 있어(도는 중 · 상태를 알 수 없음) 그 수집의 GPU 조회를 건너뛴 횟수(§12)
	InFlight     bool  // 수집이 도는 중 — 시작부터 그 수집의 모든 일(전송 · 패닉 회복 포함)이 실제로 끝날 때까지 참
}

// ResourceCollector — 아래 칸은 테스트가 바꾸는 이음매다(newResourceCollector 가 운영 값으로 채운다).
type ResourceCollector struct {
	settings ResourceMetricsSettings

	procRoot     string                                          // "/proc" — stat · loadavg · meminfo · net/dev · diskstats 를 이 아래에서 읽는다
	diskPath     string                                          // "/" — 루트 디스크 용량을 잴 경로
	statfs       func(path string, stat *syscall.Statfs_t) error // syscall.Statfs. 전체 = Blocks×Bsize, 사용 = (Blocks−Bfree)×Bsize
	docker       *dockerAPI                                      // 도커 엔진 API(유닉스 소켓)
	stream       shared.StreamWriter                             // shared.NewRedisStream(settings.RedisAddress)
	monotonicNow func() time.Duration                            // 간격을 재는 단조 시계(어떤 원점에서 지난 시간)
	wallNow      func() time.Time                                // 칸 at(epoch 밀리초)을 찍는 벽시계
	logf         func(message, level string)                     // agent 로그 — 운영은 agent.logger.Log("metrics", message, level)

	deadline       time.Duration // resourceCollectionDeadline
	dockerDeadline time.Duration // resourceDockerDeadline
	gpuDeadline    time.Duration // resourceGPUDeadline

	mutex sync.Mutex
	stats ResourceCollectorStats

	// 아래는 한 번에 하나만 도는 수집(InFlight)만 읽고 쓴다 — 겹침 방지가 차례를 보장한다.
	previousCollection *time.Duration                      // 앞 수집의 단조 시각(interval_ms)
	previousCPU        *cpuSample                          // 서버 전체 CPU 의 앞 표본
	previousCounters   map[string]map[string]counterSample // 갈래(net_rx · net_tx · io_read · io_write) → 장치 → 앞 표본
	previousContainers map[string]containerSample          // 컨테이너 전체 ID → 앞 표본
	failureStreak      int64                               // 지금 이어지는 전송 실패 횟수(mutex 아래)
	skipping           bool                                // 지금 이어지는 건너뜀(mutex 아래) — 첫 번에만 로그
	gpuExecID          string                              // 끝났다고 확인하지 못한 GPU 조회용 실행(§12) — 있으면 새로 만들지 않는다
	gpuSkipStreak      int64                               // 지금 이어지는 GPU 건너뜀
	gpuSkipStartedAt   time.Duration                       // 그 건너뜀이 시작된 단조 시각
	gpuSkipLoggedAt    time.Duration                       // 그 건너뜀의 마지막 로그 단조 시각
}

type cpuSample struct {
	busy, total uint64
	at          time.Duration
}

type counterSample struct {
	value uint64
	at    time.Duration
}

type containerSample struct {
	totalUsage uint64 // 나노초 누적
	at         time.Duration
}

// StartResourceMetrics — 설정을 읽어 수집을 시작한다(resourceMetricsInterval 마다 — 첫 수집은 한 주기 뒤).
// Redis 주소가 비면 시작하지 않고 로그 1줄을 남긴 뒤 nil. ctx 가 끝나면 멈춘다.
func (agent *Agent) StartResourceMetrics(ctx context.Context) *ResourceCollector {
	settings := LoadResourceMetricsSettings()
	if settings.RedisAddress == "" {
		agent.logger.Log("metrics", "resource metrics disabled: LOG_REDIS_URL is empty", "info")
		return nil
	}
	collector := agent.newResourceCollector(settings)
	ticker := time.NewTicker(resourceMetricsInterval)
	go func() {
		defer ticker.Stop()
		collector.Run(ctx, ticker.C)
	}()
	agent.logger.Log("metrics", fmt.Sprintf("resource metrics every %s to stream %s (gpu container %q, extra containers %d)",
		resourceMetricsInterval, settings.Stream, settings.GPUContainer, len(settings.ExtraContainers)), "info")
	return collector
}

// newResourceCollector — 운영 이음매(/proc · "/" · syscall.Statfs · 도커 소켓 · Redis · 시계 · agent 로그)와 기본 기한으로 채운 수집기.
func (agent *Agent) newResourceCollector(settings ResourceMetricsSettings) *ResourceCollector {
	origin := time.Now()
	return &ResourceCollector{settings: settings,
		procRoot: "/proc", diskPath: "/", statfs: syscall.Statfs,
		docker: newDockerAPI(dockerSocketPath), stream: shared.NewRedisStream(settings.RedisAddress),
		monotonicNow: func() time.Duration { return time.Since(origin) }, // time.Since 는 단조 시계로 잰다
		wallNow:      time.Now,
		logf:         func(message, level string) { agent.logger.Log("metrics", message, level) },
		deadline:     resourceCollectionDeadline, dockerDeadline: resourceDockerDeadline, gpuDeadline: resourceGPUDeadline}
}

// Run — ticks 가 올 때마다 수집 하나를 시작한다(앞 수집이 끝나지 않았으면 건너뛰고 센다). ctx 가 끝나거나 ticks 가 닫히면 돌아온다.
// 수집 자체는 따로 돌아 Run 을 붙잡지 않는다.
func (collector *ResourceCollector) Run(ctx context.Context, ticks <-chan time.Time) {
	for {
		select {
		case <-ctx.Done():
			return
		case _, open := <-ticks:
			if !open {
				return
			}
			collector.startCollection(ctx)
		}
	}
}

func (collector *ResourceCollector) Stats() ResourceCollectorStats {
	collector.mutex.Lock()
	defer collector.mutex.Unlock()
	return collector.stats
}

// startCollection — 앞 수집이 실제로 끝났으면 새 수집을 따로 돌리고, 아니면 건너뛰고 센다.
func (collector *ResourceCollector) startCollection(ctx context.Context) {
	collector.mutex.Lock()
	if collector.stats.InFlight {
		collector.stats.Skipped++
		firstSkip := !collector.skipping
		collector.skipping = true
		skipped := collector.stats.Skipped
		collector.mutex.Unlock()
		if firstSkip {
			collector.logf(fmt.Sprintf("resource metrics skipped: previous collection has not finished (skipped %d)", skipped), "warning")
		}
		return
	}
	collector.stats.InFlight = true
	collector.stats.Collections++
	collector.skipping = false
	collector.mutex.Unlock()

	go func() {
		defer func() {
			recovered := recover()
			if recovered != nil {
				collector.logf(fmt.Sprintf("resource metrics collection panic: %v", recovered), "error")
			}
			collector.mutex.Lock()
			if recovered != nil {
				collector.stats.Panics++
			}
			collector.stats.InFlight = false
			collector.mutex.Unlock()
		}()
		collector.collectAndSend(ctx)
	}()
}

// collectAndSend — 수집 하나: 세 갈래를 읽어 항목 하나로 보낸다. 전체가 collector.deadline 안이다.
func (collector *ResourceCollector) collectAndSend(parent context.Context) {
	ctx, cancel := context.WithTimeout(parent, collector.deadline)
	defer cancel()
	at := collector.wallNow()
	now := collector.monotonicNow()

	body := ResourceBody{Containers: []ContainerResource{}, GPUs: []GPUResource{}, Missing: []string{}}
	if collector.previousCollection != nil {
		if interval := (now - *collector.previousCollection).Milliseconds(); interval >= 0 {
			body.IntervalMs = &interval
		}
	}
	collector.previousCollection = &now

	if !collector.readHost(&body.Host, now) {
		body.Missing = append(body.Missing, missingHost)
	}
	containers, truncated, err := collector.readContainers(ctx)
	if err != nil {
		body.Missing = append(body.Missing, missingContainers)
	} else {
		body.Containers = containers
	}
	if collector.settings.GPUContainer != "" {
		if gpus, err := collector.readGPUs(ctx); err != nil {
			body.Missing = append(body.Missing, missingGPUs)
		} else {
			body.GPUs = gpus
		}
	}
	encoded, err := encodeResourceBody(body, truncated)
	if err == nil {
		err = ctx.Err() // 기한이 지난 수집은 버린다(보내지 못한 것으로 센다)
	}
	if err == nil {
		err = collector.stream.Append(ctx, collector.settings.Stream, resourceStreamMaxLength, []shared.StreamField{
			{Name: "kind", Value: resourceStreamKind}, {Name: "v", Value: resourceStreamVersion},
			{Name: "server", Value: collector.settings.Server}, {Name: "at", Value: strconv.FormatInt(at.UnixMilli(), 10)},
			{Name: "body", Value: encoded}})
	}
	collector.recordSend(err)
}

// encodeResourceBody — body JSON. 컨테이너가 50개를 넘었거나(truncated) 64KB 를 넘으면 이름순 뒤에서부터 빼고 containers_truncated.
func encodeResourceBody(body ResourceBody, truncated bool) (string, error) {
	for {
		if truncated && !containsString(body.Missing, missingContainersTruncated) {
			body.Missing = append(body.Missing, missingContainersTruncated)
		}
		encoded, err := json.Marshal(body)
		if err != nil {
			return "", err
		}
		if len(encoded) <= resourceBodyByteLimit {
			return string(encoded), nil
		}
		if len(body.Containers) == 0 {
			return "", fmt.Errorf("resource body %d bytes exceeds limit", len(encoded))
		}
		body.Containers = body.Containers[:len(body.Containers)-1]
		truncated = true
	}
}

func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

// recordSend — 전송 결과를 센다. 로그는 연속 실패의 첫 번(warning)과 회복한 첫 성공(info)에만.
func (collector *ResourceCollector) recordSend(err error) {
	collector.mutex.Lock()
	var message, level string
	if err == nil {
		collector.stats.Sent++
		if collector.failureStreak > 0 {
			message, level = fmt.Sprintf("resource metrics stream recovered after %d failed sends", collector.failureStreak), "info"
		}
		collector.failureStreak = 0
	} else {
		collector.stats.SendFailures++
		collector.failureStreak++
		if collector.failureStreak == 1 {
			message, level = "resource metrics stream send failed (further failures are counted, not logged): "+err.Error(), "warning"
		}
	}
	collector.mutex.Unlock()
	if message != "" {
		collector.logf(message, level)
	}
}

// ===== 서버 전체 (/proc · statfs) =====

// readHost — 서버 전체 값. 하나라도 못 읽으면 false(그 값만 null, 앞 표본은 그대로 둔다).
func (collector *ResourceCollector) readHost(host *HostResource, now time.Duration) bool {
	complete := true
	if busy, total, err := readProcCPU(filepath.Join(collector.procRoot, "stat")); err != nil {
		complete = false
	} else {
		host.CPUPct = collector.hostCPURate(busy, total, now)
	}
	if load1, err := readProcLoad1(filepath.Join(collector.procRoot, "loadavg")); err != nil {
		complete = false
	} else {
		host.Load1 = &load1
	}
	if total, available, err := readProcMemory(filepath.Join(collector.procRoot, "meminfo")); err != nil {
		complete = false
	} else {
		host.MemTotal = &total
		host.MemUsed = subtractOrNil(total, available)
	}
	var stat syscall.Statfs_t
	if err := collector.statfs(collector.diskPath, &stat); err != nil {
		complete = false
	} else {
		blockSize := uint64(stat.Bsize)
		total := stat.Blocks * blockSize
		host.DiskTotal = &total
		host.DiskUsed = subtractOrNil(stat.Blocks*blockSize, stat.Bfree*blockSize)
	}
	if received, transmitted, err := readProcNetwork(filepath.Join(collector.procRoot, "net", "dev")); err != nil {
		complete = false
	} else {
		host.NetRxBps = collector.counterRate("net_rx", received, 1, now)
		host.NetTxBps = collector.counterRate("net_tx", transmitted, 1, now)
	}
	if sectorsRead, sectorsWritten, err := readProcDisks(filepath.Join(collector.procRoot, "diskstats")); err != nil {
		complete = false
	} else {
		host.IOReadBps = collector.counterRate("io_read", sectorsRead, diskSectorBytes, now)
		host.IOWriteBps = collector.counterRate("io_write", sectorsWritten, diskSectorBytes, now)
	}
	return complete
}

const diskSectorBytes = 512

// hostCPURate — 앞 표본과의 차이로 바쁨 비율(0~100). 기준이 없거나 · 줄었거나 · 2분 초과 · 움직이지 않았으면 null.
func (collector *ResourceCollector) hostCPURate(busy, total uint64, now time.Duration) *float64 {
	previous := collector.previousCPU
	collector.previousCPU = &cpuSample{busy: busy, total: total, at: now}
	if previous == nil || !rateGapAllowed(now-previous.at) || total < previous.total || busy < previous.busy || total == previous.total {
		return nil
	}
	percent := math.Min(100, roundTenth(float64(busy-previous.busy)/float64(total-previous.total)*100))
	return &percent
}

// counterRate — 장치별 앞 표본과의 차이를 합한 초당 값(× unit 바이트). 앞 표본이 없거나 · 줄었거나 · 2분 초과인 장치는 그 몫만 뺀다.
// 합칠 몫이 없으면 null. 지금 값이 새 기준이 된다(사라진 장치의 기준은 버린다).
func (collector *ResourceCollector) counterRate(group string, current map[string]uint64, unit uint64, now time.Duration) *uint64 {
	if collector.previousCounters == nil {
		collector.previousCounters = map[string]map[string]counterSample{}
	}
	previous := collector.previousCounters[group]
	next := make(map[string]counterSample, len(current))
	var sum float64
	contributed := false
	for name, value := range current {
		next[name] = counterSample{value: value, at: now}
		sample, known := previous[name]
		if !known || value < sample.value || !rateGapAllowed(now-sample.at) {
			continue
		}
		sum += float64((value-sample.value)*unit) / (now - sample.at).Seconds()
		contributed = true
	}
	collector.previousCounters[group] = next
	if !contributed {
		return nil
	}
	rounded := uint64(math.Round(sum))
	return &rounded
}

// rateGapAllowed — 차이를 낼 수 있는 간격인가(0 초과 · resourceRateGapLimit 이하).
func rateGapAllowed(elapsed time.Duration) bool {
	return elapsed > 0 && elapsed <= resourceRateGapLimit
}

func roundTenth(value float64) float64 {
	return math.Round(value*10) / 10
}

// subtractOrNil — total − part. 음수가 되면 null.
func subtractOrNil(total, part uint64) *uint64 {
	if part > total {
		return nil
	}
	difference := total - part
	return &difference
}

// readProcCPU — /proc/stat 첫 줄(cpu)의 user..steal(앞 8칸) 합이 전체, idle + iowait 를 뺀 것이 바쁨.
// guest · guest_nice 는 이미 user · nice 에 들어 있어 더하지 않는다.
func readProcCPU(path string) (busy, total uint64, err error) {
	file, err := os.Open(path)
	if err != nil {
		return 0, 0, err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) == 0 || fields[0] != "cpu" {
			continue
		}
		if len(fields) < 5 {
			return 0, 0, fmt.Errorf("stat: short cpu line")
		}
		var idle uint64
		for index, field := range fields[1:min(len(fields), 9)] {
			value, err := strconv.ParseUint(field, 10, 64)
			if err != nil {
				return 0, 0, fmt.Errorf("stat: %w", err)
			}
			total += value
			if index == 3 || index == 4 { // idle · iowait
				idle += value
			}
		}
		return total - idle, total, nil
	}
	if err := scanner.Err(); err != nil {
		return 0, 0, err
	}
	return 0, 0, fmt.Errorf("stat: no cpu line")
}

func readProcLoad1(path string) (float64, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	fields := strings.Fields(string(content))
	if len(fields) == 0 {
		return 0, fmt.Errorf("loadavg: empty")
	}
	load1, err := strconv.ParseFloat(fields[0], 64)
	if err != nil || load1 < 0 || math.IsNaN(load1) || math.IsInf(load1, 0) {
		return 0, fmt.Errorf("loadavg: bad value")
	}
	return load1, nil
}

// readProcMemory — MemTotal · MemAvailable(kB = 1024 바이트).
func readProcMemory(path string) (total, available uint64, err error) {
	file, err := os.Open(path)
	if err != nil {
		return 0, 0, err
	}
	defer file.Close()
	found := 0
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 2 || (fields[0] != "MemTotal:" && fields[0] != "MemAvailable:") {
			continue
		}
		kilobytes, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			return 0, 0, fmt.Errorf("meminfo: %w", err)
		}
		if fields[0] == "MemTotal:" {
			total = kilobytes * 1024
		} else {
			available = kilobytes * 1024
		}
		found++
	}
	if err := scanner.Err(); err != nil {
		return 0, 0, err
	}
	if found != 2 {
		return 0, 0, fmt.Errorf("meminfo: MemTotal or MemAvailable missing")
	}
	return total, available, nil
}

// virtualNetworkPrefixes — 세지 않는 가상 장치(호스트 장치와 겹쳐 센다).
var virtualNetworkPrefixes = []string{"veth", "br-", "docker", "cni", "flannel"}

// readProcNetwork — 장치별 누적 수신 · 송신 바이트(가상 장치 제외).
func readProcNetwork(path string) (received, transmitted map[string]uint64, err error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	received, transmitted = map[string]uint64{}, map[string]uint64{}
	for _, line := range strings.Split(string(content), "\n") {
		name, counters, found := strings.Cut(line, ":")
		if !found || strings.Contains(name, "|") {
			continue // 머리줄
		}
		name = strings.TrimSpace(name)
		if name == "lo" || hasAnyPrefix(name, virtualNetworkPrefixes) {
			continue
		}
		fields := strings.Fields(counters)
		if len(fields) < 9 {
			return nil, nil, fmt.Errorf("net/dev: short line for %s", name)
		}
		rx, rxErr := strconv.ParseUint(fields[0], 10, 64)
		tx, txErr := strconv.ParseUint(fields[8], 10, 64)
		if rxErr != nil || txErr != nil {
			return nil, nil, fmt.Errorf("net/dev: bad counter for %s", name)
		}
		received[name], transmitted[name] = rx, tx
	}
	return received, transmitted, nil
}

func hasAnyPrefix(value string, prefixes []string) bool {
	for _, prefix := range prefixes {
		if strings.HasPrefix(value, prefix) {
			return true
		}
	}
	return false
}

// wholeDiskName — 통째 디스크만(파티션 · loop · ram · dm- · md · sr · zram 은 겹쳐 세거나 실제 디스크가 아니다).
var wholeDiskName = regexp.MustCompile(`^(sd[a-z]+|vd[a-z]+|xvd[a-z]+|hd[a-z]+|nvme[0-9]+n[0-9]+|mmcblk[0-9]+)$`)

// readProcDisks — 통째 디스크별 누적 읽은 · 쓴 섹터.
func readProcDisks(path string) (sectorsRead, sectorsWritten map[string]uint64, err error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	sectorsRead, sectorsWritten = map[string]uint64{}, map[string]uint64{}
	for _, line := range strings.Split(string(content), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 || !wholeDiskName.MatchString(fields[2]) {
			continue
		}
		if len(fields) < 10 {
			return nil, nil, fmt.Errorf("diskstats: short line for %s", fields[2])
		}
		read, readErr := strconv.ParseUint(fields[5], 10, 64)
		written, writeErr := strconv.ParseUint(fields[9], 10, 64)
		if readErr != nil || writeErr != nil {
			return nil, nil, fmt.Errorf("diskstats: bad counter for %s", fields[2])
		}
		sectorsRead[fields[2]], sectorsWritten[fields[2]] = read, written
	}
	return sectorsRead, sectorsWritten, nil
}

// ===== 컨테이너 (도커 API) =====

// readContainers — 이 프로젝트의 컨테이너(이름순, 최대 resourceContainerLimit). 목록이 실패하면 오류(갈래 전체 결측).
// 그 밖의 컨테이너는 목록에서 걸러 조회하지 않는다. 컨테이너 하나의 조회 실패는 그 값만 null.
func (collector *ResourceCollector) readContainers(parent context.Context) ([]ContainerResource, bool, error) {
	ctx, cancel := context.WithTimeout(parent, collector.dockerDeadline)
	defer cancel()
	listed, err := collector.docker.listContainers(ctx)
	if err != nil {
		return nil, false, err
	}
	type ownContainer struct {
		id, name, project, state string
	}
	var own []ownContainer
	for _, container := range listed {
		if len(container.Names) == 0 || container.ID == "" {
			continue
		}
		name := strings.TrimPrefix(container.Names[0], "/")
		project := container.Labels[composeProjectLabel]
		if strings.HasPrefix(project, resourceProjectPrefix) {
			own = append(own, ownContainer{container.ID, name, project, container.State})
		} else if containsString(collector.settings.ExtraContainers, name) {
			own = append(own, ownContainer{container.ID, name, "", container.State})
		}
	}
	sort.Slice(own, func(left, right int) bool { return own[left].name < own[right].name })
	truncated := len(own) > resourceContainerLimit
	if truncated {
		own = own[:resourceContainerLimit]
	}

	nextSamples := make(map[string]containerSample, len(own))
	results := make([]ContainerResource, 0, len(own))
	for _, container := range own {
		result := ContainerResource{Name: container.name, ID: container.id[:min(12, len(container.id))],
			Project: container.project, State: container.state}
		if inspect, err := collector.docker.inspectContainer(ctx, container.id); err == nil {
			restarts := inspect.RestartCount
			if restarts >= 0 {
				result.Restarts = &restarts
			}
		}
		previous, known := collector.previousContainers[container.id]
		if container.state == "running" {
			if stats, err := collector.docker.containerStats(ctx, container.id); err == nil {
				now := collector.monotonicNow()
				usage := stats.CPUStats.CPUUsage.TotalUsage
				nextSamples[container.id] = containerSample{totalUsage: usage, at: now}
				if known && usage >= previous.totalUsage && rateGapAllowed(now-previous.at) {
					percent := roundTenth(float64(usage-previous.totalUsage) / float64(now-previous.at) * 100)
					result.CPUPct = &percent
				}
				result.MemUsed = containerMemoryUsed(stats)
				known = false // 새 표본을 기준으로 삼았다
			}
		}
		if known { // 이번 조회가 실패했거나 멈춰 있다 — 앞 기준을 그대로 둔다
			nextSamples[container.id] = previous
		}
		results = append(results, result)
	}
	collector.previousContainers = nextSamples
	return results, truncated, nil
}

// containerMemoryUsed — usage − 비활성 파일 캐시(cgroup v1 은 total_inactive_file, v2 는 inactive_file). docker stats 와 같은 계산.
func containerMemoryUsed(stats dockerContainerStats) *uint64 {
	memory := stats.MemoryStats
	if memory.Usage == nil {
		return nil
	}
	inactive := uint64(0)
	if memory.Stats.TotalInactiveFile != nil {
		inactive = *memory.Stats.TotalInactiveFile
	} else if memory.Stats.InactiveFile != nil {
		inactive = *memory.Stats.InactiveFile
	}
	return subtractOrNil(*memory.Usage, inactive)
}

// ===== GPU (설정된 컨테이너 안의 nvidia-smi) =====

const mebibyte = 1048576

// readGPUs — 고정 명령 하나(§12 — 조회용 실행은 한 번에 하나만 살아 있다). 전부 GPU 기한 안이다.
//   - 남은 실행(gpuExecID)이 있으면 먼저 그 상태를 묻는다: 끝남 · 사라짐(404) → 잊고 새로 조회, 도는 중 · 알 수 없음 → 건너뛴다.
//   - 실행은 만든 즉시 기억한다(시작 전에). 끝났다고 확인한 뒤에만 잊는다 — 시작 오류 · 기한 초과 · 끝내 도는 중이면 남은 실행이다.
//   - 종료 코드가 0 이 아니거나 · 어느 줄이든 꼴이 틀리거나 · 줄이 없으면 오류(갈래 전체 결측).
func (collector *ResourceCollector) readGPUs(parent context.Context) ([]GPUResource, error) {
	ctx, cancel := context.WithTimeout(parent, collector.gpuDeadline)
	defer cancel()
	if collector.gpuExecID != "" {
		running, _, err := collector.docker.inspectExec(ctx, collector.gpuExecID)
		if (err != nil && !errors.Is(err, errDockerNotFound)) || (err == nil && running) { // 도는 중 · 알 수 없음
			collector.noteGPUSkipped()
			return nil, errors.New("gpu query skipped: previous exec may still be running")
		}
		collector.gpuExecID = "" // 끝났거나 사라졌다
		collector.noteGPUResumed()
	}

	id, err := collector.docker.createExec(ctx, collector.settings.GPUContainer, gpuQueryCommand)
	if err != nil {
		return nil, err
	}
	collector.gpuExecID = id
	output, err := collector.docker.startExec(ctx, id)
	if err != nil {
		return nil, err
	}
	exitCode, err := collector.waitGPUExecExit(ctx, id)
	if err != nil {
		return nil, err
	}
	if exitCode != 0 {
		return nil, fmt.Errorf("gpu query exit code %d", exitCode)
	}
	return parseGPUQuery(output)
}

// waitGPUExecExit — 출력이 끝난 뒤 실행의 종료 상태. 도커의 종료 처리가 출력 닫힘보다 늦을 수 있어 ctx 기한 안에서 짧게 다시 묻는다.
// 끝났으면(또는 사라졌으면) 기억을 지운다. 기한까지 도는 중이거나 상태를 알 수 없으면 기억을 남기고 오류.
func (collector *ResourceCollector) waitGPUExecExit(ctx context.Context, id string) (int, error) {
	for {
		running, exitCode, err := collector.docker.inspectExec(ctx, id)
		if errors.Is(err, errDockerNotFound) {
			collector.gpuExecID = ""
			return 0, err
		}
		if err != nil {
			return 0, err
		}
		if !running {
			collector.gpuExecID = ""
			return exitCode, nil
		}
		select {
		case <-ctx.Done():
			return 0, fmt.Errorf("gpu exec still running: %w", ctx.Err())
		case <-time.After(gpuExecPollInterval):
		}
	}
}

const gpuExecPollInterval = 50 * time.Millisecond // 끝난 직후 종료 상태를 다시 묻는 간격
const gpuStuckLogInterval = time.Hour             // GPU 건너뜀이 이어지는 동안 로그 간격

// noteGPUSkipped — 건너뜀을 센다. 로그는 건너뜀이 시작될 때 1줄, 이어지는 동안 앞 줄로부터 1시간마다 1줄(수집기의 단조 시계).
func (collector *ResourceCollector) noteGPUSkipped() {
	collector.mutex.Lock()
	collector.stats.GPUSkipped++
	collector.mutex.Unlock()
	now := collector.monotonicNow()
	collector.gpuSkipStreak++
	if collector.gpuSkipStreak == 1 {
		collector.gpuSkipStartedAt, collector.gpuSkipLoggedAt = now, now
		collector.logf(fmt.Sprintf("resource metrics gpu query skipped: previous nvidia-smi exec in %s is still running or unknown (further skips are counted)",
			collector.settings.GPUContainer), "warning")
		return
	}
	if now-collector.gpuSkipLoggedAt >= gpuStuckLogInterval {
		collector.gpuSkipLoggedAt = now
		collector.logf(fmt.Sprintf("resource metrics gpu query still skipped: %d collections over %s (previous nvidia-smi exec in %s)",
			collector.gpuSkipStreak, (now-collector.gpuSkipStartedAt).Round(time.Second), collector.settings.GPUContainer), "warning")
	}
}

// noteGPUResumed — 남은 실행이 끝난(사라진) 것을 확인했다. 건너뜀이 이어지던 중이면 1줄.
func (collector *ResourceCollector) noteGPUResumed() {
	if collector.gpuSkipStreak == 0 {
		return
	}
	collector.logf(fmt.Sprintf("resource metrics gpu query resumed after %d skipped collections over %s",
		collector.gpuSkipStreak, (collector.monotonicNow()-collector.gpuSkipStartedAt).Round(time.Second)), "info")
	collector.gpuSkipStreak = 0
}

// parseGPUQuery — 줄마다 "번호, 사용률, 메모리 사용(MiB), 메모리 전체(MiB), 온도". 출력 글자는 오류에 싣지 않는다.
func parseGPUQuery(output string) ([]GPUResource, error) {
	gpus := []GPUResource{}
	for lineNumber, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Split(line, ",")
		if len(fields) != 5 {
			return nil, fmt.Errorf("gpu query line %d: %d fields", lineNumber+1, len(fields))
		}
		for index := range fields {
			fields[index] = strings.TrimSpace(fields[index])
		}
		gpuIndex, indexErr := strconv.ParseUint(fields[0], 10, 31)
		utilization, utilizationErr := strconv.ParseFloat(fields[1], 64)
		memoryUsed, usedErr := strconv.ParseUint(fields[2], 10, 40)
		memoryTotal, totalErr := strconv.ParseUint(fields[3], 10, 40)
		temperature, temperatureErr := strconv.ParseInt(fields[4], 10, 64)
		if indexErr != nil || utilizationErr != nil || usedErr != nil || totalErr != nil || temperatureErr != nil ||
			math.IsNaN(utilization) || math.IsInf(utilization, 0) {
			return nil, fmt.Errorf("gpu query line %d: not a number", lineNumber+1)
		}
		utilization = math.Max(0, math.Min(100, roundTenth(utilization)))
		usedBytes, totalBytes := memoryUsed*mebibyte, memoryTotal*mebibyte
		gpu := GPUResource{Index: int(gpuIndex), UtilPct: &utilization, MemUsed: &usedBytes, MemTotal: &totalBytes}
		if temperature >= 0 {
			gpu.TempC = &temperature
		}
		gpus = append(gpus, gpu)
	}
	if len(gpus) == 0 {
		return nil, fmt.Errorf("gpu query: no lines")
	}
	return gpus, nil
}
