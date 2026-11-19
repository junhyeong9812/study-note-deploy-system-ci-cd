package agent

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
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
	continuousIntegrationBudget = 12 * time.Minute // 테스트 + 이미지 빌드 (backend gradle 수 분)
	continuousDeliveryBudget    = 3 * time.Minute  // 이미지 교체만 — 빌드는 CI에서 끝났다
	healthBudget                = 90 * time.Second
	keptImageTags               = 5 // 되돌리기용으로 남기는 sha 이미지 수 (디스크 상한)
	currentImageTag             = "current"
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
	buildArguments := []string{"docker", "compose", "build"}
	if composeService := agent.composeServices[service]; composeService != "" {
		buildArguments = append(buildArguments, composeService)
	}
	if err := step(StageBuild, buildArguments...); err != nil {
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

// deliver — CD: CI가 만든 sha 이미지로 대상 컨테이너만 교체. 빌드하지 않고 의존 서비스도 건드리지 않는다.
func (agent *Agent) deliver(service, commitSha string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), continuousDeliveryBudget)
	defer cancel()
	directory := agent.directories[service]
	upArguments := []string{"docker", "compose", "up", "-d", "--no-build", "--no-deps"}
	if composeService := agent.composeServices[service]; composeService != "" {
		upArguments = append(upArguments, composeService)
	}
	output, err := agent.run(ctx, directory, imageTagEnvironment(commitSha), upArguments...)
	if err != nil {
		return output, &stageError{StageUp, output + "\n" + err.Error()}
	}
	if healthURL := agent.healthURLs[service]; healthURL != "" {
		if err := agent.waitHealthy(healthURL, healthBudget); err != nil { // compose 성공 ≠ 서비스 정상
			return output, &stageError{StageHealth, err.Error()}
		}
	}
	agent.retainImages(ctx, directory, service, commitSha)
	return output, nil
}

// retainImages — 교체 성공 뒤: 지금 버전에 `current` 꼬리표를 붙이고(IMAGE_TAG 없이 수동 기동할 때의 기본값),
// 오래된 sha 이미지를 지운다. 실패해도 배포 결과는 바꾸지 않는다(정리 작업).
func (agent *Agent) retainImages(ctx context.Context, directory, service, commitSha string) {
	imageName := agent.imageNames[service]
	if imageName == "" {
		return
	}
	if output, err := agent.run(ctx, directory, nil, "docker", "tag", imageName+":"+commitSha, imageName+":"+currentImageTag); err != nil {
		agent.logger.Log("retain", fmt.Sprintf("image tag current 실패 %s: %s", service, tail(output, 200)), "warning")
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

// staleImageTags — `생성시각\t태그` 줄 목록에서, 최근 keptImageTags개와 지금 배포한 것을 뺀 sha 태그.
// 생성시각 문자열(예: 2026-11-19 20:15:33 +0900 KST)은 사전순 = 시간순이라 그대로 정렬한다.
func staleImageTags(listing, deployedSha string) []string {
	type taggedImage struct{ createdAt, tag string }
	var images []taggedImage
	for _, line := range strings.Split(strings.TrimSpace(listing), "\n") {
		createdAt, tag, found := strings.Cut(line, "\t")
		if !found || !shared.ValidCommitSha(tag) { // current·<none> 등은 대상이 아니다
			continue
		}
		images = append(images, taggedImage{createdAt, tag})
	}
	sort.SliceStable(images, func(left, right int) bool { return images[left].createdAt > images[right].createdAt })
	var stale []string
	for index, image := range images {
		if index >= keptImageTags && image.tag != deployedSha {
			stale = append(stale, image.tag)
		}
	}
	return stale
}

// waitHealthy — 배포 후 서비스 헬스가 200이 될 때까지 폴링 (기동 시간 감안)
func waitHealthy(healthURL string, budget time.Duration) error {
	deadline := time.Now().Add(budget)
	client := &http.Client{Timeout: 3 * time.Second}
	var lastError error
	for time.Now().Before(deadline) {
		response, err := client.Get(healthURL)
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
