package agent

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// blue/green 전환 — 새 버전을 비어 있는 자리에 띄워 헬스를 확인한 뒤 nginx 대상을 바꾼다.
// 판정 기준은 "nginx가 실제로 어느 자리로 보내는가"(응답의 X-Deploy-Slot)이고,
// 활성 파일은 nginx에 원하는 자리를 전하는 수단이다.

const (
	StageSwitch = "cd_switch"

	slotBlue  = "blue"
	slotGreen = "green"

	modeBlueGreen    = "bluegreen"
	slotHeader       = "X-Deploy-Slot"
	slotMarkerPrefix = "# slot: "
	proxyService     = "nginx"
	activeFile       = "deploy/active/upstream.conf"
	templatePattern  = "deploy/nginx/upstream-%s.conf"

	switchBudget = 30 * time.Second // nginx 검사·reload
)

func otherSlot(slot string) string {
	if slot == slotBlue {
		return slotGreen
	}
	return slotBlue
}

func validSlot(slot string) bool { return slot == slotBlue || slot == slotGreen }

func (agent *Agent) slotService(service, slot string) string {
	return agent.composeServices[service] + "-" + slot
}

// markedSlot — 파일 첫 줄의 `# slot: blue|green`. 표식이 없으면 빈 문자열.
func markedSlot(content string) string {
	firstLine, _, _ := strings.Cut(content, "\n")
	slot := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(firstLine), strings.TrimSpace(slotMarkerPrefix)))
	if !strings.HasPrefix(strings.TrimSpace(firstLine), strings.TrimSpace(slotMarkerPrefix)) || !validSlot(slot) {
		return ""
	}
	return slot
}

func readSlotTemplate(directory, slot string) (string, error) {
	content, err := os.ReadFile(filepath.Join(directory, fmt.Sprintf(templatePattern, slot)))
	if err != nil {
		return "", err
	}
	if markedSlot(string(content)) != slot {
		return "", fmt.Errorf("자리 템플릿에 `%s%s` 표식이 없음", slotMarkerPrefix, slot)
	}
	return string(content), nil
}

// writeActiveFile — 임시 파일에 쓴 뒤 이름을 바꿔 한 번에 교체한다(nginx가 반쯤 쓰인 파일을 읽지 않게).
func writeActiveFile(directory, content string) error {
	path := filepath.Join(directory, activeFile)
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, []byte(content), 0o644); err != nil {
		return err
	}
	// agent는 컨테이너 안에서 root로 돈다 — 원래 소유자를 유지해 서버에서 손으로 고칠 수 있게 둔다(실패해도 전환과 무관)
	if existing, err := os.Stat(path); err == nil {
		if owner, ok := existing.Sys().(*syscall.Stat_t); ok {
			_ = os.Chown(temporary, int(owner.Uid), int(owner.Gid))
		}
	}
	return os.Rename(temporary, path)
}

// probeSlot — 외부 헬스 URL이 200이고, nginx가 붙인 자리 표식이 무엇인지.
func probeSlot(healthURL string) (string, error) {
	response, err := (&http.Client{Timeout: 3 * time.Second}).Get(healthURL)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("status %d", response.StatusCode)
	}
	return response.Header.Get(slotHeader), nil
}

func notifyDrain(drainURL string) error {
	response, err := (&http.Client{Timeout: 5 * time.Second}).Post(drainURL, "application/json", strings.NewReader("{}"))
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode >= 300 {
		return fmt.Errorf("status %d", response.StatusCode)
	}
	return nil
}

// deliverBlueGreen — CD. 전환 전에 실패하면 nginx를 건드리지 않고, reload를 시도한 뒤 실패하면 이전 자리로 되돌린다.
func (agent *Agent) deliverBlueGreen(service, commitSha string) (string, error) {
	agent.waitDrained(service) // 0. 앞선 정리가 끝난 뒤에 시작한다 — 두 자리를 동시에 재사용하지 않는다
	directory := agent.directories[service]
	publicURL := agent.healthURLs[service]
	slotURLs := agent.slotHealthURLs[service]
	if publicURL == "" || slotURLs[slotBlue] == "" || slotURLs[slotGreen] == "" {
		return "", &stageError{StageHealth, "DEPLOY_HEALTH_" + strings.ToUpper(service) + "(_BLUE·_GREEN) 미설정 — 교체하지 않음"}
	}

	// 1. 활성 파일과 실제 자리. 모르는 채로는 전환하지 않는다.
	currentContent, err := os.ReadFile(filepath.Join(directory, activeFile))
	if err != nil {
		return "", &stageError{StageSwitch, "활성 파일을 읽을 수 없음: " + err.Error()}
	}
	previousContent := string(currentContent)
	if markedSlot(previousContent) == "" {
		return "", &stageError{StageSwitch, "활성 파일에 자리 표식(`" + slotMarkerPrefix + "blue|green`)이 없음"}
	}
	activeSlot, err := agent.probeSlot(publicURL)
	if err != nil || !validSlot(activeSlot) {
		return "", &stageError{StageSwitch, fmt.Sprintf("실제 활성 자리를 확인할 수 없음(자리 %q): %v", activeSlot, err)}
	}
	if markedSlot(previousContent) != activeSlot { // 파일과 실제가 다르면 실제를 따른다
		template, err := readSlotTemplate(directory, activeSlot)
		if err != nil {
			return "", &stageError{StageSwitch, "활성 파일을 실제 자리에 맞출 수 없음: " + err.Error()}
		}
		if err := writeActiveFile(directory, template); err != nil {
			return "", &stageError{StageSwitch, "활성 파일을 실제 자리에 맞출 수 없음: " + err.Error()}
		}
		agent.logger.Log("cd", fmt.Sprintf("활성 파일(%s)이 실제 자리(%s)와 달라 파일을 고침 %s", markedSlot(previousContent), activeSlot, service), "warning")
		previousContent = template
	}
	targetSlot := otherSlot(activeSlot)
	targetService := agent.slotService(service, targetSlot)

	ctx, cancel := context.WithTimeout(context.Background(), continuousDeliveryBudget)
	defer cancel()
	var combined strings.Builder
	compose := func(environment []string, arguments ...string) error {
		output, err := agent.run(ctx, directory, environment, append([]string{"docker", "compose"}, arguments...)...)
		combined.WriteString(output)
		if err != nil {
			combined.WriteString("\n" + err.Error() + "\n")
		}
		return err
	}
	// 되돌림은 CD 시간 예산과 별개로 반드시 시도하고, 실패하면 그 사실을 출력에 남긴다.
	undo := func(description string, arguments ...string) {
		undoCtx, undoCancel := context.WithTimeout(context.Background(), switchBudget)
		defer undoCancel()
		if output, err := agent.run(undoCtx, directory, nil, append([]string{"docker", "compose"}, arguments...)...); err != nil {
			combined.WriteString(fmt.Sprintf("\n되돌림 실패(%s): %s %v\n", description, tail(output, 300), err))
		}
	}
	restoreActiveFile := func() {
		if err := writeActiveFile(directory, previousContent); err != nil {
			combined.WriteString("\n되돌림 실패(활성 파일 복원): " + err.Error() + "\n")
		}
	}
	fail := func(stage string) (string, error) { return combined.String(), &stageError{stage, combined.String()} }

	// 2. 지난 정리가 실패해 남아 있을 수 있는 대상을 내린다. 비우지 못하면 진행하지 않는다.
	if err := compose(nil, "stop", targetService); err != nil {
		return fail(StageUp)
	}
	// 3. 대상 자리에 새 버전 기동 — 빌드하지 않고 의존 서비스도 건드리지 않는다.
	if err := compose(imageTagEnvironment(commitSha), "up", "-d", "--no-build", "--no-deps", targetService); err != nil {
		undo("대상 자리 정지", "stop", targetService)
		return fail(StageUp)
	}
	// 4. 대상 자리의 헬스 — 통과하기 전에는 nginx를 건드리지 않는다.
	if err := agent.waitHealthy(slotURLs[targetSlot], healthBudget); err != nil {
		combined.WriteString("\n" + err.Error() + "\n")
		undo("대상 자리 정지", "stop", targetService)
		return fail(StageHealth)
	}
	// 5~6. 활성 파일 교체 → 설정 검사. 여기까지는 nginx가 아직 구 자리로 보낸다.
	template, err := readSlotTemplate(directory, targetSlot)
	if err == nil {
		err = writeActiveFile(directory, template)
	}
	if err != nil {
		combined.WriteString("\n활성 파일 교체 실패: " + err.Error() + "\n")
		restoreActiveFile()
		undo("대상 자리 정지", "stop", targetService)
		return fail(StageSwitch)
	}
	if err := compose(nil, "exec", "-T", proxyService, "nginx", "-t"); err != nil {
		restoreActiveFile()
		undo("대상 자리 정지", "stop", targetService)
		return fail(StageSwitch)
	}
	// 7~8. reload → 새 자리가 실제로 응답하는지. reload를 시도한 뒤에는 대상 자리에 연결이 들어갔을 수 있다:
	//      되돌리더라도 대상 자리를 바로 내리지 않고 정리 절차로 넘긴다.
	revert := func() (string, error) {
		restoreActiveFile()
		undo("nginx 되돌림 reload", "exec", "-T", proxyService, "nginx", "-s", "reload")
		agent.startDrain(service, targetSlot, "")
		return fail(StageSwitch)
	}
	if err := compose(nil, "exec", "-T", proxyService, "nginx", "-s", "reload"); err != nil {
		return revert()
	}
	if err := agent.waitSlot(publicURL, targetSlot); err != nil {
		combined.WriteString("\n" + err.Error() + "\n")
		return revert()
	}
	// 9. 성공. 구 자리 정리는 뒷일이다.
	agent.startDrain(service, activeSlot, commitSha)
	return combined.String(), nil
}

// waitSlot — nginx가 지정한 자리로 보내기 시작할 때까지. 200이어도 자리가 다르면 통과가 아니다.
func (agent *Agent) waitSlot(publicURL, wantedSlot string) error {
	deadline := time.Now().Add(agent.probeBudget)
	var lastSlot string
	var lastError error
	for {
		lastSlot, lastError = agent.probeSlot(publicURL)
		if lastError == nil && lastSlot == wantedSlot {
			return nil
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("전환 미확인: 원하는 자리 %s, 응답한 자리 %q, 오류 %v", wantedSlot, lastSlot, lastError)
		}
		time.Sleep(agent.probeInterval)
	}
}

// startDrain — 자리 정리(뒷일). 남은 연결이 끝나면 바로, 아니면 상한에 그 자리를 내린다.
// deployedSha가 있으면(성공한 배포 뒤) 오래된 이미지도 정리한다.
func (agent *Agent) startDrain(service, slot, deployedSha string) {
	done := make(chan struct{})
	agent.drainMutex.Lock()
	previous := agent.draining[service]
	agent.draining[service] = done
	agent.drainMutex.Unlock()
	agent.background.Add(1)
	go func() {
		defer agent.background.Done()
		defer func() {
			agent.drainMutex.Lock()
			if agent.draining[service] == done {
				delete(agent.draining, service)
			}
			agent.drainMutex.Unlock()
			close(done)
		}()
		if previous != nil {
			<-previous // 같은 서비스의 정리는 하나씩
		}
		agent.drain(service, slot)
		if deployedSha != "" {
			agent.retainImages(agent.directories[service], service, deployedSha)
		}
	}()
}

func (agent *Agent) drain(service, slot string) {
	directory := agent.directories[service]
	slotService := agent.slotService(service, slot)
	if drainURL := agent.slotDrainURLs[service][slot]; drainURL != "" {
		if err := agent.notifyDrain(drainURL); err != nil {
			agent.logger.Log("drain", fmt.Sprintf("정리 알림 실패 %s: %v", slotService, err), "warning")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), agent.drainLimit+switchBudget)
	defer cancel()
	deadline := time.Now().Add(agent.drainLimit)
	for time.Now().Before(deadline) {
		// 종료 중인 nginx worker가 남아 있다 = 전환 전에 열린 연결이 아직 있다
		processes, err := agent.run(ctx, directory, nil, "docker", "compose", "exec", "-T", proxyService, "ps")
		if err == nil && !strings.Contains(processes, "shutting down") {
			break
		}
		time.Sleep(agent.drainPollInterval)
	}
	if output, err := agent.run(ctx, directory, nil, "docker", "compose", "stop", slotService); err != nil {
		agent.logger.Log("drain", fmt.Sprintf("자리 정지 실패 %s — 다음 배포가 다시 내린다: %s", slotService, tail(output, 200)), "error")
		return
	}
	agent.logger.Log("drain", "자리 정리 완료 "+slotService, "info")
}

func (agent *Agent) waitDrained(service string) {
	agent.drainMutex.Lock()
	done := agent.draining[service]
	agent.drainMutex.Unlock()
	if done != nil {
		<-done
	}
}

// waitBackground — 진행 중인 뒷일이 끝날 때까지.
func (agent *Agent) waitBackground() { agent.background.Wait() }

// ReconcileSlots — agent 기동 시: 배포 성공 직후 agent가 죽어 두 벌이 남았을 수 있다.
// 실제 활성 자리의 반대쪽을 정리한다. 실제 자리를 모르면 아무것도 내리지 않는다.
func (agent *Agent) ReconcileSlots() {
	for service := range agent.directories {
		if agent.modes[service] != modeBlueGreen {
			continue
		}
		activeSlot, err := agent.probeSlot(agent.healthURLs[service])
		if err != nil || !validSlot(activeSlot) {
			agent.logger.Log("boot", fmt.Sprintf("%s 활성 자리 확인 불가(자리 %q, %v) — 정리하지 않음", service, activeSlot, err), "warning")
			continue
		}
		agent.startDrain(service, otherSlot(activeSlot), "")
	}
}

// activeSlotFromFile — CI의 원복 기준·이미지 정리용. 네트워크 없이 파일 표식만 본다.
func activeSlotFromFile(directory string) (string, error) {
	content, err := os.ReadFile(filepath.Join(directory, activeFile))
	if err != nil {
		return "", err
	}
	if slot := markedSlot(string(content)); slot != "" {
		return slot, nil
	}
	return "", errors.New("활성 파일에 자리 표식이 없음")
}
