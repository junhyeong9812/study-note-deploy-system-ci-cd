package agent

// 통신 수치 수집(⑬ T5) — 이 서버의 nginx 컨테이너의 도커 로그(접속 기록)를 30초마다 읽은 자리부터 이어 읽어,
// 범위 하나를 항목 하나로 Redis 스트림 traffic 에 보낸다.
// 계약: docs/cross-cutting/plans/2026-11-25/traffic-map/requirement-spec.md "T5 착수" · "T5 설계의 선검증 반영"(뒤가 앞선다) ·
// communication-protocol.md §3.1 · §3.2 · §3.6 · §4.1~§4.4. 세부: traffic_*_test.go 머리의 주석.
//
// 이 파일은 뼈대다 — 공개 표면(설정 · 수집기 · 헬스 · 읽은 자리 파일의 쓰기)만 있고 본문은 구현하지 않았다.

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/junhyeong9812/study-note-deploy-system-ci-cd/internal/shared"
)

// TrafficSettings — 환경 변수에서 읽는 통신 수집 설정. DisabledReason 이 비어 있지 않으면 수집하지 않는다.
type TrafficSettings struct {
	RedisAddress   string   // LOG_REDIS_URL 의 host:port
	Stream         string   // TRAFFIC_STREAM, 기본 "traffic"
	Server         string   // SERVER_NAME(그대로 — 기본값을 쓰지 않는다)
	Containers     []string // TRAFFIC_CONTAINERS — 쉼표로 나눈 이름(앞뒤 공백 제거 · 빈 칸 버림), 받은 차례 그대로
	CursorPath     string   // TRAFFIC_CURSOR_PATH, 기본 "/data/traffic-cursor.json"
	DisabledReason string   // "" 이면 켜짐. 아니면 traffic_settings_test.go 의 낱말 하나
}

// LoadTrafficSettings — 환경 변수를 읽고 검사한다.
func LoadTrafficSettings() TrafficSettings {
	return TrafficSettings{DisabledReason: "not_implemented"}
}

// TrafficContainerHealth — 헬스의 컨테이너 하나.
type TrafficContainerHealth struct {
	Name     string  `json:"name"`
	State    *string `json:"state"` // ok · replaced · rotated · unreadable · cursor_lost · clock_behind — 아직 읽지 않았으면 null
	Lines    int64   `json:"lines"`
	Unparsed int64   `json:"unparsed"`
	Ignored  int64   `json:"ignored"`
}

// TrafficHealth — 에이전트의 /health 의 traffic 객체.
type TrafficHealth struct {
	Enabled          bool                     `json:"enabled"`
	DisabledReason   *string                  `json:"disabled_reason"`
	Generation       *string                  `json:"generation"`
	LastSeq          *int64                   `json:"last_seq"`
	LastSentAt       *int64                   `json:"last_sent_at"`
	Pending          bool                     `json:"pending"`
	SendFailures     int64                    `json:"send_failures"`
	SkippedCycles    int64                    `json:"skipped_cycles"`
	RequestsFiltered int64                    `json:"requests_filtered"`
	TimeReversals    int64                    `json:"time_reversals"`
	ClockBehind      int64                    `json:"clock_behind"`
	OversizedEntries int64                    `json:"oversized_entries"`
	Panics           int64                    `json:"panics"`
	Containers       []TrafficContainerHealth `json:"containers"`
}

// TrafficCollector — 아래 칸은 테스트가 바꾸는 이음매다(newTrafficCollector 가 운영 값으로 채운다).
type TrafficCollector struct {
	settings TrafficSettings

	docker        *dockerAPI                              // 도커 엔진 API(유닉스 소켓)
	stream        shared.StreamWriter                     // shared.NewRedisStream(settings.RedisAddress)
	wallNow       func() time.Time                        // 벽시계 — until · at · 지금부터의 자리
	logf          func(message, level string)             // agent 로그 — 운영은 agent.logger.Log("traffic", message, level)
	newGeneration func() string                           // 소문자 16진 16자(운영은 crypto/rand)
	writeCursor   func(path string, content []byte) error // 운영은 writeCursorAtomically(path, content, syncDirectory)
	syncDirectory func(directory string) error            // 부모 폴더 fsync
	readDeadline  time.Duration                           // 컨테이너 하나의 읽기(10초)
	cycleDeadline time.Duration                           // 한 주기 전체(25초)
}

// StartTraffic — 설정을 읽어 수집을 시작한다(30초마다). 꺼져 있으면 시작하지 않고 로그 한 줄 — 그래도 nil 이 아닌 수집기를 돌려준다(헬스가 까닭을 낸다).
func (agent *Agent) StartTraffic(ctx context.Context) *TrafficCollector {
	return &TrafficCollector{settings: TrafficSettings{DisabledReason: "not_implemented"}}
}

// newTrafficCollector — 운영 이음매와 기본 기한으로 채운 수집기(시작하지 않는다).
func (agent *Agent) newTrafficCollector(settings TrafficSettings) *TrafficCollector {
	return &TrafficCollector{settings: settings}
}

// Run — ticks 마다 주기 하나(앞 주기가 끝나지 않았으면 건너뛰고 센다). ctx 가 끝나거나 ticks 가 닫히면 돌아온다.
func (collector *TrafficCollector) Run(ctx context.Context, ticks <-chan time.Time) {}

// runCycle — 주기 하나를 지금 이 고루틴에서 돈다(테스트가 부른다): 대기 중인 항목을 다시 보내거나, 범위를 정해 적고 보낸다(주기당 항목 3개까지).
func (collector *TrafficCollector) runCycle(ctx context.Context) {}

// Health — 헬스의 traffic 객체.
func (collector *TrafficCollector) Health() TrafficHealth { return TrafficHealth{} }

// AgentHealth — 에이전트의 GET /health 의 data: {"mode": mode, "traffic": …}. cmd/server 가 에이전트 모드에서 이것을 낸다.
func AgentHealth(mode string, traffic *TrafficCollector) any { return map[string]string{"mode": mode} }

// AgentHealthHandler — GET /health 처리기(봉투 shared.WriteOK).
func AgentHealthHandler(mode string, traffic *TrafficCollector) http.HandlerFunc {
	return func(writer http.ResponseWriter, _ *http.Request) { shared.WriteOK(writer, AgentHealth(mode, traffic)) }
}

// writeCursorAtomically — 같은 폴더의 임시 파일에 쓰고 fsync → 이름 바꾸기 → 부모 폴더 fsync(syncDirectory).
func writeCursorAtomically(path string, content []byte, syncDirectory func(directory string) error) error {
	return errors.New("not implemented")
}
