package agent

// 자원 지표 스트림 항목의 body(JSON) — requirement-spec.md(2026-11-24 resource-metrics) §10.2 의 칸 이름 그대로.
// null 이 될 수 있는 값은 포인터다: 첫 수집 · 누적 값이 줄어듦 · 간격 2분 초과 · 읽지 못한 갈래.

// 이번 수집에서 읽지 못한 갈래 — body.missing 의 낱말.
const (
	missingHost                = "host"
	missingContainers          = "containers"
	missingContainersTruncated = "containers_truncated"
	missingGPUs                = "gpus"
)

// ResourceBody — 한 번의 수집 = 스트림 항목 하나.
type ResourceBody struct {
	IntervalMs *int64              `json:"interval_ms"` // 앞 수집과의 간격(단조 시계). 첫 수집은 null
	Host       HostResource        `json:"host"`        // 읽지 못하면 칸은 그대로 두고 값만 null + missing 에 host
	Containers []ContainerResource `json:"containers"`  // 이름순. 빈 경우에도 [] (null 아님)
	GPUs       []GPUResource       `json:"gpus"`        // 빈 경우에도 []
	Missing    []string            `json:"missing"`     // 빈 경우에도 []
}

// HostResource — 서버 전체. cpu_pct 는 모든 코어 평균 0~100(소수 한 자리). 바이트 · 초당 바이트는 정수(초당 값은 반올림).
type HostResource struct {
	CPUPct     *float64 `json:"cpu_pct"`
	Load1      *float64 `json:"load1"`
	MemTotal   *uint64  `json:"mem_total"`
	MemUsed    *uint64  `json:"mem_used"` // 전체 − MemAvailable
	DiskTotal  *uint64  `json:"disk_total"`
	DiskUsed   *uint64  `json:"disk_used"`
	NetRxBps   *uint64  `json:"net_rx_bps"`
	NetTxBps   *uint64  `json:"net_tx_bps"`
	IOReadBps  *uint64  `json:"io_read_bps"`
	IOWriteBps *uint64  `json:"io_write_bps"`
}

// ContainerResource — 이 프로젝트의 컨테이너 하나. cpu_pct 는 코어 하나 = 100.
type ContainerResource struct {
	Name     string   `json:"name"`     // 앞의 "/" 를 뗀 이름
	ID       string   `json:"id"`       // 앞 12자
	Project  string   `json:"project"`  // compose 프로젝트 이름(METRICS_EXTRA_CONTAINERS 로 더한 것은 없으면 "")
	CPUPct   *float64 `json:"cpu_pct"`  // 같은 ID 의 앞 표본과의 차이
	MemUsed  *uint64  `json:"mem_used"` // 사용량 − 비활성 파일 캐시
	Restarts *int     `json:"restarts"` // 누적
	State    string   `json:"state"`    // running · exited …
}

// GPUResource — GPU 하나(설정된 컨테이너 안의 nvidia-smi 조회).
type GPUResource struct {
	Index    int      `json:"index"`
	UtilPct  *float64 `json:"util_pct"`
	MemUsed  *uint64  `json:"mem_used"`  // 바이트(MiB × 1048576)
	MemTotal *uint64  `json:"mem_total"` // 바이트
	TempC    *int64   `json:"temp_c"`
}
