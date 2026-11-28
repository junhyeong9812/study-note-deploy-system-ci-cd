package agent

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/junhyeong9812/study-note-deploy-system-ci-cd/internal/shared"
)

// 실패 단계 이름 — master가 그대로 이력에 남기고 워크플로가 출력한다.
const (
	StageFetch  = "ci_fetch"
	StageTest   = "ci_test"
	StageBuild  = "ci_build"
	StageUp     = "cd_up"
	StageHealth = "cd_health"
)

const (
	continuousIntegrationBudget = 10 * time.Minute // 테스트 + 이미지 빌드 (backend gradle 수 분)
	continuousDeliveryBudget    = 4 * time.Minute  // 기동 2분 + 자리 헬스 90초 + 전환 30초 — 빌드는 CI에서 끝났다
	healthBudget                = 90 * time.Second
	retainBudget                = time.Minute // 이미지 정리 — 교체·헬스가 쓴 시간과 별개
	keptImageTags               = 5           // 되돌리기용으로 남기는 sha 이미지 수 (디스크 상한)
	outputTailLimit             = 2000
)

// commandRunner — 명령 실행 경계. 테스트는 이 함수를 바꿔 실제 git·docker 없이 순서를 검증한다.
type commandRunner func(ctx context.Context, directory string, environment []string, arguments ...string) (string, error)

func runCommand(ctx context.Context, directory string, environment []string, arguments ...string) (string, error) {
	command := exec.CommandContext(ctx, arguments[0], arguments[1:]...)
	command.Dir = directory
	command.Env = append(os.Environ(), environment...)
	output, err := command.CombinedOutput()
	return string(output), err
}

type stageError struct {
	stage  string
	output string
}

func (failure *stageError) Error() string { return failure.stage }

func imageTagEnvironment(commitSha string) []string { return []string{"IMAGE_TAG=" + commitSha} }

// integrate — CI: 받기 → 테스트 → 이미지 빌드. 실행 중인 컨테이너는 건드리지 않는다.
// 테스트·빌드가 실패하면 소스를 직전 커밋으로 되돌려, 디렉토리가 운영 중인 버전과 어긋나지 않게 한다.
func (agent *Agent) integrate(service, commitSha string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), continuousIntegrationBudget)
	defer cancel()
	directory := agent.directories[service]
	var combined strings.Builder

	previousSha, err := agent.run(ctx, directory, nil, "git", "-C", directory, "rev-parse", "HEAD")
	previousSha = strings.TrimSpace(previousSha)
	if err != nil || !shared.ValidCommitSha(previousSha) {
		return previousSha, &stageError{StageFetch, "현재 커밋 확인 실패: " + previousSha}
	}
	// 되돌릴 기준은 "운영 중인 버전"이다. 앞선 배포가 CI만 통과하고 CD에서 실패했다면 HEAD는 운영과 다르다.
	if _, runningSha := agent.runningImage(ctx, directory, service); shared.ValidCommitSha(runningSha) {
		previousSha = runningSha
	} else { // 실행 중인 컨테이너가 없거나 sha 태그 이전 방식으로 떠 있다 — 알 수 있는 기준은 HEAD뿐
		agent.logger.Log("ci", fmt.Sprintf("운영 버전 확인 불가 %s(태그 %q) — 원복 기준을 HEAD %s 로", service, runningSha, previousSha), "warning")
	}
	step := func(stage string, arguments ...string) error {
		output, err := agent.run(ctx, directory, imageTagEnvironment(commitSha), arguments...)
		combined.WriteString(output)
		if err != nil {
			return &stageError{stage, combined.String() + "\n" + err.Error()}
		}
		return nil
	}
	restore := func(failure error) (string, error) {
		// 되돌리기는 CI 시간 예산과 별개로 반드시 시도한다 (예산 초과로 실패한 경우에도)
		restoreCtx, restoreCancel := context.WithTimeout(context.Background(), time.Minute)
		defer restoreCancel()
		if output, err := agent.run(restoreCtx, directory, nil, "git", "-C", directory, "reset", "--hard", previousSha); err != nil {
			var staged *stageError
			if errors.As(failure, &staged) {
				staged.output += "\n소스 원복 실패(" + previousSha + "): " + output
			}
		}
		return combined.String(), failure
	}

	if err := step(StageFetch, "git", "-C", directory, "fetch", "--depth=50", "origin", "main"); err != nil {
		return combined.String(), err // 아직 소스를 바꾸지 않았다
	}
	if err := step(StageFetch, "git", "-C", directory, "reset", "--hard", commitSha); err != nil {
		return restore(err)
	}
	// 테스트 계약: repo의 Dockerfile `test` 단계. 무엇을 돌릴지는 repo가 정한다.
	if err := step(StageTest, "docker", "build", "--target", "test", agent.testContext(service)); err != nil {
		return restore(err)
	}
	if err := step(StageBuild, "docker", "compose", "build", agent.buildService(service)); err != nil {
		return restore(err)
	}
	return combined.String(), nil
}

func (agent *Agent) testContext(service string) string {
	if testContext := agent.testContexts[service]; testContext != "" {
		return testContext
	}
	return "."
}

// buildService — 이미지를 빌드할 compose 서비스. 두 자리는 같은 이미지를 쓰므로 한쪽만 빌드하면 된다.
func (agent *Agent) buildService(service string) string {
	if agent.modes[service] == modeBlueGreen {
		return agent.slotService(service, slotBlue)
	}
	return agent.composeServices[service]
}

// runningService — 지금 운영 중인 compose 서비스. 두 자리 방식이면 nginx가 실제로 보내는 자리(응답 기준).
// 외부 포트가 응답하지 않으면(서비스가 죽어 있어 고치는 배포일 수 있다) 활성 파일의 표식을 차선으로 쓴다.
func (agent *Agent) runningService(directory, service string) string {
	if agent.modes[service] != modeBlueGreen {
		return agent.composeServices[service]
	}
	slot, err := agent.servingSlot(agent.healthURLs[service])
	if err != nil {
		content, readErr := os.ReadFile(filepath.Join(directory, activeFile))
		if readErr != nil || markedSlot(string(content)) == "" {
			return ""
		}
		slot = markedSlot(string(content))
		agent.logger.Log("ci", fmt.Sprintf("%s 실제 자리 확인 불가(%v) — 활성 파일의 자리 %s 를 기준으로 삼음", service, err, slot), "warning")
	}
	return agent.slotService(service, slot)
}

// runningImage — 지금 실행 중인 대상 컨테이너의 이미지 이름과 태그. 알 수 없으면 빈 문자열.
func (agent *Agent) runningImage(ctx context.Context, directory, service string) (name, tag string) {
	runningService := agent.runningService(directory, service)
	if runningService == "" {
		return "", ""
	}
	containerIDs, err := agent.run(ctx, directory, nil, "docker", "compose", "ps", "-q", runningService)
	containerID, _, _ := strings.Cut(strings.TrimSpace(containerIDs), "\n")
	if err != nil || containerID == "" {
		return "", ""
	}
	image, err := agent.run(ctx, directory, nil, "docker", "inspect", "--format", "{{.Config.Image}}", containerID)
	if err != nil {
		return "", ""
	}
	image = strings.TrimSpace(image)
	separator := strings.LastIndex(image, ":")
	if separator < 0 {
		return image, ""
	}
	return image[:separator], image[separator+1:]
}

// deliver — CD: CI가 만든 sha 이미지로 대상 컨테이너만 교체. 빌드하지 않고 의존 서비스도 건드리지 않는다.
func (agent *Agent) deliver(service, commitSha string) (string, error) {
	if agent.modes[service] == modeBlueGreen {
		return agent.deliverBlueGreen(service, commitSha)
	}
	ctx, cancel := context.WithTimeout(context.Background(), continuousDeliveryBudget)
	defer cancel()
	directory := agent.directories[service]
	healthURL := agent.healthURLs[service]
	if healthURL == "" { // 헬스를 확인할 수 없는 배포는 성공이라 말할 수 없다 — 운영을 바꾸기 전에 거절
		return "", &stageError{StageHealth, "DEPLOY_HEALTH_" + strings.ToUpper(service) + " 미설정 — 교체하지 않음"}
	}
	output, err := agent.run(ctx, directory, imageTagEnvironment(commitSha),
		"docker", "compose", "up", "-d", "--no-build", "--no-deps", agent.composeServices[service])
	if err != nil {
		return output, &stageError{StageUp, output + "\n" + err.Error()}
	}
	if err := agent.waitHealthy(healthURL, healthBudget); err != nil { // compose 성공 ≠ 서비스 정상
		return output, &stageError{StageHealth, err.Error()}
	}
	agent.retainImages(directory, service, commitSha)
	return output, nil
}

// retainImages — 교체 성공 뒤 오래된 sha 이미지를 지운다(디스크 상한). 실패해도 배포 결과는 바꾸지 않되 로그에 남긴다.
func (agent *Agent) retainImages(directory, service, commitSha string) {
	ctx, cancel := context.WithTimeout(context.Background(), retainBudget)
	defer cancel()
	imageName, runningTag := agent.runningImage(ctx, directory, service)
	if imageName == "" || runningTag != commitSha {
		agent.logger.Log("retain", fmt.Sprintf("image 정리 생략 %s: 실행 중 이미지 %q:%q 가 배포한 sha와 다름", service, imageName, runningTag), "warning")
		return
	}
	listing, err := agent.run(ctx, directory, nil, "docker", "images", imageName, "--format", "{{.CreatedAt}}\t{{.Tag}}")
	if err != nil {
		agent.logger.Log("retain", fmt.Sprintf("image 목록 실패 %s: %s", service, tail(listing, 200)), "warning")
		return
	}
	for _, tag := range staleImageTags(listing, commitSha) {
		if output, err := agent.run(ctx, directory, nil, "docker", "rmi", imageName+":"+tag); err != nil {
			agent.logger.Log("retain", fmt.Sprintf("image 삭제 실패 %s:%s: %s", imageName, tag, tail(output, 200)), "warning")
		}
	}
}

// staleImageTags — `생성시각\t태그` 줄 목록에서 지울 sha 태그. 남기는 것은 지금 배포한 태그 + 최신순으로 합계 keptImageTags개.
// 생성시각 문자열(예: 2026-11-19 20:15:33 +0900 KST)은 사전순 = 시간순이라 그대로 정렬한다.
func staleImageTags(listing, deployedSha string) []string {
	type taggedImage struct{ createdAt, tag string }
	var images []taggedImage
	for _, line := range strings.Split(strings.TrimSpace(listing), "\n") {
		createdAt, tag, found := strings.Cut(line, "\t")
		if !found || !shared.ValidCommitSha(tag) || tag == deployedSha { // local·<none> 등은 대상이 아니다
			continue
		}
		images = append(images, taggedImage{createdAt, tag})
	}
	sort.SliceStable(images, func(left, right int) bool { return images[left].createdAt > images[right].createdAt })
	var stale []string
	for index, image := range images {
		if index >= keptImageTags-1 { // 한 자리는 지금 배포한 태그 몫
			stale = append(stale, image.tag)
		}
	}
	return stale
}

// getWithFreshTrackingID — 들어온 요청 없이 시작되는 호출(배포 중의 헬스 · 자리 확인): 호출마다 새 추적 ID 를 싣는다.
func getWithFreshTrackingID(client *http.Client, target string) (*http.Response, error) {
	request, err := http.NewRequest(http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set(shared.RequestIDHeader, shared.NewTrackingID())
	return client.Do(request)
}

// waitHealthy — 배포 후 서비스 헬스가 200이 될 때까지 폴링 (기동 시간 감안)
func waitHealthy(healthURL string, budget time.Duration) error {
	deadline := time.Now().Add(budget)
	client := &http.Client{Timeout: 3 * time.Second}
	var lastError error
	for time.Now().Before(deadline) {
		response, err := getWithFreshTrackingID(client, healthURL)
		if err == nil {
			response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return nil
			}
			lastError = fmt.Errorf("status %d", response.StatusCode)
		} else {
			lastError = err
		}
		time.Sleep(3 * time.Second)
	}
	return fmt.Errorf("health 미도달: %v", lastError)
}

// tail — 출력의 끝부분. 빌드·테스트 오류는 끝에 있다.
func tail(input string, limit int) string {
	if len(input) <= limit {
		return input
	}
	return input[len(input)-limit:]
}
