package agent

// 통신 수치 수집(⑬ T5) — 이 서버의 nginx 컨테이너의 도커 로그(접속 기록)를 30초마다 읽은 자리부터 이어 읽어,
// 범위 하나를 항목 하나로 Redis 스트림 traffic 에 보낸다.
// 계약: docs/cross-cutting/plans/2026-11-25/traffic-map/requirement-spec.md "T5 착수" · "T5 설계의 선검증 반영" · "T5 테스트가 정한 세부"
// (뒤가 앞선다) · communication-protocol.md §3.1 · §3.2 · §3.6 · §4.1~§4.4. 세부: traffic_*_test.go 머리의 주석.
//
// 불변식
//   - 배포를 막지 않는다: busy 잠금을 잡지 않는다. 주기의 어떤 실패 · 패닉도 agent 를 죽이지 않고 다음 주기가 돈다(앞 주기가 끝나지 않았으면 건너뛰고 센다).
//   - 읽기만: 도커에는 GET /containers/{이름}/json 과 GET /containers/{Id}/logs(표준 출력만)뿐. 설정으로 준 이름의 컨테이너만 읽는다.
//   - 먼저 적고 보낸다: 항목을 다 만든 뒤 읽은 자리의 파일에 written 으로 적고 → XADD → 성공하면 sent 와 옮긴 자리를 적는다.
//     written 이 남아 있으면 도커를 부르지 않고 파일에 적은 글자 그대로 다시 보낸다 — 같은 순번은 언제나 바이트까지 같다.
//   - 응답은 프레임 단위로 흘려 읽는다(상한에서 멈추고 연결을 끊는다). 몸을 통째로 메모리에 올리지 않는다.
//   - 접속 기록의 내용(경로 · 요청 ID · 주소)은 헬스 · 로그에 싣지 않는다. 패닉은 형만 로그에 싣는다.

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/junhyeong9812/study-note-deploy-system-ci-cd/internal/shared"
)

const (
	trafficInterval          = 30 * time.Second // 주기(StartTraffic 의 ticker — 자원 지표와 같은 틱)
	trafficReadDeadline      = 10 * time.Second // 컨테이너 하나의 읽기(Id 확인 포함)
	trafficCycleDeadline     = 25 * time.Second // 한 주기의 읽기 전체
	trafficSendDeadline      = 5 * time.Second  // XADD 하나
	trafficUntilLag          = 2 * time.Second  // until = 지금 − 2초(도커가 아직 쓰는 중인 줄을 피한다)
	trafficEntriesPerCycle   = 3                // 주기당 항목
	trafficFrameLimit        = 5000             // 한 번 읽기의 프레임 상한
	trafficReadByteLimit     = 8 << 20          // 한 번 읽기의 몸 상한(머리 포함)
	trafficGroupFrameLimit   = 256              // 같은 시각의 묶음이 이보다 크면 끊는다(skip 으로 건너뛴 프레임은 세지 않는다)
	trafficFramePayloadLimit = 1 << 20          // 프레임 하나가 이보다 크면 깨진 응답으로 본다(도커는 16KB 조각으로 준다)
	trafficLineByteLimit     = 64 << 10         // 이어 붙인 줄이 이보다 길면 unparsed
	trafficSegmentLimit      = 60               // 항목 하나의 묶음 상한
	trafficRequestLimit      = 100              // 항목 하나의 요청별 줄 상한
	trafficBodyByteLimit     = 32768            // body 상한(UTF-8 바이트)
	trafficPathLimit         = 1024             // 경로 상한(글자)
	trafficTextLimit         = 128              // upstream_status 상한(글자 — 관측 서버)
	trafficStreamMaxLength   = 5000             // XADD traffic MAXLEN ~ 5000
	trafficContainerLimit    = 16               // TRAFFIC_CONTAINERS 의 상한
	trafficCursorVersion     = 1                // 읽은 자리 파일의 판
	trafficStreamKind        = "traffic"
	trafficStreamVersion     = "1"
	defaultTrafficStream     = "traffic"
	defaultTrafficCursorPath = "/data/traffic-cursor.json"
	trafficCursorWritten     = "written"
	trafficCursorSent        = "sent"
)

// 꺼진 까닭(헬스의 disabled_reason).
const (
	trafficNoContainers         = "no_containers"
	trafficTooManyContainers    = "too_many_containers"
	trafficInvalidContainerName = "invalid_container_name"
	trafficDuplicateContainer   = "duplicate_container"
	trafficNoServerName         = "no_server_name"
	trafficInvalidServerName    = "invalid_server_name"
	trafficNoRedis              = "no_redis"
)

// 컨테이너의 상태(헬스 · missing 의 이유).
const (
	trafficStateOK          = "ok"
	trafficStateReplaced    = "replaced"
	trafficStateRotated     = "rotated"
	trafficStateUnreadable  = "unreadable"
	trafficStateCursorLost  = "cursor_lost"
	trafficStateClockBehind = "clock_behind"
)

var (
	trafficContainerNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)
	trafficServerNamePattern    = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)
	trafficGenerationPattern    = regexp.MustCompile(`^[0-9a-f]{16}$`)
	trafficContainerIDPattern   = regexp.MustCompile(`^[0-9a-f]{64}$`)
	trafficCursorFieldNames     = []string{"kind", "v", "server", "generation", "seq", "at", "body"}
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
	settings := TrafficSettings{RedisAddress: shared.RedisAddress(os.Getenv("LOG_REDIS_URL")), Stream: os.Getenv("TRAFFIC_STREAM"),
		Server: os.Getenv("SERVER_NAME"), CursorPath: os.Getenv("TRAFFIC_CURSOR_PATH")}
	if settings.Stream == "" {
		settings.Stream = defaultTrafficStream
	}
	if settings.CursorPath == "" {
		settings.CursorPath = defaultTrafficCursorPath
	}
	for _, name := range strings.Split(os.Getenv("TRAFFIC_CONTAINERS"), ",") {
		if name = strings.TrimSpace(name); name != "" {
			settings.Containers = append(settings.Containers, name)
		}
	}
	settings.DisabledReason = trafficDisabledReason(settings)
	return settings
}

func trafficDisabledReason(settings TrafficSettings) string {
	switch {
	case len(settings.Containers) == 0:
		return trafficNoContainers
	case len(settings.Containers) > trafficContainerLimit:
		return trafficTooManyContainers
	}
	seen := map[string]bool{}
	for _, name := range settings.Containers {
		if !trafficContainerNamePattern.MatchString(name) {
			return trafficInvalidContainerName
		}
		if seen[name] {
			return trafficDuplicateContainer
		}
		seen[name] = true
	}
	switch {
	case settings.Server == "":
		return trafficNoServerName
	case !trafficServerNamePattern.MatchString(settings.Server):
		return trafficInvalidServerName
	case settings.RedisAddress == "":
		return trafficNoRedis
	}
	return ""
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

	// 아래는 한 번에 하나만 도는 주기(inFlight)만 읽고 쓴다 — 겹침 방지가 차례를 보장한다.
	cursor         *trafficCursorState            // 읽은 자리(파일의 메모리 사본). nil 이면 다음 주기가 파일에서 읽는다
	lostContainers map[string]bool                // 다음 항목에 cursor_lost 로 실을 컨테이너(지금부터 — 로그를 읽지 않는다)
	incomplete     map[string]logPosition         // 줄바꿈 없이 끝난 마지막 묶음을 한 번 기다린 자리 — 단순화: 메모리에만 둔다, 다시 시작하면 한 주기를 더 기다린다(줄을 잃지 않는다)
	pendingDeltas  map[string]trafficSourceCounts // written 항목의 수 — 보내기에 성공하면 헬스에 더한다(단순화: 다시 시작한 뒤 다시 보낸 항목의 수는 헬스에 더하지 않는다 — 헬스는 이 실행의 누적)
	pendingFilter  int64                          // written 항목의 requests_filtered
	cursorFailing  bool                           // 읽은 자리 파일을 쓰지 못하는 중(첫 번에만 로그)

	mutex         sync.Mutex
	health        TrafficHealth
	inFlight      bool
	skipping      bool  // 지금 이어지는 건너뜀(첫 번에만 로그)
	failureStreak int64 // 지금 이어지는 보내기 실패
}

// logPosition — 읽은 자리 (도커 시각 epoch 나노초, 그 시각의 프레임 가운데 이미 읽은 수).
type logPosition struct {
	nanoseconds int64
	skip        int64
}

type trafficCursorContainerState struct {
	name, id string
	position logPosition
	anchored bool // 그 시각의 프레임을 실제로 읽었다(돌림의 판정은 이때만)
}

type trafficCursorSourceState struct {
	container, id string
	from, to      logPosition
	anchored      bool
}

type trafficCursorLastState struct {
	seq, at int64
	state   string
	sources []trafficCursorSourceState // sent 로 바꿀 때 자리를 옮길 값
	fields  []shared.StreamField
}

type trafficCursorState struct {
	generation string
	containers []trafficCursorContainerState // 이름 순
	last       *trafficCursorLastState
}

// 읽은 자리 파일의 꼴(traffic_cursor_test.go 머리의 계약).
type trafficFilePosition struct {
	DockerTimestamp string `json:"docker_ts"`
	Skip            int64  `json:"skip"`
}

type trafficFileContainer struct {
	Name     string              `json:"name"`
	ID       string              `json:"id"`
	Position trafficFilePosition `json:"position"`
	Anchored bool                `json:"anchored"`
}

type trafficFileSource struct {
	Container string              `json:"container"`
	ID        string              `json:"id"`
	From      trafficFilePosition `json:"from"`
	To        trafficFilePosition `json:"to"`
	Anchored  bool                `json:"anchored"`
}

type trafficFileField struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type trafficFileLast struct {
	Seq     int64               `json:"seq"`
	At      int64               `json:"at"`
	State   string              `json:"state"`
	Sources []trafficFileSource `json:"sources"`
	Fields  []trafficFileField  `json:"fields"`
}

type trafficFileDocument struct {
	Version    int                    `json:"version"`
	Generation string                 `json:"generation"`
	Containers []trafficFileContainer `json:"containers"`
	Last       *trafficFileLast       `json:"last"`
}

// StartTraffic — 설정을 읽어 수집을 시작한다(30초마다 — 첫 주기는 한 주기 뒤). 꺼져 있으면 시작하지 않고 로그 한 줄 —
// 그래도 nil 이 아닌 수집기를 돌려준다(헬스가 까닭을 낸다).
func (agent *Agent) StartTraffic(ctx context.Context) *TrafficCollector {
	settings := LoadTrafficSettings()
	collector := agent.newTrafficCollector(settings)
	if settings.DisabledReason != "" {
		agent.logger.Log("traffic", "traffic collection disabled: "+settings.DisabledReason, "info")
		return collector
	}
	ticker := time.NewTicker(trafficInterval)
	go func() {
		defer ticker.Stop()
		collector.Run(ctx, ticker.C)
	}()
	agent.logger.Log("traffic", fmt.Sprintf("traffic collection every %s from %d containers to stream %s",
		trafficInterval, len(settings.Containers), settings.Stream), "info")
	return collector
}

// newTrafficCollector — 운영 이음매와 기본 기한으로 채운 수집기(시작하지 않는다).
func (agent *Agent) newTrafficCollector(settings TrafficSettings) *TrafficCollector {
	collector := &TrafficCollector{settings: settings,
		docker: newDockerAPI(dockerSocketPath), stream: shared.NewRedisStream(settings.RedisAddress), wallNow: time.Now,
		logf:          func(message, level string) { agent.logger.Log("traffic", message, level) },
		newGeneration: randomTrafficGeneration, syncDirectory: syncDirectoryOnDisk,
		readDeadline: trafficReadDeadline, cycleDeadline: trafficCycleDeadline,
		incomplete: map[string]logPosition{}}
	collector.writeCursor = func(path string, content []byte) error {
		return writeCursorAtomically(path, content, collector.syncDirectory)
	}
	collector.health = TrafficHealth{Enabled: settings.DisabledReason == "", Containers: []TrafficContainerHealth{}}
	if settings.DisabledReason != "" {
		reason := settings.DisabledReason
		collector.health.DisabledReason = &reason
	} else {
		for _, name := range sortedNames(settings.Containers) {
			collector.health.Containers = append(collector.health.Containers, TrafficContainerHealth{Name: name})
		}
	}
	return collector
}

func sortedNames(names []string) []string {
	sorted := append([]string(nil), names...)
	sort.Strings(sorted)
	return sorted
}

func randomTrafficGeneration() string {
	random := make([]byte, 8)
	if _, err := rand.Read(random); err != nil {
		// crypto/rand 는 리눅스에서 실패하지 않는다 — 실패하면 시각으로라도 겹치지 않게 한다
		binary.BigEndian.PutUint64(random, uint64(time.Now().UnixNano()))
	}
	return hex.EncodeToString(random)
}

// Run — ticks 마다 주기 하나(앞 주기가 끝나지 않았으면 건너뛰고 센다). ctx 가 끝나거나 ticks 가 닫히면 돌아온다.
// 주기 자체는 따로 돌아 Run 을 붙잡지 않는다.
func (collector *TrafficCollector) Run(ctx context.Context, ticks <-chan time.Time) {
	for {
		select {
		case <-ctx.Done():
			return
		case _, open := <-ticks:
			if !open {
				return
			}
			collector.startCycle(ctx)
		}
	}
}

func (collector *TrafficCollector) startCycle(ctx context.Context) {
	collector.mutex.Lock()
	if collector.inFlight {
		collector.health.SkippedCycles++
		firstSkip := !collector.skipping
		collector.skipping = true
		skipped := collector.health.SkippedCycles
		collector.mutex.Unlock()
		if firstSkip {
			collector.logf(fmt.Sprintf("traffic cycle skipped: previous cycle has not finished (skipped %d)", skipped), "warning")
		}
		return
	}
	collector.inFlight, collector.skipping = true, false
	collector.mutex.Unlock()
	go func() {
		defer func() {
			recovered := recover()
			if recovered != nil {
				collector.cursor = nil // 메모리의 자리를 믿지 않는다 — 다음 주기가 파일에서 다시 읽는다
				collector.logf(fmt.Sprintf("traffic cycle panic: %T", recovered), "error")
			}
			collector.mutex.Lock()
			if recovered != nil {
				collector.health.Panics++
			}
			collector.inFlight = false
			collector.mutex.Unlock()
		}()
		collector.runCycle(ctx)
	}()
}

// runCycle — 주기 하나를 지금 이 고루틴에서 돈다(테스트가 부른다): 대기 중인 항목을 다시 보내거나, 범위를 정해 적고 보낸다(주기당 항목 3개까지).
func (collector *TrafficCollector) runCycle(ctx context.Context) {
	if collector.settings.DisabledReason != "" || !collector.loadCursor() {
		return
	}
	if last := collector.cursor.last; last != nil && last.state == trafficCursorWritten {
		collector.sendLast(ctx) // 도커를 부르지 않는다 — 그 주기에 새 항목은 없다
		return
	}
	cycleContext, cancel := context.WithTimeout(ctx, collector.cycleDeadline)
	defer cancel()
	for range trafficEntriesPerCycle {
		more, sent := collector.buildAndSend(ctx, cycleContext)
		if !sent || !more || cycleContext.Err() != nil {
			return
		}
	}
}

// Health — 헬스의 traffic 객체.
func (collector *TrafficCollector) Health() TrafficHealth {
	collector.mutex.Lock()
	defer collector.mutex.Unlock()
	health := collector.health
	health.Containers = append([]TrafficContainerHealth{}, collector.health.Containers...)
	return health
}

type agentHealthBody struct {
	Mode    string        `json:"mode"`
	Traffic TrafficHealth `json:"traffic"`
}

// AgentHealth — 에이전트의 GET /health 의 data: {"mode": mode, "traffic": …}. cmd/server 가 에이전트 모드에서 이것을 낸다.
func AgentHealth(mode string, traffic *TrafficCollector) any {
	if traffic == nil {
		return map[string]string{"mode": mode}
	}
	return agentHealthBody{Mode: mode, Traffic: traffic.Health()}
}

// AgentHealthHandler — GET /health 처리기(봉투 shared.WriteOK).
func AgentHealthHandler(mode string, traffic *TrafficCollector) http.HandlerFunc {
	return func(writer http.ResponseWriter, _ *http.Request) { shared.WriteOK(writer, AgentHealth(mode, traffic)) }
}

// ===== 읽은 자리의 파일 =====

// writeCursorAtomically — 같은 폴더의 임시 파일에 쓰고 fsync → 이름 바꾸기 → 부모 폴더 fsync(syncDirectory).
func writeCursorAtomically(path string, content []byte, syncDirectory func(directory string) error) error {
	directory := filepath.Dir(path)
	temporary, err := os.CreateTemp(directory, "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	_, err = temporary.Write(content)
	if err == nil {
		err = temporary.Sync()
	}
	if closeErr := temporary.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(temporary.Name(), path)
	}
	if err != nil {
		os.Remove(temporary.Name())
		return err
	}
	return syncDirectory(directory)
}

func syncDirectoryOnDisk(directory string) error {
	opened, err := os.Open(directory)
	if err != nil {
		return err
	}
	defer opened.Close()
	return opened.Sync()
}

func formatDockerTimestamp(nanoseconds int64) string {
	return time.Unix(0, nanoseconds).UTC().Format("2006-01-02T15:04:05.000000000Z")
}

func parseDockerTimestamp(text string) (int64, bool) {
	parsed, err := time.Parse(time.RFC3339Nano, text)
	if err != nil || parsed.Before(time.Unix(0, 0)) {
		return 0, false
	}
	return parsed.UnixNano(), true
}

// dockerSeconds — 도커 API 의 since · until 꼴 `초.9자리`.
func dockerSeconds(nanoseconds int64) string {
	nanoseconds = max(nanoseconds, 0)
	return fmt.Sprintf("%d.%09d", nanoseconds/int64(time.Second), nanoseconds%int64(time.Second))
}

func filePosition(position logPosition) trafficFilePosition {
	return trafficFilePosition{DockerTimestamp: formatDockerTimestamp(position.nanoseconds), Skip: position.skip}
}

func positionFromFile(position trafficFilePosition) (logPosition, bool) {
	nanoseconds, ok := parseDockerTimestamp(position.DockerTimestamp)
	return logPosition{nanoseconds: nanoseconds, skip: position.Skip}, ok && position.Skip >= 0
}

// loadCursor — 메모리에 자리가 없으면 파일에서 읽는다. 없거나 · 읽을 수 없거나 · 꼴이 틀리면 새 세대로 지금부터(틀린 파일은 .corrupt-<epoch ms> 로).
func (collector *TrafficCollector) loadCursor() bool {
	if collector.cursor != nil {
		return true
	}
	content, err := os.ReadFile(collector.settings.CursorPath)
	reason := "missing"
	if err == nil {
		if cursor, ok := parseTrafficCursor(content); ok {
			collector.cursor = cursor
			collector.reconcileContainers()
			collector.publishCursorHealth()
			return true
		}
		reason = "corrupt"
	} else if !errors.Is(err, os.ErrNotExist) {
		reason = "corrupt"
	}
	now := collector.wallNow()
	if reason == "corrupt" {
		aside := fmt.Sprintf("%s.corrupt-%d", collector.settings.CursorPath, now.UnixMilli())
		if err := os.Rename(collector.settings.CursorPath, aside); err != nil && !errors.Is(err, os.ErrNotExist) {
			collector.logf("traffic cursor could not be moved aside: "+err.Error(), "warning")
		}
	}
	collector.cursor = &trafficCursorState{generation: collector.newGeneration()}
	collector.reconcileContainers()
	collector.logf(fmt.Sprintf("traffic cursor lost (%s) — new generation %s", reason, collector.cursor.generation), "warning")
	collector.publishCursorHealth()
	return true
}

// reconcileContainers — 파일의 컨테이너를 설정에 맞춘다(이름 순). 설정에 새로 든 이름은 지금부터 · cursor_lost.
func (collector *TrafficCollector) reconcileContainers() {
	known := map[string]trafficCursorContainerState{}
	for _, container := range collector.cursor.containers {
		known[container.name] = container
	}
	nowPosition := logPosition{nanoseconds: collector.wallNow().UnixNano()}
	containers := []trafficCursorContainerState{}
	for _, name := range sortedNames(collector.settings.Containers) {
		container, present := known[name]
		if !present {
			container = trafficCursorContainerState{name: name, position: nowPosition}
			if collector.lostContainers == nil {
				collector.lostContainers = map[string]bool{}
			}
			collector.lostContainers[name] = true
		}
		containers = append(containers, container)
	}
	collector.cursor.containers = containers
}

func parseTrafficCursor(content []byte) (*trafficCursorState, bool) {
	var document trafficFileDocument
	if json.Unmarshal(content, &document) != nil || document.Version != trafficCursorVersion || !trafficGenerationPattern.MatchString(document.Generation) {
		return nil, false
	}
	cursor := &trafficCursorState{generation: document.Generation}
	seen := map[string]bool{}
	for _, container := range document.Containers {
		position, ok := positionFromFile(container.Position)
		if !ok || seen[container.Name] || !trafficContainerNamePattern.MatchString(container.Name) ||
			(container.ID != "" && !trafficContainerIDPattern.MatchString(container.ID)) {
			return nil, false
		}
		seen[container.Name] = true
		cursor.containers = append(cursor.containers, trafficCursorContainerState{name: container.Name, id: container.ID, position: position, anchored: container.Anchored})
	}
	if document.Last != nil {
		last := document.Last
		if last.Seq < 1 || last.At < 0 || (last.State != trafficCursorWritten && last.State != trafficCursorSent) || len(last.Fields) != len(trafficCursorFieldNames) {
			return nil, false
		}
		state := &trafficCursorLastState{seq: last.Seq, at: last.At, state: last.State}
		for index, field := range last.Fields {
			if field.Name != trafficCursorFieldNames[index] {
				return nil, false
			}
			state.fields = append(state.fields, shared.StreamField{Name: field.Name, Value: field.Value})
		}
		for _, source := range last.Sources {
			from, fromOK := positionFromFile(source.From)
			to, toOK := positionFromFile(source.To)
			if !fromOK || !toOK || !trafficContainerNamePattern.MatchString(source.Container) {
				return nil, false
			}
			state.sources = append(state.sources, trafficCursorSourceState{container: source.Container, id: source.ID, from: from, to: to, anchored: source.Anchored})
		}
		cursor.last = state
	}
	return cursor, true
}

func (collector *TrafficCollector) encodeCursor() ([]byte, error) {
	document := trafficFileDocument{Version: trafficCursorVersion, Generation: collector.cursor.generation, Containers: []trafficFileContainer{}}
	for _, container := range collector.cursor.containers {
		document.Containers = append(document.Containers, trafficFileContainer{Name: container.name, ID: container.id,
			Position: filePosition(container.position), Anchored: container.anchored})
	}
	if last := collector.cursor.last; last != nil {
		fileLast := &trafficFileLast{Seq: last.seq, At: last.at, State: last.state, Sources: []trafficFileSource{}, Fields: []trafficFileField{}}
		for _, source := range last.sources {
			fileLast.Sources = append(fileLast.Sources, trafficFileSource{Container: source.container, ID: source.id,
				From: filePosition(source.from), To: filePosition(source.to), Anchored: source.anchored})
		}
		for _, field := range last.fields {
			fileLast.Fields = append(fileLast.Fields, trafficFileField{Name: field.Name, Value: field.Value})
		}
		document.Last = fileLast
	}
	return json.Marshal(document)
}

// saveCursor — 메모리의 자리를 파일에 적는다. 쓰지 못하면 첫 번에만 로그(경로는 설정 값 — 접속 기록의 내용이 아니다).
func (collector *TrafficCollector) saveCursor() error {
	content, err := collector.encodeCursor()
	if err == nil {
		err = collector.writeCursor(collector.settings.CursorPath, content)
	}
	if err != nil {
		if !collector.cursorFailing {
			collector.logf("traffic cursor write failed: "+err.Error(), "warning")
		}
		collector.cursorFailing = true
		return err
	}
	collector.cursorFailing = false
	return nil
}

// publishCursorHealth — 헬스의 세대 · 대기 여부를 메모리의 자리에 맞춘다.
func (collector *TrafficCollector) publishCursorHealth() {
	generation := collector.cursor.generation
	pending := collector.cursor.last != nil && collector.cursor.last.state == trafficCursorWritten
	collector.mutex.Lock()
	collector.health.Generation, collector.health.Pending = &generation, pending
	collector.mutex.Unlock()
}

// ===== 보내기 =====

// sendLast — 파일에 적은 항목(written)을 그 글자 그대로 보낸다. 성공하면 sent 로 바꾸고 자리를 옮긴다.
func (collector *TrafficCollector) sendLast(parent context.Context) bool {
	last := collector.cursor.last
	ctx, cancel := context.WithTimeout(parent, trafficSendDeadline)
	defer cancel()
	err := collector.stream.Append(ctx, collector.settings.Stream, trafficStreamMaxLength, last.fields)
	if err != nil {
		collector.mutex.Lock()
		collector.health.SendFailures++
		collector.failureStreak++
		first := collector.failureStreak == 1
		collector.mutex.Unlock()
		if first {
			collector.logf("traffic stream send failed (further failures are counted, not logged)", "warning")
		}
		collector.publishCursorHealth()
		return false
	}
	sentAt, sequence := collector.wallNow().UnixMilli(), last.seq
	collector.mutex.Lock()
	recovered := collector.failureStreak
	collector.failureStreak, collector.health.SendFailures = 0, 0
	collector.health.LastSeq, collector.health.LastSentAt = &sequence, &sentAt
	collector.health.RequestsFiltered += collector.pendingFilter
	for index := range collector.health.Containers {
		counts := collector.pendingDeltas[collector.health.Containers[index].Name]
		collector.health.Containers[index].Lines += counts.lines
		collector.health.Containers[index].Unparsed += counts.unparsed
		collector.health.Containers[index].Ignored += counts.ignored
	}
	collector.mutex.Unlock()
	collector.pendingDeltas, collector.pendingFilter = nil, 0
	if recovered > 0 {
		collector.logf(fmt.Sprintf("traffic stream recovered after %d failed sends", recovered), "info")
	}
	for _, source := range last.sources {
		for index := range collector.cursor.containers {
			if container := &collector.cursor.containers[index]; container.name == source.container {
				container.id, container.position, container.anchored = source.id, source.to, source.anchored
			}
		}
	}
	last.state = trafficCursorSent
	// sent 를 적지 못해도(죽음) 다시 시작한 수집기가 같은 순번을 같은 바이트로 다시 보낸다 — 관측 서버가 뒤의 것을 버린다
	collector.saveCursor()
	collector.publishCursorHealth()
	return true
}

// buildAndSend — 범위를 한 번에 정해 항목 하나를 만들고 적고 보낸다. more = 같은 주기에 이어 읽을 것이 남았다.
func (collector *TrafficCollector) buildAndSend(parent, cycleContext context.Context) (more bool, sent bool) {
	until := collector.wallNow().Add(-trafficUntilLag).UnixNano()
	builder := newTrafficEntryBuilder()
	segmentCut := false
	for index := range collector.cursor.containers {
		container := collector.cursor.containers[index]
		if collector.lostContainers[container.name] {
			collector.addLostContainer(cycleContext, builder, container)
			continue
		}
		if segmentCut || cycleContext.Err() != nil { // 끊긴 뒤 · 주기 기한 — 빈 범위(자리 그대로 · missing 에 없다)
			builder.addEmpty(container)
			continue
		}
		if container.position.nanoseconds > until { // 시계가 뒤로 뛰었다 — 읽지 않는다
			builder.missing = append(builder.missing, container.name+":"+trafficStateClockBehind)
			collector.noteContainerState(container.name, trafficStateClockBehind, func(health *TrafficHealth) { health.ClockBehind++ })
			continue
		}
		readContext, cancel := context.WithTimeout(cycleContext, collector.readDeadline)
		result, err := collector.readContainer(readContext, container, until)
		cancel()
		if err != nil {
			if cycleContext.Err() != nil {
				builder.addEmpty(container)
				continue
			}
			builder.missing = append(builder.missing, container.name+":"+trafficStateUnreadable)
			reversed := errors.Is(err, errTrafficTimeReversal)
			collector.noteContainerState(container.name, trafficStateUnreadable, func(health *TrafficHealth) {
				if reversed {
					health.TimeReversals++
				}
			})
			continue
		}
		state := trafficStateOK
		switch {
		case result.replaced:
			state = trafficStateReplaced
			builder.missing = append(builder.missing, container.name+":"+trafficStateReplaced)
			collector.logf(fmt.Sprintf("traffic container %s replaced — reading from the start", container.name), "warning")
		case result.rotated:
			state = trafficStateRotated
			builder.missing = append(builder.missing, container.name+":"+trafficStateRotated)
			collector.logf(fmt.Sprintf("traffic container %s rotated — lines may be lost", container.name), "warning")
		}
		collector.noteContainerState(container.name, state, nil)
		if builder.addRead(container, result) {
			segmentCut = true
		}
		more = more || result.more || segmentCut
	}
	at := collector.wallNow().UnixMilli()
	body, err := builder.encode()
	if err != nil { // 묶음과 sources 만으로 32KB 를 넘었다 — 생길 수 없어야 한다(방어선): 보내지 않고 자리를 옮기지 않는다
		collector.mutex.Lock()
		collector.health.OversizedEntries++
		collector.mutex.Unlock()
		return false, false
	}
	sequence := int64(1)
	if collector.cursor.last != nil {
		sequence = collector.cursor.last.seq + 1
	}
	fields := []shared.StreamField{{Name: "kind", Value: trafficStreamKind}, {Name: "v", Value: trafficStreamVersion},
		{Name: "server", Value: collector.settings.Server}, {Name: "generation", Value: collector.cursor.generation},
		{Name: "seq", Value: strconv.FormatInt(sequence, 10)}, {Name: "at", Value: strconv.FormatInt(at, 10)}, {Name: "body", Value: body}}
	previous := collector.cursor.last
	collector.cursor.last = &trafficCursorLastState{seq: sequence, at: at, state: trafficCursorWritten, sources: builder.cursorSources, fields: fields}
	if collector.saveCursor() != nil { // 적지 못한 항목은 보내지 않는다 — 다음 주기가 같은 자리부터 다시 정한다
		collector.cursor.last = previous
		return false, false
	}
	collector.lostContainers = nil
	collector.pendingDeltas, collector.pendingFilter = builder.counts, builder.filtered
	collector.publishCursorHealth()
	return more, collector.sendLast(parent)
}

// addLostContainer — 지금부터 읽는 컨테이너: 로그를 읽지 않고 Id 만 확인해 빈 범위로 싣는다(cursor_lost).
func (collector *TrafficCollector) addLostContainer(cycleContext context.Context, builder *trafficEntryBuilder, container trafficCursorContainerState) {
	builder.missing = append(builder.missing, container.name+":"+trafficStateCursorLost)
	readContext, cancel := context.WithTimeout(cycleContext, collector.readDeadline)
	id, err := collector.inspectContainerID(readContext, container.name)
	cancel()
	if err != nil {
		builder.missing = append(builder.missing, container.name+":"+trafficStateUnreadable)
		collector.noteContainerState(container.name, trafficStateUnreadable, nil)
		return
	}
	container.id = id
	builder.addEmpty(container)
	collector.noteContainerState(container.name, trafficStateCursorLost, nil)
}

func (collector *TrafficCollector) noteContainerState(name, state string, change func(health *TrafficHealth)) {
	collector.mutex.Lock()
	defer collector.mutex.Unlock()
	for index := range collector.health.Containers {
		if collector.health.Containers[index].Name == name {
			value := state
			collector.health.Containers[index].State = &value
		}
	}
	if change != nil {
		change(&collector.health)
	}
}

// ===== 읽기 =====

var errTrafficTimeReversal = errors.New("traffic: frame timestamp went backwards")

func (collector *TrafficCollector) inspectContainerID(ctx context.Context, name string) (string, error) {
	var inspected struct {
		ID string `json:"Id"`
	}
	if err := collector.docker.decode(ctx, http.MethodGet, "/containers/"+url.PathEscape(name)+"/json", nil, &inspected); err != nil {
		return "", err
	}
	if !trafficContainerIDPattern.MatchString(inspected.ID) {
		return "", errors.New("docker inspect: unexpected id")
	}
	return inspected.ID, nil
}

// trafficUnit — 읽은 줄 하나(또는 버린 조각의 끝). end = 그 줄의 마지막 프레임까지 읽은 자리.
type trafficUnit struct {
	kind   int // trafficUnitLine · trafficUnitUnparsed · trafficUnitIgnored · trafficUnitDiscarded
	record *trafficAccessRecord
	end    logPosition
}

const (
	trafficUnitLine = iota
	trafficUnitUnparsed
	trafficUnitIgnored
	trafficUnitDiscarded // 앞 읽기가 삼킨 줄의 나머지 조각 — 세지 않는다
)

// trafficRead — 컨테이너 하나의 한 번 읽기.
type trafficRead struct {
	id                string
	replaced, rotated bool
	from              logPosition // sources.from — 실제로 읽기 시작한 자리
	units             []trafficUnit
	to                logPosition
	anchored          bool
	more              bool // 상한에서 멈췄다 — 이어 읽을 것이 남았다
}

type trafficFrame struct {
	nanoseconds int64
	content     []byte
}

// trafficFrameReader — 다중화된 흐름의 프레임을 하나씩 읽는다(머리 [1, 0, 0, 0, 길이] · 내용 `<시각> <줄>`).
type trafficFrameReader struct {
	reader        *bufio.Reader
	framesRead    int
	bytesRead     int64
	lastTimestamp int64
}

var errTrafficReadLimit = errors.New("traffic: read limit reached")

// next — 프레임 하나. 끝이면 io.EOF. 상한에 닿으면 그 프레임을 읽어 돌려주되 errTrafficReadLimit 와 함께(범위에 넣지 않는다).
func (frames *trafficFrameReader) next() (trafficFrame, error) {
	header := make([]byte, 8)
	if _, err := io.ReadFull(frames.reader, header); err != nil {
		if errors.Is(err, io.EOF) {
			return trafficFrame{}, io.EOF
		}
		return trafficFrame{}, fmt.Errorf("traffic: frame header: %w", err)
	}
	if header[0] != 1 || header[1] != 0 || header[2] != 0 || header[3] != 0 {
		return trafficFrame{}, errors.New("traffic: broken frame header")
	}
	length := int64(binary.BigEndian.Uint32(header[4:]))
	if length > trafficFramePayloadLimit {
		return trafficFrame{}, errors.New("traffic: frame too large")
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(frames.reader, payload); err != nil {
		return trafficFrame{}, fmt.Errorf("traffic: frame payload: %w", err)
	}
	space := bytes.IndexByte(payload, ' ')
	if space < 0 {
		return trafficFrame{}, errors.New("traffic: frame without timestamp")
	}
	nanoseconds, ok := parseDockerTimestamp(string(payload[:space]))
	if !ok {
		return trafficFrame{}, errors.New("traffic: unreadable frame timestamp")
	}
	if nanoseconds < frames.lastTimestamp {
		return trafficFrame{}, errTrafficTimeReversal
	}
	frames.lastTimestamp = nanoseconds
	frame := trafficFrame{nanoseconds: nanoseconds, content: payload[space+1:]}
	limited := frames.framesRead >= trafficFrameLimit || frames.bytesRead+8+length > trafficReadByteLimit
	frames.framesRead++
	frames.bytesRead += 8 + length
	if limited {
		return frame, errTrafficReadLimit
	}
	return frame, nil
}

// trafficLineAssembler — 프레임을 줄로 잇고 읽은 자리를 센다.
type trafficLineAssembler struct {
	units         []trafficUnit
	groupTime     int64 // 지금 묶음(같은 시각)의 시각 — 아직 없으면 −1
	framesAtTime  int64 // 그 시각의 프레임 가운데 읽은 수(skip 으로 건너뛴 것 포함 — 자리의 skip)
	groupFrames   int64 // 그 시각의 묶음 크기(건너뛴 · 버린 조각 제외 — 256 상한)
	pending       []byte
	pendingLength int
	hasPending    bool
	discarding    bool // 앞 읽기가 삼킨 줄의 나머지 조각을 줄바꿈까지 버린다
}

// finishPending — 이어 붙이던 줄을 한 줄로 낸다(줄바꿈으로 끝났거나 시각이 다른 프레임이 왔다).
func (assembler *trafficLineAssembler) finishPending() {
	if !assembler.hasPending {
		return
	}
	end := logPosition{nanoseconds: assembler.groupTime, skip: assembler.framesAtTime}
	if assembler.pendingLength > trafficLineByteLimit {
		assembler.units = append(assembler.units, trafficUnit{kind: trafficUnitUnparsed, end: end})
	} else {
		kind, record := classifyTrafficLine(assembler.pending)
		assembler.units = append(assembler.units, trafficUnit{kind: kind, record: record, end: end})
	}
	assembler.pending, assembler.pendingLength, assembler.hasPending = nil, 0, false
}

// add — 프레임 하나를 더한다. false 면 같은 시각의 묶음이 256 프레임을 넘어 그 앞에서 끊었다(삼킨 줄은 unparsed).
func (assembler *trafficLineAssembler) add(frame trafficFrame) bool {
	if frame.nanoseconds != assembler.groupTime {
		assembler.closeGroup()
		assembler.groupTime, assembler.framesAtTime, assembler.groupFrames = frame.nanoseconds, 0, 0
	}
	endsLine := bytes.HasSuffix(frame.content, []byte("\n"))
	if assembler.discarding {
		assembler.framesAtTime++
		if endsLine {
			assembler.discarding = false
			assembler.units = append(assembler.units, trafficUnit{kind: trafficUnitDiscarded, end: logPosition{assembler.groupTime, assembler.framesAtTime}})
		}
		return true
	}
	if assembler.groupFrames >= trafficGroupFrameLimit {
		if assembler.hasPending { // 삼킨 조각은 unparsed 한 줄 — 나머지는 다음 읽기가 skip 으로 건너 줄바꿈까지 버린다
			assembler.units = append(assembler.units, trafficUnit{kind: trafficUnitUnparsed, end: logPosition{assembler.groupTime, assembler.framesAtTime}})
			assembler.pending, assembler.pendingLength, assembler.hasPending = nil, 0, false
		}
		return false
	}
	assembler.framesAtTime++
	assembler.groupFrames++
	content := bytes.TrimSuffix(frame.content, []byte("\n"))
	assembler.pendingLength += len(content)
	if assembler.pendingLength <= trafficLineByteLimit {
		assembler.pending = append(assembler.pending, content...)
	}
	assembler.hasPending = true
	if endsLine {
		assembler.finishPending()
	}
	return true
}

// dropTrailingGroup — 끝의 같은 시각 묶음 전체를 범위에서 뺀다(덜 온 줄 · 상한에서 가르지 않기).
// keepProgress 면 빼고 나서 범위에 남는 줄이 없을 때(묶음 하나가 읽기 상한보다 크다) 빼지 않고 false — 그대로 두면 다음 읽기가 같은 자리에서 멈춘다.
func (assembler *trafficLineAssembler) dropTrailingGroup(keepProgress bool) bool {
	kept := len(assembler.units)
	for kept > 0 && assembler.units[kept-1].end.nanoseconds == assembler.groupTime {
		kept--
	}
	if keepProgress && kept == 0 {
		return false
	}
	assembler.units = assembler.units[:kept]
	assembler.pending, assembler.pendingLength, assembler.hasPending, assembler.discarding = nil, 0, false, false
	return true
}

// closeGroup — 지금 묶음을 닫는다: 이어 붙이던 줄은 한 줄로, 버리던 조각은 거기까지.
func (assembler *trafficLineAssembler) closeGroup() {
	assembler.finishPending()
	if assembler.discarding {
		assembler.units = append(assembler.units, trafficUnit{kind: trafficUnitDiscarded, end: logPosition{assembler.groupTime, assembler.framesAtTime}})
		assembler.discarding = false
	}
}

// readContainer — 컨테이너 하나를 읽은 자리부터 until 까지 읽는다. 오류면 그 컨테이너는 unreadable(자리 그대로).
func (collector *TrafficCollector) readContainer(ctx context.Context, container trafficCursorContainerState, until int64) (trafficRead, error) {
	result := trafficRead{}
	id, err := collector.inspectContainerID(ctx, container.name)
	if err != nil {
		return result, err
	}
	result.id = id
	position, anchored := container.position, container.anchored
	if container.id != "" && container.id != id { // 다시 만들어졌다 — 새 컨테이너의 줄은 모두 아직 읽지 않은 줄이다
		result.replaced = true
		position, anchored = logPosition{}, false
	}
	query := url.Values{"stdout": {"1"}, "stderr": {"0"}, "timestamps": {"1"}, "since": {dockerSeconds(position.nanoseconds)}, "until": {dockerSeconds(until)}}
	response, err := collector.docker.request(ctx, http.MethodGet, "/containers/"+url.PathEscape(id)+"/logs?"+query.Encode(), nil)
	if err != nil {
		return result, err
	}
	defer response.Body.Close() // 상한에서 멈추면 여기서 연결을 끊는다 — 나머지는 읽지 않는다
	frames := &trafficFrameReader{reader: bufio.NewReaderSize(response.Body, 64<<10), lastTimestamp: position.nanoseconds}
	assembler := &trafficLineAssembler{groupTime: -1}
	result.from = position

	// 자리의 시각을 가진 앞 skip 프레임은 건너뛴다. 자리가 실제로 읽은 줄이면(anchored) 그 프레임이 모자라거나 더 뒤의 줄부터 오면 돌려졌다.
	var skipped []trafficFrame
	var frame trafficFrame
	var readErr error
	for {
		frame, readErr = frames.next()
		if readErr != nil || frame.nanoseconds != position.nanoseconds || int64(len(skipped)) >= position.skip {
			break
		}
		skipped = append(skipped, frame)
	}
	if readErr != nil && !errors.Is(readErr, io.EOF) && !errors.Is(readErr, errTrafficReadLimit) {
		return result, readErr
	}
	fewer := int64(len(skipped)) < position.skip
	sawFrame := len(skipped) > 0 || readErr == nil || errors.Is(readErr, errTrafficReadLimit)
	switch {
	case anchored && !sawFrame: // 빈 응답인데 자리가 실제로 읽은 줄이다 — 다 돌려졌다
		result.rotated, anchored = true, false
	case anchored && (fewer || (len(skipped) == 0 && frame.nanoseconds > position.nanoseconds)):
		result.rotated = true
		first := frame.nanoseconds
		if len(skipped) > 0 {
			first = skipped[0].nanoseconds
		}
		result.from = logPosition{nanoseconds: first}
		for _, again := range skipped { // 받은 프레임을 처음부터 다 센다
			assembler.add(again)
		}
		skipped = nil
	}
	if len(skipped) > 0 { // 건너뛴 프레임 — 자리의 시각의 프레임 수로만 센다
		assembler.groupTime, assembler.framesAtTime = position.nanoseconds, int64(len(skipped))
		last := skipped[len(skipped)-1].content
		assembler.discarding = !bytes.HasSuffix(last, []byte("\n")) // 앞 읽기가 줄의 가운데서 끊었다 — 나머지 조각은 버린다
	}

	limited := false
	for readErr == nil || errors.Is(readErr, errTrafficReadLimit) {
		if errors.Is(readErr, errTrafficReadLimit) { // 상한 — 이 프레임은 범위에 넣지 않는다
			limited = true
			if frame.nanoseconds != assembler.groupTime || !assembler.dropTrailingGroup(true) {
				// 시각이 다른 프레임이 왔다(묶음이 끝났다) · 또는 묶음 하나가 상한보다 커 가르지 않으면 나아가지 못한다
				assembler.closeGroup()
			}
			break
		}
		if !assembler.add(frame) {
			limited = true
			break
		}
		frame, readErr = frames.next()
	}
	if readErr != nil && !errors.Is(readErr, io.EOF) && !errors.Is(readErr, errTrafficReadLimit) {
		return result, readErr
	}
	if !limited && errors.Is(readErr, io.EOF) {
		if assembler.hasPending { // 마지막 줄이 줄바꿈 없이 끝났다 — 뒤가 아직 오지 않았을 수 있다
			waiting := logPosition{nanoseconds: assembler.groupTime, skip: assembler.framesAtTime}
			if seen, present := collector.incomplete[container.name]; present && seen == waiting {
				assembler.units = append(assembler.units, trafficUnit{kind: trafficUnitUnparsed, end: waiting})
				assembler.pending, assembler.pendingLength, assembler.hasPending = nil, 0, false
				delete(collector.incomplete, container.name)
			} else {
				collector.incomplete[container.name] = waiting
				assembler.dropTrailingGroup(false)
			}
		} else {
			delete(collector.incomplete, container.name)
		}
	}
	result.units, result.more = assembler.units, limited
	result.to, result.anchored = result.from, anchored
	if count := len(assembler.units); count > 0 {
		result.to, result.anchored = assembler.units[count-1].end, true
	}
	return result, nil
}

// ===== 줄 → 수 =====

// trafficAccessRecord — 규약 §3.1 의 줄 하나에서 읽은 값.
type trafficAccessRecord struct {
	endMilliseconds      int64
	requestID            string
	caller, service      string
	method, path         string
	pathTruncated        bool
	status               int64
	durationMilliseconds int64
	upstreamMilliseconds *int64
	upstreamStatus       *string
	slot                 *string
	bytesSent            int64
	class                string
}

var (
	trafficLinePrefix         = []byte(`{"v":`)
	trafficAccessFieldNames   = []string{"v", "ts", "request_id", "caller", "service", "method", "path", "path_truncated", "status", "request_time", "upstream_time", "upstream_status", "slot", "bytes_sent"}
	trafficThreeDecimals      = regexp.MustCompile(`^[0-9]+\.[0-9]{3}$`)
	trafficDigits             = regexp.MustCompile(`^[0-9]+$`)
	trafficRequestIDPattern   = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	trafficMethodPattern      = regexp.MustCompile(`^[A-Z_-]{0,32}$`)
	trafficUpstreamTime       = regexp.MustCompile(`^(?:[0-9]+\.[0-9]{3}|-)(?:(?:, | : )(?:[0-9]+\.[0-9]{3}|-))*$`)
	trafficUpstreamStatus     = regexp.MustCompile(`^(?:[0-9]{3}|-)(?:(?:, | : )(?:[0-9]{3}|-))*$`)
	trafficRecordedCallers    = map[string]bool{"front": true, "backend": true, "llm-wrapper": true, "alert": true, "observe": true, "admin": true, "ci-master": true, "ci-agent": true, "edge": true, "outside": true}
	trafficServiceNames       = map[string]bool{"front": true, "backend": true, "llm-wrapper": true, "alert": true, "observe": true, "admin": true, "ci-master": true, "ci-agent": true, "edge": true}
	trafficSlots              = map[string]bool{"blue": true, "green": true, "": true}
	trafficHealthCallers      = map[string]bool{"alert": true, "ci-agent": true}
	trafficHealthPaths        = map[string][]string{"front": {"/"}, "backend": {"/actuator/health", "/api/health/dependencies"}, "llm-wrapper": {"/health"}, "alert": {"/health"}, "observe": {"/health"}, "admin": {"/api/health"}, "ci-master": {"/health"}, "ci-agent": {"/health"}}
	trafficStreamRoutes       = map[string][]string{"ci-agent": {"POST /agent/ci", "POST /agent/cd"}}
	trafficDurationBoundaries = []int64{5, 10, 25, 50, 100, 250, 500, 1000, 2500, 5000, 10000, 15000, 20000, 30000, 45000, 60000, 90000, 120000, 180000}
)

// classifyTrafficLine — `{"v":` 로 시작하지 않으면 ignored. 시작하는데 규약(칸 · 값의 꼴 · 관측 서버가 항목을 버리게 하는 값)에 맞지 않으면 unparsed.
func classifyTrafficLine(content []byte) (int, *trafficAccessRecord) {
	if !bytes.HasPrefix(content, trafficLinePrefix) {
		return trafficUnitIgnored, nil
	}
	record, ok := parseTrafficAccessLine(content)
	if !ok {
		return trafficUnitUnparsed, nil
	}
	return trafficUnitLine, record
}

// threeDecimalMilliseconds — `초.셋` 의 글자에서 소수점을 뺀 정수(부동소수 곱셈을 쓰지 않는다).
func threeDecimalMilliseconds(text string) (int64, bool) {
	if !trafficThreeDecimals.MatchString(text) {
		return 0, false
	}
	value, err := strconv.ParseInt(strings.Replace(text, ".", "", 1), 10, 64)
	return value, err == nil
}

func parseTrafficAccessLine(content []byte) (*trafficAccessRecord, bool) {
	decoder := json.NewDecoder(bytes.NewReader(content))
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		return nil, false
	}
	raw := map[string]json.RawMessage{}
	for decoder.More() {
		token, err := decoder.Token()
		name, isName := token.(string)
		if err != nil || !isName {
			return nil, false
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, false
		}
		if _, duplicate := raw[name]; duplicate {
			return nil, false
		}
		raw[name] = value
	}
	if token, err := decoder.Token(); err != nil || token != json.Delim('}') {
		return nil, false
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) { // 뒤에 더 붙은 글자
		return nil, false
	}
	if len(raw) != len(trafficAccessFieldNames) {
		return nil, false
	}
	text := map[string]string{}
	for _, name := range trafficAccessFieldNames {
		value, present := raw[name]
		if !present {
			return nil, false
		}
		if len(value) > 0 && value[0] == '"' {
			var decoded string
			if json.Unmarshal(value, &decoded) != nil {
				return nil, false
			}
			text[name] = decoded
		}
	}
	isText := func(names ...string) bool {
		for _, name := range names {
			if _, ok := text[name]; !ok {
				return false
			}
		}
		return true
	}
	if !isText("request_id", "caller", "service", "method", "path", "upstream_time", "upstream_status", "slot") || string(raw["v"]) != "1" {
		return nil, false
	}
	record := &trafficAccessRecord{requestID: text["request_id"], caller: text["caller"], service: text["service"], method: text["method"], path: text["path"]}
	endMilliseconds, ok := threeDecimalMilliseconds(string(raw["ts"]))
	if !ok || endMilliseconds < 1_000_000_000_000 || endMilliseconds >= 4_000_000_000_000 {
		return nil, false
	}
	record.endMilliseconds = endMilliseconds
	if !trafficRequestIDPattern.MatchString(record.requestID) || !trafficRecordedCallers[record.caller] || !trafficServiceNames[record.service] ||
		!trafficMethodPattern.MatchString(record.method) || utf8.RuneCountInString(record.path) > trafficPathLimit || hasControlCharacter(record.path) {
		return nil, false
	}
	switch string(raw["path_truncated"]) {
	case "true":
		record.pathTruncated = true
		if len(record.path) < trafficPathLimit {
			return nil, false
		}
	case "false":
	default:
		return nil, false
	}
	if !trafficDigits.MatchString(string(raw["status"])) {
		return nil, false
	}
	record.status, _ = strconv.ParseInt(string(raw["status"]), 10, 64)
	if record.status < 100 || record.status > 599 {
		return nil, false
	}
	if record.durationMilliseconds, ok = threeDecimalMilliseconds(string(raw["request_time"])); !ok {
		return nil, false
	}
	upstreamTime, upstreamStatus, slot := text["upstream_time"], text["upstream_status"], text["slot"]
	if (upstreamTime != "" && !trafficUpstreamTime.MatchString(upstreamTime)) || (upstreamStatus != "" && !trafficUpstreamStatus.MatchString(upstreamStatus)) ||
		utf8.RuneCountInString(upstreamStatus) > trafficTextLimit || !trafficSlots[slot] {
		return nil, false
	}
	bytesSent, err := strconv.ParseInt(string(raw["bytes_sent"]), 10, 64)
	if !trafficDigits.MatchString(string(raw["bytes_sent"])) || err != nil {
		return nil, false
	}
	record.bytesSent = bytesSent
	record.upstreamMilliseconds = sumUpstreamMilliseconds(upstreamTime)
	if upstreamStatus != "" && upstreamStatus != "-" {
		record.upstreamStatus = &upstreamStatus
	}
	if slot != "" {
		record.slot = &slot
	}
	record.class = classifyTrafficRequest(record)
	return record, true
}

func hasControlCharacter(text string) bool {
	for _, character := range text {
		if character < ' ' || character == 0x7f {
			return true
		}
	}
	return false
}

// sumUpstreamMilliseconds — `,` · `:` 로 나눈 조각의 합(`-` · "" 조각은 건너뛴다). 더할 조각이 없으면 nil.
func sumUpstreamMilliseconds(upstreamTime string) *int64 {
	var total int64
	found := false
	for _, piece := range strings.FieldsFunc(upstreamTime, func(character rune) bool { return character == ',' || character == ':' }) {
		piece = strings.TrimSpace(piece)
		if piece == "" || piece == "-" {
			continue
		}
		if milliseconds, ok := threeDecimalMilliseconds(piece); ok {
			total += milliseconds
			found = true
		}
	}
	if !found {
		return nil
	}
	return &total
}

// classifyTrafficRequest — 규약 §3.6 의 갈래: health → stream → request(표에 없는 서비스는 request).
func classifyTrafficRequest(record *trafficAccessRecord) string {
	if trafficHealthCallers[record.caller] && (record.method == "GET" || record.method == "HEAD") {
		for _, path := range trafficHealthPaths[record.service] {
			if record.path == path {
				return "health"
			}
		}
	}
	for _, route := range trafficStreamRoutes[record.service] {
		if route == record.method+" "+record.path {
			return "stream"
		}
	}
	return "request"
}

var (
	trafficEncodedQuestion = regexp.MustCompile(`%3[Ff]`)
	trafficEncodedAddress  = []struct {
		pattern *regexp.Regexp
		plain   string
	}{{regexp.MustCompile(`%3[Aa]`), ":"}, {regexp.MustCompile(`%2[Ee]`), "."}, {regexp.MustCompile(`%5[Bb]`), "["}, {regexp.MustCompile(`%5[Dd]`), "]"}}
	trafficAddressShapes = []*regexp.Regexp{
		regexp.MustCompile(`(^|[^0-9])[0-9]{1,3}(\.[0-9]{1,3}){3}([^0-9]|$)`),
		regexp.MustCompile(`(^|[^0-9A-Fa-f])[0-9A-Fa-f]{1,4}(:[0-9A-Fa-f]{1,4}){7}([^0-9A-Fa-f]|$)`),
		regexp.MustCompile(`(^|[^0-9A-Za-z])[0-9A-Fa-f]{1,4}::|::[0-9A-Fa-f]{1,4}([^0-9A-Za-z]|$)`),
		regexp.MustCompile(`\[[^\]]*:[^\]]*]`),
	}
)

// trafficPathAllowed — 요청별 줄에 실어도 되는 경로(관측 서버가 그 줄만 버리는 판정과 같다): 날것 ? · %3F 가 없고, %3A %2E %5B %5D 를 푼 뒤 주소 꼴이 없다.
func trafficPathAllowed(path string) bool {
	if strings.Contains(path, "?") || trafficEncodedQuestion.MatchString(path) {
		return false
	}
	decoded := path
	for _, encoded := range trafficEncodedAddress {
		decoded = encoded.pattern.ReplaceAllString(decoded, encoded.plain)
	}
	for _, shape := range trafficAddressShapes {
		if shape.MatchString(decoded) {
			return false
		}
	}
	return true
}

// ===== 항목 =====

type trafficSourceCounts struct{ lines, unparsed, ignored int64 }

type trafficSegmentKey struct {
	bucketStart            int64
	service, caller, class string
}

type trafficEntryStatus struct {
	Success     int64 `json:"2xx"`
	Redirect    int64 `json:"3xx"`
	ClientError int64 `json:"4xx"`
	ServerError int64 `json:"5xx"`
	Other       int64 `json:"other"`
}

type trafficEntrySegment struct {
	BucketStartMs int64              `json:"bucket_start_ms"`
	Service       string             `json:"service"`
	Caller        string             `json:"caller"`
	Class         string             `json:"class"`
	Count         int64              `json:"count"`
	Status        trafficEntryStatus `json:"status"`
	Buckets       []int64            `json:"buckets"`
	MaxMs         *int64             `json:"max_ms"`
}

type trafficEntryPosition struct {
	DockerTimestamp string `json:"docker_ts"`
	Skip            int64  `json:"skip"`
}

type trafficEntrySource struct {
	Container   string               `json:"container"`
	ContainerID string               `json:"container_id"`
	From        trafficEntryPosition `json:"from"`
	To          trafficEntryPosition `json:"to"`
	Lines       int64                `json:"lines"`
	Unparsed    int64                `json:"unparsed"`
	Ignored     int64                `json:"ignored"`
}

type trafficEntryRequest struct {
	RequestID      string  `json:"request_id"`
	Service        string  `json:"service"`
	Caller         string  `json:"caller"`
	Method         string  `json:"method"`
	Path           string  `json:"path"`
	PathTruncated  bool    `json:"path_truncated"`
	Status         int64   `json:"status"`
	EndMs          int64   `json:"end_ms"`
	DurationMs     int64   `json:"duration_ms"`
	UpstreamMs     *int64  `json:"upstream_ms"`
	UpstreamStatus *string `json:"upstream_status"`
	Slot           *string `json:"slot"`
	BytesSent      int64   `json:"bytes_sent"`
}

// trafficEntryBuilder — 범위를 한 번에 정하며 항목 하나를 모은다(묶음 60개에서 끊는다).
type trafficEntryBuilder struct {
	sources       []trafficEntrySource
	cursorSources []trafficCursorSourceState
	missing       []string
	segments      map[trafficSegmentKey]*trafficEntrySegment
	requests      []trafficEntryRequest
	filtered      int64
	counts        map[string]trafficSourceCounts
}

func newTrafficEntryBuilder() *trafficEntryBuilder {
	return &trafficEntryBuilder{missing: []string{}, segments: map[trafficSegmentKey]*trafficEntrySegment{}, counts: map[string]trafficSourceCounts{}}
}

func shortContainerID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// addEmpty — 빈 범위(from = to = 지금의 자리).
func (builder *trafficEntryBuilder) addEmpty(container trafficCursorContainerState) {
	builder.sources = append(builder.sources, trafficEntrySource{Container: container.name, ContainerID: shortContainerID(container.id),
		From: trafficEntryPosition(filePosition(container.position)), To: trafficEntryPosition(filePosition(container.position))})
	builder.cursorSources = append(builder.cursorSources, trafficCursorSourceState{container: container.name, id: container.id,
		from: container.position, to: container.position, anchored: container.anchored})
}

// addRead — 읽은 줄을 차례로 더한다. 묶음이 60개를 넘게 되는 줄 앞에서 끊고 true(뒤의 컨테이너는 이번 항목에서 빈 범위).
func (builder *trafficEntryBuilder) addRead(container trafficCursorContainerState, read trafficRead) bool {
	counts := trafficSourceCounts{}
	to, anchored, cut := read.to, read.anchored, false
	for index, unit := range read.units {
		if unit.kind == trafficUnitLine {
			record := unit.record
			key := trafficSegmentKey{bucketStart: record.endMilliseconds / 30000 * 30000, service: record.service, caller: record.caller, class: record.class}
			if _, present := builder.segments[key]; !present && len(builder.segments) >= trafficSegmentLimit {
				cut = true
				if index == 0 {
					to, anchored = read.from, container.anchored && !read.replaced && !read.rotated
				} else {
					to = read.units[index-1].end
				}
				break
			}
			builder.addRecord(key, record)
			counts.lines++
		} else if unit.kind == trafficUnitUnparsed {
			counts.unparsed++
		} else if unit.kind == trafficUnitIgnored {
			counts.ignored++
		}
	}
	builder.counts[container.name] = counts
	builder.sources = append(builder.sources, trafficEntrySource{Container: container.name, ContainerID: shortContainerID(read.id),
		From: trafficEntryPosition(filePosition(read.from)), To: trafficEntryPosition(filePosition(to)), Lines: counts.lines, Unparsed: counts.unparsed, Ignored: counts.ignored})
	builder.cursorSources = append(builder.cursorSources, trafficCursorSourceState{container: container.name, id: read.id, from: read.from, to: to, anchored: anchored})
	return cut
}

func (builder *trafficEntryBuilder) addRecord(key trafficSegmentKey, record *trafficAccessRecord) {
	segment, present := builder.segments[key]
	if !present {
		segment = &trafficEntrySegment{BucketStartMs: key.bucketStart, Service: key.service, Caller: key.caller, Class: key.class, Buckets: make([]int64, len(trafficDurationBoundaries)+1)}
		builder.segments[key] = segment
	}
	segment.Count++
	switch record.status / 100 {
	case 2:
		segment.Status.Success++
	case 3:
		segment.Status.Redirect++
	case 4:
		segment.Status.ClientError++
	case 5:
		segment.Status.ServerError++
	default:
		segment.Status.Other++
	}
	if record.class != "request" {
		return
	}
	bucket := len(trafficDurationBoundaries)
	for index, boundary := range trafficDurationBoundaries {
		if record.durationMilliseconds <= boundary {
			bucket = index
			break
		}
	}
	segment.Buckets[bucket]++
	if segment.MaxMs == nil || record.durationMilliseconds > *segment.MaxMs {
		maximum := record.durationMilliseconds
		segment.MaxMs = &maximum
	}
	if record.method == "" || record.path == "" {
		return
	}
	if !trafficPathAllowed(record.path) {
		builder.filtered++
		return
	}
	builder.requests = append(builder.requests, trafficEntryRequest{RequestID: record.requestID, Service: record.service, Caller: record.caller,
		Method: record.method, Path: record.path, PathTruncated: record.pathTruncated, Status: record.status, EndMs: record.endMilliseconds,
		DurationMs: record.durationMilliseconds, UpstreamMs: record.upstreamMilliseconds, UpstreamStatus: record.upstreamStatus, Slot: record.slot, BytesSent: record.bytesSent})
}

// encodeTrafficJSON — 빈칸 없는 JSON · HTML 이스케이프 없음 · 끝의 줄바꿈 없음.
func encodeTrafficJSON(value any) (string, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return "", err
	}
	return strings.TrimSuffix(buffer.String(), "\n"), nil
}

// encode — body 의 글자. 요청별 줄은 차례대로 100줄 · 32,768바이트 안에 드는 만큼(담지 못하는 첫 줄에서 멈춘다), 나머지는 requests_dropped.
func (builder *trafficEntryBuilder) encode() (string, error) {
	sort.Slice(builder.sources, func(left, right int) bool { return builder.sources[left].Container < builder.sources[right].Container })
	sort.Strings(builder.missing)
	keys := make([]trafficSegmentKey, 0, len(builder.segments))
	for key := range builder.segments {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(left, right int) bool {
		first, second := keys[left], keys[right]
		if first.bucketStart != second.bucketStart {
			return first.bucketStart < second.bucketStart
		}
		if first.service != second.service {
			return first.service < second.service
		}
		if first.caller != second.caller {
			return first.caller < second.caller
		}
		return first.class < second.class
	})
	segments := make([]*trafficEntrySegment, 0, len(keys))
	for _, key := range keys {
		segments = append(segments, builder.segments[key])
	}
	sort.Slice(builder.requests, func(left, right int) bool {
		first, second := builder.requests[left], builder.requests[right]
		switch {
		case first.EndMs != second.EndMs:
			return first.EndMs < second.EndMs
		case first.RequestID != second.RequestID:
			return first.RequestID < second.RequestID
		case first.Service != second.Service:
			return first.Service < second.Service
		case first.Method != second.Method:
			return first.Method < second.Method
		}
		return first.Path < second.Path
	})
	sources, err := encodeTrafficJSON(append([]trafficEntrySource{}, builder.sources...))
	if err != nil {
		return "", err
	}
	missing, err := encodeTrafficJSON(builder.missing)
	if err != nil {
		return "", err
	}
	encodedSegments, err := encodeTrafficJSON(segments)
	if err != nil {
		return "", err
	}
	prefix := `{"sources":` + sources + `,"missing":` + missing + `,"segments":` + encodedSegments + `,"requests":[`
	suffix := func(dropped int) string {
		return `],"requests_dropped":` + strconv.Itoa(dropped) + `,"dependencies":[]}`
	}
	eligible := len(builder.requests)
	if len(prefix)+len(suffix(eligible)) > trafficBodyByteLimit {
		return "", fmt.Errorf("traffic body %d bytes without requests", len(prefix)+len(suffix(eligible)))
	}
	var encodedRequests []string
	size := len(prefix)
	for _, request := range builder.requests {
		if len(encodedRequests) >= trafficRequestLimit {
			break
		}
		encoded, err := encodeTrafficJSON(request)
		if err != nil {
			return "", err
		}
		separator := 0
		if len(encodedRequests) > 0 {
			separator = 1
		}
		if size+separator+len(encoded)+len(suffix(eligible-len(encodedRequests)-1)) > trafficBodyByteLimit {
			break
		}
		size += separator + len(encoded)
		encodedRequests = append(encodedRequests, encoded)
	}
	body := prefix + strings.Join(encodedRequests, ",") + suffix(eligible-len(encodedRequests))
	return body, nil
}
