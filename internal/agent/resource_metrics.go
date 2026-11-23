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
	"context"
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
	return ResourceMetricsSettings{}
}

// ResourceCollectorStats — 수집기의 누적 횟수와 지금 상태.
type ResourceCollectorStats struct {
	Collections  int64 // 시작한 수집 — 시작할 때 센다
	Sent         int64 // 스트림에 넣은 항목
	SendFailures int64 // 스트림에 넣지 못한 항목
	Skipped      int64 // 앞 수집이 끝나지 않아 건너뛴 주기
	Panics       int64 // 수집 · 전송 중 되살린 패닉
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
}

// StartResourceMetrics — 설정을 읽어 수집을 시작한다(resourceMetricsInterval 마다 — 첫 수집은 한 주기 뒤).
// Redis 주소가 비면 시작하지 않고 로그 1줄을 남긴 뒤 nil. ctx 가 끝나면 멈춘다.
func (agent *Agent) StartResourceMetrics(ctx context.Context) *ResourceCollector {
	return nil
}

// newResourceCollector — 운영 이음매(/proc · "/" · syscall.Statfs · 도커 소켓 · Redis · 시계 · agent 로그)와 기본 기한으로 채운 수집기.
func (agent *Agent) newResourceCollector(settings ResourceMetricsSettings) *ResourceCollector {
	return &ResourceCollector{settings: settings}
}

// Run — ticks 가 올 때마다 수집 하나를 시작한다(앞 수집이 끝나지 않았으면 건너뛰고 센다). ctx 가 끝나거나 ticks 가 닫히면 돌아온다.
// 수집 자체는 따로 돌아 Run 을 붙잡지 않는다.
func (collector *ResourceCollector) Run(ctx context.Context, ticks <-chan time.Time) {
	<-ctx.Done()
}

func (collector *ResourceCollector) Stats() ResourceCollectorStats {
	return ResourceCollectorStats{}
}
