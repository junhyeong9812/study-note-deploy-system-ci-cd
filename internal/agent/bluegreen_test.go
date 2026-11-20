package agent

// blue/green 전환 계약 테스트 — 기준: docs/cross-cutting/plans/2026-11-20/zero-downtime-deploy/{requirement-spec.md ②, design.md}.
// 구현을 보지 않고 design.md "테스트 접점"만 쓴다(blind).
// 가짜 세계(slotWorld)는 nginx를 흉내 낸다: reload가 성공하면 그 순간 활성 파일 첫 줄의 자리로 보내기 시작하고,
// probe는 그 자리를 돌려준다.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/junhyeong9812/study-note-deploy-system-ci-cd/internal/shared"
)

const (
	slotFirstRequestSha  = "4444444444444444444444444444444444444444"
	slotSecondRequestSha = "5555555555555555555555555555555555555555"

	slotExternalHealthURL = "http://127.0.0.1:8000/health"
	slotBlueHealthURL     = "http://127.0.0.1:18001/health"
	slotGreenHealthURL    = "http://127.0.0.1:18002/health"
	slotBlueDrainURL      = "http://127.0.0.1:18001/internal/drain"
	slotGreenDrainURL     = "http://127.0.0.1:18002/internal/drain"

	slotBlueTemplate  = "# slot: blue\nupstream wrapper_upstream { server wrapper-blue:8000; }\n"
	slotGreenTemplate = "# slot: green\nupstream wrapper_upstream { server wrapper-green:8000; }\n"
	// 템플릿과 다른 "이전 내용" — 복원이 템플릿 재작성이 아니라 보관한 내용 그대로인지 가른다(첫 줄 표식은 blue로 같다).
	slotPreviousBlueContent = slotBlueTemplate + "# 손으로 남긴 줄 — 복원은 이 내용 그대로여야 한다\n"

	slotNginxCommand     = "exec -T nginx nginx"
	slotNginxCheck       = "docker compose exec -T nginx nginx -t"
	slotNginxReload      = "docker compose exec -T nginx nginx -s reload"
	slotNginxProcessList = "exec -T nginx ps"
	slotStopBlue         = "docker compose stop wrapper-blue"
	slotStopGreen        = "docker compose stop wrapper-green"
	slotUpBlue           = "docker compose up -d --no-build --no-deps wrapper-blue"
	slotUpGreen          = "docker compose up -d --no-build --no-deps wrapper-green"

	slotShuttingDownListing = "    1 root      0:00 nginx: master process nginx\n   30 nginx     0:00 nginx: worker process is shutting down\n   31 nginx     0:00 nginx: worker process\n"
	slotIdleListing         = "    1 root      0:00 nginx: master process nginx\n   31 nginx     0:00 nginx: worker process\n"
)

// slotEvent — 가짜 세계에서 일어난 일 하나. kind = run · health · probe · drain · ps-result.
type slotEvent struct {
	kind          string
	text          string
	activeContent string // run 시점의 활성 파일 내용("" = 파일 없음)
	environment   []string
}

type slotWorld struct {
	mutex          sync.Mutex
	activePath     string
	events         []slotEvent
	failures       map[string]int // 명령 조각 → 남은 실패 횟수(-1 = 항상)
	servingSlot    string         // nginx가 실제로 보내는 자리
	probeError     error
	reloadIgnored  bool          // reload가 성공을 돌려주지만 적용되지 않는다
	drainNeverEnds bool          // nginx ps가 언제나 shutting down
	drainHold      chan struct{} // 닫히기 전까지 nginx ps가 shutting down
	drainReleased  bool
	unhealthyURL   string
}

func (world *slotWorld) readActive() string {
	content, err := os.ReadFile(world.activePath)
	if err != nil {
		return ""
	}
	return string(content)
}

func slotMarker(content string) string {
	firstLine := strings.SplitN(content, "\n", 2)[0]
	if !strings.HasPrefix(firstLine, "# slot: ") {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(firstLine, "# slot: "))
}

func (world *slotWorld) drainIsHeld() bool {
	if world.drainNeverEnds {
		return true
	}
	if world.drainHold == nil {
		return false
	}
	select {
	case <-world.drainHold:
		return false
	default:
		return true
	}
}

func (world *slotWorld) holdDrain() {
	world.mutex.Lock()
	defer world.mutex.Unlock()
	world.drainHold = make(chan struct{})
	world.drainReleased = false
}

func (world *slotWorld) releaseDrain() {
	world.mutex.Lock()
	defer world.mutex.Unlock()
	if world.drainHold != nil && !world.drainReleased {
		close(world.drainHold)
		world.drainReleased = true
	}
}

func (world *slotWorld) run(_ context.Context, _ string, environment []string, arguments ...string) (string, error) {
	line := strings.Join(arguments, " ")
	world.mutex.Lock()
	defer world.mutex.Unlock()
	activeContent := world.readActive()
	world.events = append(world.events, slotEvent{
		kind:          "run",
		text:          line,
		activeContent: activeContent,
		environment:   append([]string(nil), environment...),
	})
	for fragment, remaining := range world.failures {
		if remaining != 0 && strings.Contains(line, fragment) {
			if remaining > 0 {
				world.failures[fragment] = remaining - 1
			}
			return "boom output", errors.New("exit status 1")
		}
	}
	if strings.Contains(line, slotNginxProcessList) {
		if world.drainIsHeld() {
			world.events = append(world.events, slotEvent{kind: "ps-result", text: "shutting"})
			return slotShuttingDownListing, nil
		}
		world.events = append(world.events, slotEvent{kind: "ps-result", text: "idle"})
		return slotIdleListing, nil
	}
	if line == slotNginxReload && !world.reloadIgnored {
		if slot := slotMarker(activeContent); slot != "" {
			world.servingSlot = slot
		}
	}
	return "", nil
}

func (world *slotWorld) waitHealthy(healthURL string, _ time.Duration) error {
	world.mutex.Lock()
	defer world.mutex.Unlock()
	world.events = append(world.events, slotEvent{kind: "health", text: healthURL})
	if healthURL == world.unhealthyURL {
		return errors.New("health 미도달: status 503")
	}
	return nil
}

func (world *slotWorld) probe(healthURL string) (string, error) {
	world.mutex.Lock()
	defer world.mutex.Unlock()
	world.events = append(world.events, slotEvent{kind: "probe", text: healthURL})
	if world.probeError != nil {
		return "", world.probeError
	}
	return world.servingSlot, nil
}

func (world *slotWorld) notify(drainURL string) error {
	world.mutex.Lock()
	defer world.mutex.Unlock()
	world.events = append(world.events, slotEvent{kind: "drain", text: drainURL})
	return nil
}

func (world *slotWorld) snapshot() []slotEvent {
	world.mutex.Lock()
	defer world.mutex.Unlock()
	return append([]slotEvent(nil), world.events...)
}

func (world *slotWorld) setFailure(fragment string, times int) {
	world.mutex.Lock()
	defer world.mutex.Unlock()
	world.failures[fragment] = times
}

// slotFind — from 이후 kind가 같고 text에 fragment가 들어간 첫 사건의 위치. 없으면 -1.
func slotFind(events []slotEvent, kind string, fragment string, from int) int {
	if from < 0 {
		return -1
	}
	for index := from; index < len(events); index++ {
		if events[index].kind == kind && strings.Contains(events[index].text, fragment) {
			return index
		}
	}
	return -1
}

func slotCount(events []slotEvent, kind string, fragment string) int {
	count := 0
	for _, event := range events {
		if event.kind == kind && strings.Contains(event.text, fragment) {
			count++
		}
	}
	return count
}

func slotRunCount(events []slotEvent) int {
	return slotCount(events, "run", "")
}

func slotDescribe(events []slotEvent) string {
	lines := make([]string, 0, len(events))
	for _, event := range events {
		lines = append(lines, event.kind+": "+event.text)
	}
	return strings.Join(lines, "\n")
}

// slotLastProcessResultBefore — before 앞의 마지막 ps-result 값. 없으면 "".
func slotLastProcessResultBefore(events []slotEvent, before int) string {
	for index := before - 1; index >= 0; index-- {
		if events[index].kind == "ps-result" {
			return events[index].text
		}
	}
	return ""
}

type slotSetup struct {
	activeContent     string
	withoutActiveFile bool
	withoutSlotHealth bool
	servingSlot       string
}

func slotNewAgent(t *testing.T, setup slotSetup) (*Agent, *slotWorld) {
	t.Helper()
	directory := t.TempDir()
	nginxDirectory := filepath.Join(directory, "deploy", "nginx")
	activeDirectory := filepath.Join(directory, "deploy", "active")
	for _, path := range []string{nginxDirectory, activeDirectory} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatalf("테스트 디렉토리 준비 실패: %v", err)
		}
	}
	if err := os.WriteFile(filepath.Join(nginxDirectory, "upstream-blue.conf"), []byte(slotBlueTemplate), 0o644); err != nil {
		t.Fatalf("blue 템플릿 준비 실패: %v", err)
	}
	if err := os.WriteFile(filepath.Join(nginxDirectory, "upstream-green.conf"), []byte(slotGreenTemplate), 0o644); err != nil {
		t.Fatalf("green 템플릿 준비 실패: %v", err)
	}
	activePath := filepath.Join(activeDirectory, "upstream.conf")
	if !setup.withoutActiveFile {
		if err := os.WriteFile(activePath, []byte(setup.activeContent), 0o644); err != nil {
			t.Fatalf("활성 파일 준비 실패: %v", err)
		}
	}

	t.Setenv("DEPLOY_SECRET", "test-value")
	t.Setenv("DEPLOY_DIR_LLM", directory)
	t.Setenv("DEPLOY_SVC_LLM", "wrapper")
	t.Setenv("DEPLOY_TEST_CONTEXT_LLM", "wrapper")
	t.Setenv("DEPLOY_MODE_LLM", "bluegreen")
	t.Setenv("DEPLOY_HEALTH_LLM", slotExternalHealthURL)
	slotHealthKeys := map[string]string{
		"DEPLOY_HEALTH_LLM_BLUE":  slotBlueHealthURL,
		"DEPLOY_HEALTH_LLM_GREEN": slotGreenHealthURL,
	}
	for key, value := range slotHealthKeys {
		if setup.withoutSlotHealth {
			t.Setenv(key, "") // 끝나면 원래 값으로 돌려놓게 등록만 하고
			os.Unsetenv(key)  // 테스트 동안은 아예 없앤다
			continue
		}
		t.Setenv(key, value)
	}
	t.Setenv("DEPLOY_DRAIN_LLM_BLUE", slotBlueDrainURL)
	t.Setenv("DEPLOY_DRAIN_LLM_GREEN", slotGreenDrainURL)

	world := &slotWorld{
		activePath:  activePath,
		failures:    map[string]int{},
		servingSlot: setup.servingSlot,
	}
	agent := New(shared.NewLogger())
	agent.run = world.run
	agent.waitHealthy = world.waitHealthy
	agent.probeSlot = world.probe
	agent.notifyDrain = world.notify
	agent.drainPollInterval = 5 * time.Millisecond
	agent.drainLimit = 5 * time.Second
	agent.probeInterval = 5 * time.Millisecond
	agent.probeBudget = 300 * time.Millisecond
	t.Cleanup(func() {
		world.releaseDrain()
		done := make(chan struct{})
		go func() {
			agent.waitBackground()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Errorf("테스트가 끝났는데 뒷일이 10초 안에 끝나지 않는다")
		}
	})
	return agent, world
}

func slotPost(agent *Agent, sha string) *httptest.ResponseRecorder {
	mux := http.NewServeMux()
	agent.Register(mux)
	request := httptest.NewRequest("POST", "/agent/cd", strings.NewReader(`{"service":"llm","commit_sha":"`+sha+`","request_id":"gh-slot"}`))
	request.Header.Set(shared.SecretHeader, "test-value")
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, request)
	return recorder
}

func slotWaitBackground(t *testing.T, agent *Agent) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		agent.waitBackground()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("뒷일(구 자리 정리)이 10초 안에 끝나지 않는다")
	}
}

func slotRequireFailure(t *testing.T, recorder *httptest.ResponseRecorder, code string) {
	t.Helper()
	if recorder.Code != 500 || !strings.Contains(recorder.Body.String(), `"code":"`+code+`"`) {
		t.Fatalf("500 %s여야 한다: %d %s", code, recorder.Code, recorder.Body.String())
	}
}

func slotRequireActiveContent(t *testing.T, world *slotWorld, want string, situation string) {
	t.Helper()
	world.mutex.Lock()
	content := world.readActive()
	world.mutex.Unlock()
	if content != want {
		t.Fatalf("%s: 활성 파일 내용이 다르다\n기대:\n%s\n실제:\n%s", situation, want, content)
	}
}

// 정상 전환 — design 1~9단계 순서와, 성공 응답 뒤에야 도는 구 자리 정리.
func TestBlueGreenSwitchRunsStepsInOrderAndCleansOldSlotAfterResponse(t *testing.T) {
	agent, world := slotNewAgent(t, slotSetup{activeContent: slotBlueTemplate, servingSlot: "blue"})
	world.holdDrain() // 구 자리 정리를 붙잡아 둔다 — 응답 시점에 구 자리가 살아 있는지 본다

	recorder := slotPost(agent, slotFirstRequestSha)
	if recorder.Code != 200 {
		t.Fatalf("전환 성공이어야 하는데 %d %s", recorder.Code, recorder.Body.String())
	}
	events := world.snapshot()
	// 2. 대상(green) 정지
	stopIndex := slotFind(events, "run", slotStopGreen, 0)
	// 3. 대상 기동
	upIndex := slotFind(events, "run", slotUpGreen, stopIndex+1)
	// 4. 대상 자리 헬스
	healthIndex := slotFind(events, "health", slotGreenHealthURL, upIndex+1)
	// 6. 설정 검사
	checkIndex := slotFind(events, "run", slotNginxCheck, healthIndex+1)
	// 7. reload
	reloadIndex := slotFind(events, "run", slotNginxReload, checkIndex+1)
	// 8. 자리 확인
	probeIndex := slotFind(events, "probe", slotExternalHealthURL, reloadIndex+1)
	if stopIndex < 0 || upIndex < 0 || healthIndex < 0 || checkIndex < 0 || reloadIndex < 0 || probeIndex < 0 {
		t.Fatalf("전환 순서(대상 정지 → 기동 → 자리 헬스 → nginx -t → reload → probe)가 다르다:\n%s", slotDescribe(events))
	}
	if !strings.Contains(strings.Join(events[upIndex].environment, " "), "IMAGE_TAG="+slotFirstRequestSha) {
		t.Fatalf("대상 기동에 sha 태그가 전달되지 않았다: %v", events[upIndex].environment)
	}
	if firstNginx := slotFind(events, "run", slotNginxCommand, 0); firstNginx < healthIndex {
		t.Fatalf("자리 헬스 전에 nginx를 건드렸다:\n%s", slotDescribe(events))
	}
	// 5. 활성 파일은 설정 검사 전에 대상 템플릿 내용으로 바뀌어 있어야 한다
	if events[checkIndex].activeContent != slotGreenTemplate {
		t.Fatalf("nginx -t 시점의 활성 파일이 green 템플릿이 아니다:\n%s", events[checkIndex].activeContent)
	}
	slotRequireActiveContent(t, world, slotGreenTemplate, "전환 뒤")
	if slotCount(events, "run", "wrapper-blue") != 0 {
		t.Fatalf("성공 응답 전에 구 자리(blue)에 명령을 보냈다:\n%s", slotDescribe(events))
	}
	if slotFind(events, "run", slotStopGreen, upIndex+1) >= 0 {
		t.Fatalf("성공한 대상 자리를 내렸다:\n%s", slotDescribe(events))
	}

	// 9. 뒷일 — 알림 → shutting down 없어질 때까지 → 구 자리 정지
	world.releaseDrain()
	slotWaitBackground(t, agent)
	events = world.snapshot()
	drainIndex := slotFind(events, "drain", slotBlueDrainURL, probeIndex+1)
	oldStopIndex := slotFind(events, "run", slotStopBlue, probeIndex+1)
	if drainIndex < 0 || oldStopIndex < 0 || drainIndex > oldStopIndex {
		t.Fatalf("구 자리 정리가 알림 → 정지 순서가 아니다:\n%s", slotDescribe(events))
	}
	if last := slotLastProcessResultBefore(events, oldStopIndex); last != "idle" {
		t.Fatalf("shutting down이 없어진 것을 확인하기 전에 구 자리를 내렸다(마지막 ps 결과 %q):\n%s", last, slotDescribe(events))
	}
	if slotCount(events, "drain", slotGreenDrainURL) != 0 || slotFind(events, "run", slotStopGreen, upIndex+1) >= 0 {
		t.Fatalf("새 활성 자리(green)를 정리했다:\n%s", slotDescribe(events))
	}
}

// 1단계 — 활성 파일이 없거나 표식이 없으면 cd_switch, 명령 0개.
func TestBlueGreenRefusesWhenActiveFileIsMissingOrUnmarked(t *testing.T) {
	cases := map[string]slotSetup{
		"활성 파일 없음": {withoutActiveFile: true, servingSlot: "blue"},
		"표식 없음":    {activeContent: "upstream wrapper_upstream { server wrapper-blue:8000; }\n", servingSlot: "blue"},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			agent, world := slotNewAgent(t, setup)
			recorder := slotPost(agent, slotFirstRequestSha)
			slotRequireFailure(t, recorder, "cd_switch")
			slotWaitBackground(t, agent)
			if events := world.snapshot(); slotRunCount(events) != 0 {
				t.Fatalf("활성 자리를 모르는데 명령을 실행했다:\n%s", slotDescribe(events))
			}
		})
	}
}

// 1단계 — probe 실패(외부 포트 무응답)면 cd_switch, 명령 0개.
func TestBlueGreenRefusesWhenProbeFails(t *testing.T) {
	agent, world := slotNewAgent(t, slotSetup{activeContent: slotBlueTemplate, servingSlot: "blue"})
	world.probeError = errors.New("connection refused")
	recorder := slotPost(agent, slotFirstRequestSha)
	slotRequireFailure(t, recorder, "cd_switch")
	slotWaitBackground(t, agent)
	if events := world.snapshot(); slotRunCount(events) != 0 {
		t.Fatalf("운영 상태를 모르는데 명령을 실행했다:\n%s", slotDescribe(events))
	}
	slotRequireActiveContent(t, world, slotBlueTemplate, "probe 실패 뒤")
}

// 1단계 — 파일(green)과 probe(blue)가 다르면 probe를 따른다: 파일을 blue 템플릿으로 고치고, 대상은 green.
func TestBlueGreenFollowsProbeWhenActiveFileDisagrees(t *testing.T) {
	agent, world := slotNewAgent(t, slotSetup{activeContent: slotGreenTemplate, servingSlot: "blue"})
	world.holdDrain()
	recorder := slotPost(agent, slotFirstRequestSha)
	if recorder.Code != 200 {
		t.Fatalf("전환 성공이어야 하는데 %d %s", recorder.Code, recorder.Body.String())
	}
	events := world.snapshot()
	firstRun := slotFind(events, "run", "", 0)
	if firstRun < 0 {
		t.Fatalf("명령이 하나도 없다:\n%s", slotDescribe(events))
	}
	if events[firstRun].activeContent != slotBlueTemplate {
		t.Fatalf("첫 명령 전에 활성 파일을 probe 자리(blue)의 템플릿으로 고치지 않았다:\n%s", events[firstRun].activeContent)
	}
	if slotFind(events, "run", slotUpGreen, 0) < 0 || slotFind(events, "run", "wrapper-blue", 0) >= 0 {
		t.Fatalf("대상 자리는 probe의 반대쪽(green)이어야 한다:\n%s", slotDescribe(events))
	}
	slotRequireActiveContent(t, world, slotGreenTemplate, "전환 뒤")
}

// 2단계 — 대상 자리 정지 실패면 cd_up으로 중단, 기동 명령 없음.
func TestBlueGreenAbortsWhenTargetSlotCannotBeStopped(t *testing.T) {
	agent, world := slotNewAgent(t, slotSetup{activeContent: slotPreviousBlueContent, servingSlot: "blue"})
	world.setFailure(slotStopGreen, -1)
	recorder := slotPost(agent, slotFirstRequestSha)
	slotRequireFailure(t, recorder, "cd_up")
	slotWaitBackground(t, agent)
	events := world.snapshot()
	if slotFind(events, "run", "up -d", 0) >= 0 || slotFind(events, "run", slotNginxCommand, 0) >= 0 {
		t.Fatalf("대상 자리를 비우지 못했는데 기동·전환을 진행했다:\n%s", slotDescribe(events))
	}
	if slotFind(events, "run", "wrapper-blue", 0) >= 0 {
		t.Fatalf("운영 중인 자리(blue)를 건드렸다:\n%s", slotDescribe(events))
	}
	slotRequireActiveContent(t, world, slotPreviousBlueContent, "정지 실패 뒤")
}

// 3·4단계 실패의 공통 기대 — nginx 무변경, 활성 파일 불변, 대상 자리만 내린다.
func slotRequireOperationUntouched(t *testing.T, agent *Agent, world *slotWorld) {
	t.Helper()
	slotWaitBackground(t, agent)
	events := world.snapshot()
	if slotFind(events, "run", slotNginxCommand, 0) >= 0 {
		t.Fatalf("전환 전 실패인데 nginx를 건드렸다:\n%s", slotDescribe(events))
	}
	slotRequireActiveContent(t, world, slotPreviousBlueContent, "전환 전 실패 뒤")
	upIndex := slotFind(events, "run", slotUpGreen, 0)
	if upIndex < 0 || slotFind(events, "run", slotStopGreen, upIndex+1) < 0 {
		t.Fatalf("기동을 시도한 대상 자리(green)를 내리지 않았다:\n%s", slotDescribe(events))
	}
	if slotFind(events, "run", "wrapper-blue", 0) >= 0 || slotCount(events, "drain", slotBlueDrainURL) != 0 {
		t.Fatalf("운영 중인 자리(blue)를 건드렸다:\n%s", slotDescribe(events))
	}
}

// 3단계 — 기동 실패 cd_up.
func TestBlueGreenStartFailureLeavesOperationUntouched(t *testing.T) {
	agent, world := slotNewAgent(t, slotSetup{activeContent: slotPreviousBlueContent, servingSlot: "blue"})
	world.setFailure(slotUpGreen, -1)
	recorder := slotPost(agent, slotFirstRequestSha)
	slotRequireFailure(t, recorder, "cd_up")
	slotRequireOperationUntouched(t, agent, world)
}

// 4단계 — 자리 헬스 실패 cd_health.
func TestBlueGreenSlotHealthFailureLeavesOperationUntouched(t *testing.T) {
	agent, world := slotNewAgent(t, slotSetup{activeContent: slotPreviousBlueContent, servingSlot: "blue"})
	world.unhealthyURL = slotGreenHealthURL
	recorder := slotPost(agent, slotFirstRequestSha)
	slotRequireFailure(t, recorder, "cd_health")
	if !strings.Contains(recorder.Body.String(), "503") {
		t.Fatalf("헬스 실패 출력이 응답에 없다: %s", recorder.Body.String())
	}
	slotRequireOperationUntouched(t, agent, world)
}

// 6단계 — nginx -t 실패: 활성 파일 복원, reload 없음, 대상 정지, cd_switch.
func TestBlueGreenConfigCheckFailureRestoresFileWithoutReload(t *testing.T) {
	agent, world := slotNewAgent(t, slotSetup{activeContent: slotPreviousBlueContent, servingSlot: "blue"})
	world.setFailure(slotNginxCheck, -1)
	recorder := slotPost(agent, slotFirstRequestSha)
	slotRequireFailure(t, recorder, "cd_switch")
	slotWaitBackground(t, agent)
	events := world.snapshot()
	checkIndex := slotFind(events, "run", slotNginxCheck, 0)
	if checkIndex < 0 || events[checkIndex].activeContent != slotGreenTemplate {
		t.Fatalf("nginx -t가 green 템플릿을 검사하지 않았다:\n%s", slotDescribe(events))
	}
	if slotFind(events, "run", slotNginxReload, 0) >= 0 {
		t.Fatalf("설정 검사 실패인데 reload했다:\n%s", slotDescribe(events))
	}
	slotRequireActiveContent(t, world, slotPreviousBlueContent, "설정 검사 실패 뒤")
	if slotFind(events, "run", slotStopGreen, checkIndex+1) < 0 {
		t.Fatalf("설정 검사 실패 뒤 대상 자리(green)를 내리지 않았다:\n%s", slotDescribe(events))
	}
	if slotFind(events, "run", "wrapper-blue", 0) >= 0 {
		t.Fatalf("운영 중인 자리(blue)를 건드렸다:\n%s", slotDescribe(events))
	}
}

// 7·8단계 실패의 공통 기대 — 파일 복원 → 되돌림 reload → 대상은 바로 내리지 않고 자리 정리로.
func slotRequireRolledBackAfterReload(t *testing.T, agent *Agent, world *slotWorld, recorder *httptest.ResponseRecorder) {
	t.Helper()
	slotRequireFailure(t, recorder, "cd_switch")
	events := world.snapshot()
	upIndex := slotFind(events, "run", slotUpGreen, 0)
	firstReload := slotFind(events, "run", slotNginxReload, 0)
	rollbackReload := slotFind(events, "run", slotNginxReload, firstReload+1)
	if upIndex < 0 || firstReload < 0 || rollbackReload < 0 {
		t.Fatalf("전환 reload 뒤 되돌림 reload가 없다:\n%s", slotDescribe(events))
	}
	if events[rollbackReload].activeContent != slotPreviousBlueContent {
		t.Fatalf("되돌림 reload 전에 활성 파일을 이전 내용으로 복원하지 않았다:\n%s", events[rollbackReload].activeContent)
	}
	slotRequireActiveContent(t, world, slotPreviousBlueContent, "reload 뒤 실패")
	if slotFind(events, "run", slotStopGreen, upIndex+1) >= 0 {
		t.Fatalf("reload를 시도한 뒤인데 대상 자리(green)를 즉시 내렸다:\n%s", slotDescribe(events))
	}

	world.releaseDrain()
	slotWaitBackground(t, agent)
	events = world.snapshot()
	targetStop := slotFind(events, "run", slotStopGreen, upIndex+1)
	if targetStop < 0 {
		t.Fatalf("자리 정리에서 대상 자리(green)를 내리지 않았다:\n%s", slotDescribe(events))
	}
	if last := slotLastProcessResultBefore(events, targetStop); last != "idle" {
		t.Fatalf("대상 자리를 남은 연결 확인 없이 내렸다(마지막 ps 결과 %q):\n%s", last, slotDescribe(events))
	}
	if slotFind(events, "run", "wrapper-blue", 0) >= 0 || slotCount(events, "drain", slotBlueDrainURL) != 0 {
		t.Fatalf("되돌린 운영 자리(blue)를 건드렸다:\n%s", slotDescribe(events))
	}
}

// 7단계 — reload 실패.
func TestBlueGreenReloadFailureRestoresAndDrainsTargetLater(t *testing.T) {
	agent, world := slotNewAgent(t, slotSetup{activeContent: slotPreviousBlueContent, servingSlot: "blue"})
	world.setFailure(slotNginxReload, 1) // 전환 reload만 실패, 되돌림 reload는 성공
	world.holdDrain()
	recorder := slotPost(agent, slotFirstRequestSha)
	slotRequireRolledBackAfterReload(t, agent, world, recorder)
}

// 8단계 — reload는 성공을 돌려주지만 probe가 계속 구 자리.
func TestBlueGreenReloadNotTakingEffectRestoresAndDrainsTargetLater(t *testing.T) {
	agent, world := slotNewAgent(t, slotSetup{activeContent: slotPreviousBlueContent, servingSlot: "blue"})
	world.reloadIgnored = true
	world.holdDrain()
	recorder := slotPost(agent, slotFirstRequestSha)
	events := world.snapshot()
	reloadIndex := slotFind(events, "run", slotNginxReload, 0)
	if slotFind(events, "probe", slotExternalHealthURL, reloadIndex+1) < 0 {
		t.Fatalf("reload 뒤 자리 확인을 하지 않았다:\n%s", slotDescribe(events))
	}
	slotRequireRolledBackAfterReload(t, agent, world, recorder)
}

// 자리 정리 상한 — shutting down이 끝나지 않아도 drainLimit 뒤에는 구 자리를 정지한다.
func TestBlueGreenOldSlotIsStoppedAfterDrainLimitEvenIfStillShuttingDown(t *testing.T) {
	agent, world := slotNewAgent(t, slotSetup{activeContent: slotBlueTemplate, servingSlot: "blue"})
	world.drainNeverEnds = true
	agent.drainLimit = 100 * time.Millisecond
	recorder := slotPost(agent, slotFirstRequestSha)
	if recorder.Code != 200 {
		t.Fatalf("전환 성공이어야 하는데 %d %s", recorder.Code, recorder.Body.String())
	}
	slotWaitBackground(t, agent)
	events := world.snapshot()
	if slotCount(events, "ps-result", "shutting") == 0 {
		t.Fatalf("남은 연결을 확인하지 않았다:\n%s", slotDescribe(events))
	}
	if slotFind(events, "run", slotStopBlue, 0) < 0 {
		t.Fatalf("상한이 지났는데 구 자리(blue)를 내리지 않았다:\n%s", slotDescribe(events))
	}
}

// 0단계 — 정리 진행 중에 온 다음 CD는 정리가 끝난 뒤에야 명령을 시작한다.
func TestBlueGreenNextDeliveryWaitsForCleanup(t *testing.T) {
	agent, world := slotNewAgent(t, slotSetup{activeContent: slotBlueTemplate, servingSlot: "blue"})
	world.holdDrain()
	if recorder := slotPost(agent, slotFirstRequestSha); recorder.Code != 200 {
		t.Fatalf("첫 전환 성공이어야 하는데 %d %s", recorder.Code, recorder.Body.String())
	}

	// 정리 대기(ps 확인·알림)를 뺀, CD가 스스로 시작하는 일의 수
	countDeliveryWork := func(events []slotEvent) int {
		count := 0
		for _, event := range events {
			switch {
			case event.kind == "run" && !strings.Contains(event.text, slotNginxProcessList):
				count++
			case event.kind == "probe" || event.kind == "health":
				count++
			}
		}
		return count
	}
	workBefore := countDeliveryWork(world.snapshot())

	secondResponse := make(chan *httptest.ResponseRecorder, 1)
	go func() { secondResponse <- slotPost(agent, slotSecondRequestSha) }()
	time.Sleep(200 * time.Millisecond)

	eventsWhileHeld := world.snapshot()
	if workAfter := countDeliveryWork(eventsWhileHeld); workAfter != workBefore {
		t.Fatalf("구 자리 정리가 끝나기 전에 다음 CD가 일을 시작했다:\n%s", slotDescribe(eventsWhileHeld))
	}
	select {
	case recorder := <-secondResponse:
		t.Fatalf("정리 중인데 다음 CD가 기다리지 않고 응답했다: %d %s", recorder.Code, recorder.Body.String())
	default:
	}

	world.releaseDrain()
	var recorder *httptest.ResponseRecorder
	select {
	case recorder = <-secondResponse:
	case <-time.After(10 * time.Second):
		t.Fatal("정리가 끝났는데 다음 CD가 진행하지 않는다")
	}
	if recorder.Code != 200 {
		t.Fatalf("정리 뒤의 다음 CD는 성공이어야 하는데 %d %s", recorder.Code, recorder.Body.String())
	}
	slotWaitBackground(t, agent)
	events := world.snapshot()
	cleanupStop := slotFind(events, "run", slotStopBlue, 0)
	nextUp := slotFind(events, "run", slotUpBlue, 0)
	if cleanupStop < 0 || nextUp < 0 || nextUp < cleanupStop {
		t.Fatalf("구 자리(blue) 정리 정지가 다음 CD의 기동보다 먼저여야 한다:\n%s", slotDescribe(events))
	}
	if !strings.Contains(strings.Join(events[nextUp].environment, " "), "IMAGE_TAG="+slotSecondRequestSha) {
		t.Fatalf("다음 CD의 기동에 두 번째 sha가 전달되지 않았다: %v", events[nextUp].environment)
	}
}

// 설정 — 자리별 헬스 URL이 없으면 cd_health로 거절, 운영 무변경.
func TestBlueGreenRefusesWithoutSlotHealthURLs(t *testing.T) {
	agent, world := slotNewAgent(t, slotSetup{activeContent: slotPreviousBlueContent, servingSlot: "blue", withoutSlotHealth: true})
	recorder := slotPost(agent, slotFirstRequestSha)
	slotRequireFailure(t, recorder, "cd_health")
	slotWaitBackground(t, agent)
	events := world.snapshot()
	for _, fragment := range []string{"compose stop", "up -d", slotNginxCommand} {
		if slotFind(events, "run", fragment, 0) >= 0 {
			t.Fatalf("자리 헬스를 확인할 수 없는데 운영을 건드렸다(%s):\n%s", fragment, slotDescribe(events))
		}
	}
	slotRequireActiveContent(t, world, slotPreviousBlueContent, "자리 헬스 미설정 거절 뒤")
}

// agent 기동 시 정리 — probe가 가리키는 자리(blue)의 반대쪽(green)을 자리 정리 절차로 내린다.
func TestReconcileSlotsCleansSlotOppositeToProbe(t *testing.T) {
	// 파일은 green이라고 하지만 실제(probe)는 blue — 정리 대상은 실제의 반대쪽이다
	agent, world := slotNewAgent(t, slotSetup{activeContent: slotGreenTemplate, servingSlot: "blue"})
	agent.ReconcileSlots()
	slotWaitBackground(t, agent)
	events := world.snapshot()
	if slotFind(events, "probe", slotExternalHealthURL, 0) < 0 {
		t.Fatalf("기동 시 정리가 실제 활성 자리를 확인하지 않았다:\n%s", slotDescribe(events))
	}
	drainIndex := slotFind(events, "drain", slotGreenDrainURL, 0)
	stopIndex := slotFind(events, "run", slotStopGreen, 0)
	if drainIndex < 0 || stopIndex < 0 || drainIndex > stopIndex {
		t.Fatalf("반대쪽 자리(green)를 알림 → 정지 순서로 정리하지 않았다:\n%s", slotDescribe(events))
	}
	if last := slotLastProcessResultBefore(events, stopIndex); last != "idle" {
		t.Fatalf("남은 연결 확인 없이 반대쪽 자리를 내렸다(마지막 ps 결과 %q):\n%s", last, slotDescribe(events))
	}
	if slotFind(events, "run", "wrapper-blue", 0) >= 0 || slotCount(events, "drain", slotBlueDrainURL) != 0 {
		t.Fatalf("실제 활성 자리(blue)를 건드렸다:\n%s", slotDescribe(events))
	}
}

// agent 기동 시 정리 — probe가 실패하면 아무것도 내리지 않는다.
func TestReconcileSlotsStopsNothingWhenProbeFails(t *testing.T) {
	agent, world := slotNewAgent(t, slotSetup{activeContent: slotBlueTemplate, servingSlot: "blue"})
	world.probeError = errors.New("connection refused")
	agent.ReconcileSlots()
	slotWaitBackground(t, agent)
	events := world.snapshot()
	if slotFind(events, "run", "compose stop", 0) >= 0 || slotCount(events, "drain", "") != 0 {
		t.Fatalf("실제 활성 자리를 모르는데 자리를 정리했다:\n%s", slotDescribe(events))
	}
}
