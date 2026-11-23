package agent

// 도커 엔진 API — 유닉스 소켓 위의 HTTP 를 직접 부른다(새 의존성 없음). 자원 지표 수집만 쓴다.
// 허용된 요청: GET 조회(/containers/json · /containers/{id}/json · /containers/{id}/stats?stream=false&one-shot=true ·
// /exec/{id}/json) + GPU 값을 읽는 고정 명령 실행 하나(POST /containers/{설정된 이름}/exec · POST /exec/{id}/start).
// 모든 요청은 부르는 쪽의 ctx(기한)를 그대로 쓴다.

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
)

const dockerSocketPath = "/var/run/docker.sock"

const (
	dockerResponseByteLimit = 8 << 20  // 응답 하나를 해석할 상한(목록 · 조회 · 통계)
	dockerExecOutputLimit   = 64 << 10 // GPU 명령의 출력 상한
)

// dockerAPI — 테스트는 baseURL·client 를 가짜 HTTP 서버로 바꾼다.
type dockerAPI struct {
	baseURL string       // 소켓이면 "http://docker" — 경로는 이 뒤에 붙는다(API 판 접두사 없이)
	client  *http.Client // 소켓이면 유닉스 소켓으로 잇는 Transport
}

// newDockerAPI — socketPath 의 유닉스 소켓으로 잇는 클라이언트.
func newDockerAPI(socketPath string) *dockerAPI {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var dialer net.Dialer
			return dialer.DialContext(ctx, "unix", socketPath)
		},
		MaxIdleConns:    2,
		IdleConnTimeout: resourceMetricsInterval * 2,
	}
	return &dockerAPI{baseURL: "http://docker", client: &http.Client{Transport: transport}}
}

// dockerListedContainer — GET /containers/json 의 원소에서 쓰는 칸만. 명령줄 · 주소 · 마운트 · 이미지는 해석하지 않는다.
type dockerListedContainer struct {
	ID     string            `json:"Id"`
	Names  []string          `json:"Names"`
	Labels map[string]string `json:"Labels"` // compose 프로젝트 라벨 하나만 읽는다
	State  string            `json:"State"`
}

// dockerContainerInspect — GET /containers/{id}/json 에서 쓰는 칸(환경 변수 · 명령줄은 해석하지 않는다).
type dockerContainerInspect struct {
	RestartCount int `json:"RestartCount"`
}

// dockerContainerStats — 한 번짜리 통계에서 쓰는 칸.
type dockerContainerStats struct {
	CPUStats struct {
		CPUUsage struct {
			TotalUsage uint64 `json:"total_usage"`
		} `json:"cpu_usage"`
	} `json:"cpu_stats"`
	MemoryStats struct {
		Usage *uint64 `json:"usage"`
		Stats struct {
			TotalInactiveFile *uint64 `json:"total_inactive_file"` // cgroup v1
			InactiveFile      *uint64 `json:"inactive_file"`       // cgroup v2
		} `json:"stats"`
	} `json:"memory_stats"`
}

func (docker *dockerAPI) request(ctx context.Context, method, path string, body any) (*http.Response, error) {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, docker.baseURL+path, reader)
	if err != nil {
		return nil, err
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := docker.client.Do(request)
	if err != nil {
		return nil, err
	}
	if response.StatusCode < 200 || response.StatusCode > 299 {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		response.Body.Close()
		if response.StatusCode == http.StatusNotFound {
			return nil, fmt.Errorf("docker %s %s: %w", method, strings.SplitN(path, "?", 2)[0], errDockerNotFound)
		}
		return nil, fmt.Errorf("docker %s %s: status %d", method, strings.SplitN(path, "?", 2)[0], response.StatusCode)
	}
	return response, nil
}

func (docker *dockerAPI) decode(ctx context.Context, method, path string, body any, target any) error {
	response, err := docker.request(ctx, method, path, body)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	return json.NewDecoder(io.LimitReader(response.Body, dockerResponseByteLimit)).Decode(target)
}

// listContainers — 멈춘 것까지 모든 컨테이너(all=1).
func (docker *dockerAPI) listContainers(ctx context.Context) ([]dockerListedContainer, error) {
	var containers []dockerListedContainer
	if err := docker.decode(ctx, http.MethodGet, "/containers/json?all=1", nil, &containers); err != nil {
		return nil, err
	}
	return containers, nil
}

func (docker *dockerAPI) inspectContainer(ctx context.Context, id string) (dockerContainerInspect, error) {
	var inspect dockerContainerInspect
	err := docker.decode(ctx, http.MethodGet, "/containers/"+url.PathEscape(id)+"/json", nil, &inspect)
	return inspect, err
}

// containerStats — 한 번짜리 통계(직전 표본 없이 바로 답한다).
func (docker *dockerAPI) containerStats(ctx context.Context, id string) (dockerContainerStats, error) {
	var stats dockerContainerStats
	err := docker.decode(ctx, http.MethodGet, "/containers/"+url.PathEscape(id)+"/stats?stream=false&one-shot=true", nil, &stats)
	return stats, err
}

// errDockerNotFound — 도커가 404 로 답했다(그 컨테이너 · 실행이 없다).
var errDockerNotFound = errors.New("docker: not found")

// createExec — 컨테이너 안에 command 를 Tty 없이 실행할 실행을 만든다(아직 시작하지 않는다). 실행 ID 를 돌려준다.
func (docker *dockerAPI) createExec(ctx context.Context, containerName string, command []string) (string, error) {
	var created struct {
		ID string `json:"Id"`
	}
	createBody := map[string]any{"AttachStdout": true, "AttachStderr": true, "Tty": false, "Cmd": command}
	if err := docker.decode(ctx, http.MethodPost, "/containers/"+url.PathEscape(containerName)+"/exec", createBody, &created); err != nil {
		return "", err
	}
	if created.ID == "" {
		return "", errors.New("docker exec: empty id")
	}
	return created.ID, nil
}

// startExec — 실행을 시작하고 출력이 끝날 때까지 stdout 을 읽는다(stderr 는 버린다).
func (docker *dockerAPI) startExec(ctx context.Context, id string) (string, error) {
	response, err := docker.request(ctx, http.MethodPost, "/exec/"+url.PathEscape(id)+"/start", map[string]any{"Detach": false, "Tty": false})
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	return readMultiplexedStdout(response.Body, dockerExecOutputLimit)
}

// inspectExec — 실행의 상태. 끝났으면 running=false 와 종료 코드. 실행이 없으면 errDockerNotFound.
func (docker *dockerAPI) inspectExec(ctx context.Context, id string) (running bool, exitCode int, err error) {
	var inspect struct {
		Running  bool `json:"Running"`
		ExitCode *int `json:"ExitCode"`
	}
	if err := docker.decode(ctx, http.MethodGet, "/exec/"+url.PathEscape(id)+"/json", nil, &inspect); err != nil {
		return false, 0, err
	}
	if inspect.Running {
		return true, 0, nil
	}
	if inspect.ExitCode == nil {
		return false, 0, errors.New("docker exec: finished without exit code")
	}
	return false, *inspect.ExitCode, nil
}

// readMultiplexedStdout — 다중화된 흐름(조각마다 8바이트 머리 [흐름, 0, 0, 0, 길이(빅엔디언 4바이트)] + 내용)에서 stdout(1)만 잇는다.
func readMultiplexedStdout(reader io.Reader, limit int) (string, error) {
	var stdout bytes.Buffer
	header := make([]byte, 8)
	for {
		if _, err := io.ReadFull(reader, header); err != nil {
			if errors.Is(err, io.EOF) {
				return stdout.String(), nil
			}
			return "", err
		}
		length := int64(binary.BigEndian.Uint32(header[4:]))
		if header[0] != 1 {
			if _, err := io.CopyN(io.Discard, reader, length); err != nil {
				return "", err
			}
			continue
		}
		if int64(stdout.Len())+length > int64(limit) {
			return "", errors.New("docker exec: output too large")
		}
		if _, err := io.CopyN(&stdout, reader, length); err != nil {
			return "", err
		}
	}
}
