package shared

// 접속 줄 — 마스터 · 에이전트가 받은 요청마다 표준 출력에 쓰는 통신 규약 v1 의 한 줄(docs 통신 규약 3.1 · 3.4).
// nginx 의 줄 14칸에 loopback 을 더한 15칸. 주소 · 쿼리 · 리퍼러 · User-Agent · 쿠키 · 인증 · 호스트는 싣지 않는다(3.5).
// 로그(Logger)를 타지 않는다: 앞에 시각이 붙지 않고 · Redis 로 가지 않고 · 로그의 잠금을 기다리지 않는다.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/netip"
	"os"
	"time"
)

// accessLinePathLimit — path 칸의 상한(바이트 — nginx 의 정규식 map 이 바이트로 센다, 3.2).
const accessLinePathLimit = 1024

// protocolCallers — 머리글로 받는 호출자의 닫힌 목록(규약 2.2 · 3.3 의 map). edge · outside 는 머리글로 받지 않는다.
var protocolCallers = map[string]bool{
	"front": true, "backend": true, "llm-wrapper": true, "alert": true, "observe": true, "admin": true, "ci-master": true, "ci-agent": true,
}

var loopbackNetworks = []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8"), netip.MustParsePrefix("::1/128")}

// CallerFor — 접속 줄의 caller(규약 2.2 · 3.3 의 $protocol_caller 와 같은 판정). 연결 주소(RemoteAddr)가 사설 대역 · 루프백이고
// X-Caller-Service 가 하나뿐이며 닫힌 목록의 이름이면 그 이름, 아니면 outside. ci-cd 앱에는 엣지 판정이 없다(늘 0).
// nginx 의 map 처럼 대소문자를 가리지 않고(ASCII 만 — 다른 글자의 대소문자 접기로 목록의 이름이 되지 않게) 소문자로 적는다.
func CallerFor(request *http.Request) string {
	values := request.Header.Values(CallerServiceHeader)
	if len(values) != 1 || !FromPrivateNetwork(request.RemoteAddr) {
		return "outside"
	}
	if name := string(asciiLower([]byte(values[0]))); protocolCallers[name] {
		return name
	}
	return "outside"
}

// fromLoopback — 연결 주소가 루프백(127.0.0.0/8 · ::1)인가. IPv4 를 담은 IPv6 는 IPv4 로 본다. 판정할 수 없으면 아니다.
func fromLoopback(remoteAddress string) bool {
	address, ok := connectionAddress(remoteAddress)
	if !ok {
		return false
	}
	for _, network := range loopbackNetworks {
		if network.Contains(address) {
			return true
		}
	}
	return false
}

// accessLineFields — 칸의 이름 · 차례 · 형(3.1 · 3.4). ts · request_time 은 소수 셋째 자리의 수라 글자로 만들어 수로 싣는다.
type accessLineFields struct {
	Version        int         `json:"v"`
	Timestamp      json.Number `json:"ts"`
	RequestID      string      `json:"request_id"`
	Caller         string      `json:"caller"`
	Service        string      `json:"service"`
	Method         string      `json:"method"`
	Path           string      `json:"path"`
	PathTruncated  bool        `json:"path_truncated"`
	Status         int         `json:"status"`
	RequestTime    json.Number `json:"request_time"`
	UpstreamTime   string      `json:"upstream_time"`
	UpstreamStatus string      `json:"upstream_status"`
	Slot           string      `json:"slot"`
	BytesSent      int64       `json:"bytes_sent"`
	Loopback       bool        `json:"loopback"`
}

// accessLineRecorder — 핸들러가 낸 상태(1xx 를 뺀 첫 상태)와 넘긴 본문 바이트를 센다. 응답에는 손대지 않는다.
type accessLineRecorder struct {
	http.ResponseWriter
	status    int
	bytesSent int64
}

func (recorder *accessLineRecorder) WriteHeader(status int) {
	if recorder.status == 0 && (status >= 200 || status == http.StatusSwitchingProtocols) {
		recorder.status = status
	}
	recorder.ResponseWriter.WriteHeader(status)
}

func (recorder *accessLineRecorder) Write(content []byte) (int, error) {
	if recorder.status == 0 {
		recorder.status = http.StatusOK
	}
	written, err := recorder.ResponseWriter.Write(content)
	recorder.bytesSent += int64(written)
	return written, err
}

// Unwrap — http.ResponseController 가 안의 writer 를 찾게 한다.
func (recorder *accessLineRecorder) Unwrap() http.ResponseWriter { return recorder.ResponseWriter }

func threeDecimalSeconds(milliseconds int64) json.Number {
	return json.Number(fmt.Sprintf("%d.%03d", milliseconds/1000, milliseconds%1000))
}

// WriteAccessLines — 받은 요청마다 응답을 마친 뒤 표준 출력에 접속 줄 한 줄을 쓰는 감싸개. TrackRequests 의 안쪽에 씌운다
// (그래야 request_id 가 TrackRequests 가 정한 추적 ID 와 같다). service 는 ci-master 또는 ci-agent.
// 핸들러의 panic 은 삼키지 않고 그 요청의 줄은 쓰지 않는다. 줄을 쓰지 못한 오류는 버린다(응답은 이미 끝났다).
func WriteAccessLines(service string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		started := time.Now()
		recorder := &accessLineRecorder{ResponseWriter: writer}
		next.ServeHTTP(recorder, request)
		finished := time.Now()
		path, truncated := request.URL.EscapedPath(), false
		if len(path) > accessLinePathLimit { // EscapedPath 는 ASCII 라 바이트로 잘라도 글자가 깨지지 않는다
			path, truncated = path[:accessLinePathLimit], true
		}
		status := recorder.status
		if status == 0 {
			status = http.StatusOK
		}
		line := accessLineFields{Version: 1, Timestamp: threeDecimalSeconds(finished.UnixMilli()), RequestID: TrackingID(request.Context()),
			Caller: CallerFor(request), Service: service, Method: request.Method, Path: path, PathTruncated: truncated, Status: status,
			RequestTime: threeDecimalSeconds(finished.Sub(started).Milliseconds()), BytesSent: recorder.bytesSent, Loopback: fromLoopback(request.RemoteAddr)}
		var buffer bytes.Buffer
		encoder := json.NewEncoder(&buffer)
		encoder.SetEscapeHTML(false) // & 를 & 으로 늘리지 않는다 — 줄을 짧게(한 번의 쓰기가 파이프에서 끊기지 않게)
		if encoder.Encode(line) != nil {
			return
		}
		_, _ = os.Stdout.Write(buffer.Bytes()) // 한 번의 쓰기 · 쓸 때마다 os.Stdout 을 읽는다 · 오류는 버린다
	})
}
