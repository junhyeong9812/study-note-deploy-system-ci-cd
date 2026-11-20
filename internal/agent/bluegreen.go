package agent

import (
	"context"
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

// probeSlot — nginx가 붙인 자리 표식과, 응답이 200인지.
// 200이 아니어도 표식은 돌려준다: 앱이 죽어 nginx가 502를 내는 상황에서도 "nginx가 어느 자리로 보내는지"는 알 수 있다.
func probeSlot(healthURL string) (string, error) {
	response, err := (&http.Client{Timeout: 3 * time.Second}).Get(healthURL)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	slot := response.Header.Get(slotHeader)
	if response.StatusCode != http.StatusOK {
		return slot, fmt.Errorf("status %d", response.StatusCode)
	}
	return slot, nil
}

// servingSlot — nginx가 지금 보내는 자리. 응답이 200이 아니어도 표식이 있으면 그 자리다(운영 중인 앱이 죽은 경우).
// nginx 자체가 응답하지 않으면 알 수 없다 — 그때만 오류.
func (agent *Agent) servingSlot(publicURL string) (string, error) {
	var slot string
	var err error
	for attempt := 0; attempt < 3; attempt++ { // 일시적인 시간 초과 한 번으로 "모름"이 되지 않게
		slot, err = agent.probeSlot(publicURL)
		if validSlot(slot) {
			return slot, nil
		}
		time.Sleep(agent.probeInterval)
	}
	if err == nil {
		err = fmt.Errorf("자리 표식 없음(%q)", slot)
	}
	return "", err
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
	// 0. 앞선 정리가 끝난 뒤에 시작한다 — 두 자리를 동시에 재사용하지 않는다. 끝나지 않으면 진행하지 않는다.
	if !agent.waitDrained(service) {
		return "", &stageError{StageUp, "앞선 배포의 자리 정리가 상한을 넘겨도 끝나지 않음 — 서버에서 두 자리 상태 확인 필요"}
	}
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
	activeSlot, err := agent.servingSlot(publicURL)
	if err != nil {
		return "", &stageError{StageSwitch, "실제 활성 자리를 확인할 수 없음(nginx 무응답 또는 표식 없음): " + err.Error()}
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
	// 활성 파일을 이전 내용으로 되돌린다. 실패하면 파일이 대상 자리를 가리킨 채 남는다 —
	// 그 상태에서 대상 자리를 내리면 nginx가 다시 뜰 때 갈 곳이 없으므로, 호출한 쪽은 대상 자리를 내리지 않는다.
	restoreActiveFile := func() bool {
		if err := writeActiveFile(directory, previousContent); err != nil {
			combined.WriteString("\n되돌림 실패(활성 파일 복원) — 대상 자리를 내리지 않고 둔다(서버에서 확인 필요): " + err.Error() + "\n")
			agent.logger.Log("cd", fmt.Sprintf("%s 활성 파일 복원 실패 — 두 자리 유지: %v", service, err), "error")
			return false
		}
		return true
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
		if restoreActiveFile() {
			undo("대상 자리 정지", "stop", targetService)
		}
		return fail(StageSwitch)
	}
	if err := compose(nil, "exec", "-T", proxyService, "nginx", "-t"); err != nil {
		if restoreActiveFile() {
			undo("대상 자리 정지", "stop", targetService)
		}
		return fail(StageSwitch)
	}
	// 7~8. reload → 새 자리가 실제로 응답하는지. reload를 시도한 뒤에는 대상 자리에 연결이 들어갔을 수 있다:
	//      되돌리더라도 대상 자리를 바로 내리지 않고 정리 절차로 넘긴다.
	revert := func() (string, error) {
		restored := restoreActiveFile()
		undo("nginx 되돌림 reload", "exec", "-T", proxyService, "nginx", "-s", "reload")
		if !restored {
			return fail(StageSwitch)
		}
		// 구 자리로 돌아온 것을 확인한 뒤에만 대상 자리를 정리한다. 확인하지 못하면 nginx가 대상 자리로
		// 보내고 있을 수 있다 — 어느 자리도 내리지 않고 사람이 보게 남긴다.
		if err := agent.waitSlot(publicURL, activeSlot); err != nil {
			combined.WriteString("\n되돌림 미확인 — 두 자리를 모두 그대로 둔다(서버에서 확인 필요): " + err.Error() + "\n")
			agent.logger.Log("cd", fmt.Sprintf("%s 되돌림 미확인 — 두 자리 유지: %v", service, err), "error")
			return fail(StageSwitch)
		}
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
// deployedSha가 있으면(성공한 배포 뒤) 오래된 이미지도 정리한다 — 다음 CD는 이미지 정리를 기다리지 않는다.
func (agent *Agent) startDrain(service, slot, deployedSha string) {
	done := make(chan struct{})
	agent.drainMutex.Lock()
	previous := agent.draining[service]
	agent.draining[service] = done
	agent.drainMutex.Unlock()
	agent.background.Add(1)
	go func() {
		defer agent.background.Done()
		if previous != nil {
			<-previous // 같은 서비스의 정리는 하나씩
		}
		agent.drain(service, slot)
		agent.drainMutex.Lock()
		if agent.draining[service] == done {
			delete(agent.draining, service)
		}
		agent.drainMutex.Unlock()
		close(done)
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
	// 기다림과 정지는 시간 예산을 따로 쓴다 — 상한까지 기다린 뒤에도 컨테이너가 정상 종료할 시간이 남아야 한다.
	stopCtx, stopCancel := context.WithTimeout(context.Background(), agent.drainLimit+agent.drainWaitMargin)
	defer stopCancel()
	for time.Now().Before(deadline) {
		// 종료 중인 nginx worker가 남아 있다 = 전환 전에 열린 연결이 아직 있다
		processes, err := agent.run(ctx, directory, nil, "docker", "compose", "exec", "-T", proxyService, "ps")
		if err == nil && !strings.Contains(processes, "shutting down") {
			break
		}
		time.Sleep(agent.drainPollInterval)
	}
	// 내리기 직전에 한 번 더 확인한다: nginx가 지금 이 자리로 보내고 있거나 알 수 없으면 내리지 않는다.
	// (되돌림이 실제로는 적용되지 않았거나, 그사이 상태가 바뀐 경우 운영 중인 자리를 내리지 않기 위해)
	servingSlot, err := agent.servingSlot(agent.healthURLs[service])
	if err != nil || servingSlot == slot {
		agent.logger.Log("drain", fmt.Sprintf("자리 정지 보류 %s — nginx가 보내는 자리 %q, 오류 %v. 다음 배포가 실제 상태를 보고 정리한다", slotService, servingSlot, err), "error")
		return
	}
	if output, err := agent.run(stopCtx, directory, nil, "docker", "compose", "stop", slotService); err != nil {
		agent.logger.Log("drain", fmt.Sprintf("자리 정지 실패 %s — 다음 배포가 다시 내린다: %s", slotService, tail(output, 200)), "error")
		return
	}
	agent.logger.Log("drain", "자리 정리 완료 "+slotService, "info")
}

// waitDrained — 같은 서비스의 정리가 끝날 때까지 기다린다. 상한(정리 상한 + 여유)을 넘으면 false.
func (agent *Agent) waitDrained(service string) bool {
	agent.drainMutex.Lock()
	done := agent.draining[service]
	agent.drainMutex.Unlock()
	if done == nil {
		return true
	}
	select {
	case <-done:
		return true
	case <-time.After(agent.drainLimit + agent.drainWaitMargin):
		return false
	}
}

// waitBackground — 진행 중인 뒷일이 끝날 때까지.
func (agent *Agent) waitBackground() { agent.background.Wait() }

// ReconcileSlots — agent 기동 시: 배포 성공 직후 agent가 죽어 두 벌이 남았을 수 있다.
// 실제 활성 자리의 반대쪽을 정리한다. 실제 자리를 모르면 아무것도 내리지 않는다.
func (agent *Agent) ReconcileSlots() {
	for service := range agent.directories {
		agent.reconcileSlot(service)
	}
}

// ReconcileSlotsUntil — 서버가 막 부팅했으면 앱과 nginx가 뜨는 데 시간이 걸린다.
// 실제 자리를 확인할 수 있을 때까지 간격을 두고 다시 시도한다(상한 있음).
func (agent *Agent) ReconcileSlotsUntil(budget, interval time.Duration) {
	pending := map[string]bool{}
	for service := range agent.directories {
		if agent.modes[service] == modeBlueGreen {
			pending[service] = true
		}
	}
	deadline := time.Now().Add(budget) // budget이 0 이하면 확인될 때까지 계속한다
	for len(pending) > 0 {
		for service := range pending {
			if agent.reconcileSlot(service) {
				delete(pending, service)
			}
		}
		if len(pending) == 0 || (budget > 0 && !time.Now().Before(deadline)) {
			break
		}
		time.Sleep(interval)
	}
	for service := range pending {
		agent.logger.Log("boot", service+" 활성 자리를 끝내 확인하지 못함 — 두 자리가 떠 있을 수 있다", "error")
	}
}

// reconcileSlot — 실제 자리를 확인해 정리를 넘겼으면 true.
// CI·CD와 같은 잠금을 쥐고 판단·등록한다: CD가 대상 자리를 띄우는 사이에 그 자리를 정리 대상으로 삼지 않게.
func (agent *Agent) reconcileSlot(service string) bool {
	if agent.modes[service] != modeBlueGreen {
		return true
	}
	agent.busy.Lock()
	defer agent.busy.Unlock()
	activeSlot, err := agent.servingSlot(agent.healthURLs[service])
	if err != nil {
		agent.logger.Log("boot", fmt.Sprintf("%s 활성 자리 확인 불가(%v) — 정리하지 않음", service, err), "warning")
		return false
	}
	// 활성 파일이 실제와 다르면 맞춘다(파일을 바꾼 직후 agent가 죽은 경우 — nginx가 다시 뜰 때 엉뚱한 자리를 읽지 않게).
	// 맞추지 못하면 반대쪽을 내리지 않는다: 파일이 가리키는 자리를 내리면 nginx 재기동 때 갈 곳이 없어진다.
	directory := agent.directories[service]
	content, err := os.ReadFile(filepath.Join(directory, activeFile))
	if err != nil || markedSlot(string(content)) != activeSlot {
		template, templateErr := readSlotTemplate(directory, activeSlot)
		if templateErr == nil {
			templateErr = writeActiveFile(directory, template)
		}
		if templateErr != nil {
			agent.logger.Log("boot", fmt.Sprintf("%s 활성 파일을 실제 자리(%s)에 맞추지 못함 — 정리 보류: %v", service, activeSlot, templateErr), "error")
			return false
		}
		agent.logger.Log("boot", fmt.Sprintf("%s 활성 파일을 실제 자리(%s)에 맞춤", service, activeSlot), "warning")
	}
	agent.startDrain(service, otherSlot(activeSlot), "")
	return true
}
