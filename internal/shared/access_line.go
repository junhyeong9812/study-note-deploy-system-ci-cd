package shared

// 접속 줄 — 마스터 · 에이전트가 받은 요청마다 표준 출력에 쓰는 통신 규약 v1 의 한 줄(docs 통신 규약 3.1 · 3.4).
// nginx 의 줄 14칸에 loopback 을 더한 15칸. 뼈대: 이름과 형만 정해 두었다 — 동작은 구현 조각이 채운다.

import "net/http"

// CallerFor — 접속 줄의 caller(규약 2.2 · 3.3 의 $protocol_caller 와 같은 판정). 연결 주소(RemoteAddr)가 사설 대역 · 루프백이고
// X-Caller-Service 가 하나뿐이며 닫힌 목록의 이름이면 그 이름, 아니면 outside. ci-cd 앱에는 엣지 판정이 없다(늘 0).
func CallerFor(request *http.Request) string {
	return ""
}

// WriteAccessLines — 받은 요청마다 응답을 마친 뒤 표준 출력에 접속 줄 한 줄을 쓰는 감싸개. TrackRequests 의 안쪽에 씌운다
// (그래야 request_id 가 TrackRequests 가 정한 추적 ID 와 같다). service 는 ci-master 또는 ci-agent.
func WriteAccessLines(service string, next http.Handler) http.Handler {
	return next
}
