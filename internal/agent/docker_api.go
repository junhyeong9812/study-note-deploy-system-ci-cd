package agent

// 도커 엔진 API — 유닉스 소켓 위의 HTTP 를 직접 부른다(새 의존성 없음). 자원 지표 수집만 쓴다.
// 허용된 요청: GET 조회(/containers/json · /containers/{id}/json · /containers/{id}/stats?stream=false&one-shot=true ·
// /exec/{id}/json) + GPU 값을 읽는 고정 명령 실행 하나(POST /containers/{설정된 이름}/exec · POST /exec/{id}/start).
// 모든 요청은 부르는 쪽의 ctx(기한)를 그대로 쓴다.

import "net/http"

const dockerSocketPath = "/var/run/docker.sock"

// dockerAPI — 테스트는 baseURL·client 를 가짜 HTTP 서버로 바꾼다.
type dockerAPI struct {
	baseURL string       // 소켓이면 "http://docker" — 경로는 이 뒤에 붙는다(API 판 접두사 없이)
	client  *http.Client // 소켓이면 유닉스 소켓으로 잇는 Transport
}

// newDockerAPI — socketPath 의 유닉스 소켓으로 잇는 클라이언트.
func newDockerAPI(socketPath string) *dockerAPI {
	return &dockerAPI{}
}
