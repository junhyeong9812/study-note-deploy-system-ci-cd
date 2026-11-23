package agent

// GPU — 기준: requirement-spec.md(2026-11-24 resource-metrics) §2.3 · §2.5 · §10.1 · §10.2.
// 정한 세부(명세가 정하지 않은 것):
//   - 설정 METRICS_GPU_CONTAINER(컨테이너 이름 하나). 그 컨테이너 안에서 고정 명령 하나를 exec 로 실행한다:
//     nvidia-smi --query-gpu=index,utilization.gpu,memory.used,memory.total,temperature.gpu --format=csv,noheader,nounits
//     줄마다 "번호, 사용률, 메모리 사용(MiB), 메모리 전체(MiB), 온도" 다섯 칸 — MiB 는 1048576 바이트.
//   - 종료 코드가 0 이 아니거나 · 어느 줄이든 꼴이 틀리거나(칸 수 · 숫자 아님) · 줄이 하나도 없거나 · exec 를 만들 수 없으면(컨테이너 꺼짐)
//     gpus: [] + missing 에 gpus. 다른 갈래는 그대로 나간다.
//   - exec 는 Tty 없이 — 출력은 도커의 다중화된 흐름(8바이트 머리 + 내용)이고 조각이 줄 중간에서 나뉠 수 있다.

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"
	"time"
)

var gpuKeys = []string{"index", "util_pct", "mem_used", "mem_total", "temp_c"}

func newGPUHarness(t *testing.T, output string, exitCode int) *resourceHarness {
	settings := defaultResourceSettings()
	settings.GPUContainer = "llm-ollama"
	harness := newResourceHarness(t, settings)
	harness.docker.set(func(docker *fakeDocker) {
		docker.containers = []*fakeContainer{projectContainer("a1", "front-app-green", "front")}
		docker.gpuOutput, docker.gpuExitCode = output, exitCode
	})
	return harness
}

func execCreations(requests []dockerRequest) []dockerRequest {
	var creations []dockerRequest
	for _, request := range requests {
		if request.method == "POST" && dockerExecCreatePath.MatchString(request.path) {
			creations = append(creations, request)
		}
	}
	return creations
}

func TestResourceGPUsAreEmptyWithoutSetting(t *testing.T) {
	harness := newResourceHarness(t, defaultResourceSettings())
	entry := harness.collect()
	if gpus := listOf(t, entry, "gpus"); len(gpus) != 0 {
		t.Fatalf("설정이 없으면 gpus 는 []: %v", gpus)
	}
	requireMissing(t, entry, "GPU 설정 없음 — missing 에 넣지 않는다")
	if creations := execCreations(harness.docker.recorded()); len(creations) != 0 {
		t.Fatalf("설정이 없는데 명령을 실행했다: %v", creations)
	}
}

func TestResourceGPUReadsEveryLineAndConvertsMebibytes(t *testing.T) {
	harness := newGPUHarness(t, "0, 0, 9, 8192, 28\n1, 75, 4096, 24576, 61\n", 0)
	entry := harness.collect()
	gpus := listOf(t, entry, "gpus")
	if len(gpus) != 2 {
		t.Fatalf("GPU 둘을 모두 실어야 한다: %v", gpus)
	}
	for _, gpu := range gpus {
		requireKeys(t, "gpu", gpu, gpuKeys...)
	}
	requireNumber(t, "gpu0 index", gpus[0]["index"], 0, 0)
	requireNumber(t, "gpu0 util_pct", gpus[0]["util_pct"], 0, 0)
	requireNumber(t, "gpu0 mem_used(9 MiB)", gpus[0]["mem_used"], 9437184, 0)
	requireNumber(t, "gpu0 mem_total(8192 MiB)", gpus[0]["mem_total"], 8589934592, 0)
	requireNumber(t, "gpu0 temp_c", gpus[0]["temp_c"], 28, 0)
	requireNumber(t, "gpu1 index", gpus[1]["index"], 1, 0)
	requireNumber(t, "gpu1 util_pct", gpus[1]["util_pct"], 75, 0)
	requireNumber(t, "gpu1 mem_used", gpus[1]["mem_used"], 4096*1048576, 0)
	requireNumber(t, "gpu1 mem_total", gpus[1]["mem_total"], 24576*1048576, 0)
	requireNumber(t, "gpu1 temp_c", gpus[1]["temp_c"], 61, 0)
	requireMissing(t, entry, "GPU 정상")
}

func TestResourceGPUFailureMarksMissingButKeepsOtherParts(t *testing.T) {
	cases := []struct {
		situation    string
		output       string
		exitCode     int
		createStatus int
	}{
		{"NVML 초기화 실패(.158 실측)", "Failed to initialize NVML: Unknown Error\n", 255, 0},
		{"종료 코드 0 이지만 칸이 넷", "0, 9, 8192, 28\n", 0, 0},
		{"숫자가 아닌 칸", "0, [N/A], 9, 8192, 28\n", 0, 0},
		{"둘째 줄만 틀림", "0, 0, 9, 8192, 28\ngarbage\n", 0, 0},
		{"출력 없음", "", 0, 0},
		{"GPU 컨테이너가 꺼져 있음", "", 0, 409},
		{"GPU 컨테이너가 없음", "", 0, 404},
	}
	for _, testCase := range cases {
		t.Run(testCase.situation, func(t *testing.T) {
			harness := newGPUHarness(t, testCase.output, testCase.exitCode)
			harness.docker.set(func(docker *fakeDocker) { docker.execCreateStatus = testCase.createStatus })
			entry := harness.collect()
			if gpus := listOf(t, entry, "gpus"); len(gpus) != 0 {
				t.Fatalf("GPU 를 읽지 못하면 gpus 는 []: %v", gpus)
			}
			requireMissing(t, entry, testCase.situation, missingGPUs)
			if containers := listOf(t, entry, "containers"); len(containers) != 1 {
				t.Fatalf("GPU 가 실패해도 컨테이너는 그대로: %v", containers)
			}
			requireHostLevels(t, testCase.situation, hostOf(t, entry), 0.11)
			if strings.Contains(entry.text(), "NVML") {
				t.Fatal("명령의 출력 글자를 싣지 않는다")
			}
		})
	}
}

func TestResourceGPURunsOnlyFixedQueryInConfiguredContainer(t *testing.T) {
	harness := newGPUHarness(t, "0, 0, 9, 8192, 28\n", 0)
	harness.collect()
	harness.collect()
	creations := execCreations(harness.docker.recorded())
	if len(creations) != 2 {
		t.Fatalf("수집마다 명령 하나: %d", len(creations))
	}
	for _, creation := range creations {
		if name, _ := url.PathUnescape(dockerExecCreatePath.FindStringSubmatch(creation.path)[1]); name != "llm-ollama" {
			t.Fatalf("설정된 컨테이너에서만 실행한다: %s", creation.path)
		}
		var body map[string]any
		if err := json.Unmarshal(creation.body, &body); err != nil {
			t.Fatalf("exec 만들기 본문이 JSON 이 아니다: %v %q", err, creation.body)
		}
		command, _ := json.Marshal(body["Cmd"])
		want := `["nvidia-smi","--query-gpu=index,utilization.gpu,memory.used,memory.total,temperature.gpu","--format=csv,noheader,nounits"]`
		if string(command) != want {
			t.Fatalf("실행하는 명령은 고정이다: %s", command)
		}
		for _, forbidden := range []string{"Privileged", "Env", "User", "WorkingDir"} {
			if value, present := body[forbidden]; present && value != false && value != "" && value != nil {
				t.Fatalf("exec 에 %s 를 주지 않는다: %v", forbidden, value)
			}
		}
		if tty, _ := body["Tty"].(bool); tty {
			t.Fatal("Tty 없이 실행한다(출력 흐름이 다중화된다)")
		}
	}
}

// 읽기만 — 도커에 보내는 요청은 GET 조회와 GPU 용 exec(만들기 · 시작) 뿐이고, /proc 받침 파일은 그대로다.
func TestResourceCollectorSendsOnlyReadRequestsToDocker(t *testing.T) {
	settings := defaultResourceSettings()
	settings.ExtraContainers, settings.GPUContainer = []string{"claude-bridge-relay"}, "llm-ollama"
	harness := newResourceHarness(t, settings)
	harness.docker.set(func(docker *fakeDocker) {
		docker.containers = mixedContainers()
		docker.gpuOutput = "0, 0, 9, 8192, 28\n"
	})
	before := snapshotTree(t, harness.procRoot)
	harness.collect()
	harness.clock.advance(30 * time.Second)
	harness.collect()
	requireSameSnapshot(t, before, snapshotTree(t, harness.procRoot))

	var creations, starts int
	for _, request := range harness.docker.recorded() {
		switch {
		case request.method == "GET":
		case request.method == "POST" && dockerExecCreatePath.MatchString(request.path):
			creations++
		case request.method == "POST" && dockerExecStartPath.MatchString(request.path):
			starts++
		default:
			t.Fatalf("조회도 GPU 명령도 아닌 요청: %s %s?%s", request.method, request.path, request.rawQuery)
		}
	}
	if creations != 2 || starts != 2 {
		t.Fatalf("수집마다 GPU 명령 하나(만들기 · 시작): %d %d", creations, starts)
	}
}
