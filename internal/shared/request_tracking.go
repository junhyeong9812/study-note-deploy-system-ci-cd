package shared

// 추적의 ID — 요청이 거친 길을 잇는 값(docs 통신 규약 1.2 · 1.3). 일의 ID(본문 request_id — 멱등 열쇠)와 섞지 않는다.
// ci-cd 의 마스터 · 에이전트는 nginx 없이 요청을 처음 받는 쪽이라 받은 ID 의 판정을 앱이 직접 한다.

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"time"
)

// RequestIDHeader — 추적의 ID 를 싣는 머리글(받기 · 다음 호출 · 응답).
const RequestIDHeader = "X-Request-Id"

// privateNetworks — nginx 의 geo $from_private 와 같은 대역(사설 · 루프백 · 링크 로컬 IPv6). 169.254/16 · 100.64/10 은 넣지 않는다.
var privateNetworks = []netip.Prefix{
	netip.MustParsePrefix("127.0.0.0/8"), netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.168.0.0/16"), netip.MustParsePrefix("::1/128"), netip.MustParsePrefix("fc00::/7"), netip.MustParsePrefix("fe80::/10"),
}

// NewTrackingID — 16진수 소문자 32자의 난수(nginx 의 $request_id 와 같은 꼴). 부를 때마다 새 값.
func NewTrackingID() string {
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		// crypto/rand 는 리눅스에서 실패하지 않는다 — 실패하면 시각으로라도 겹치지 않게 한다
		binary.BigEndian.PutUint64(random, uint64(time.Now().UnixNano()))
	}
	return hex.EncodeToString(random)
}

// FromPrivateNetwork — 연결 주소(http.Request.RemoteAddr 꼴, "host:port")가 사설 대역 · 루프백인가.
// 대역은 nginx 의 geo $from_private 와 같다. IPv4 를 담은 IPv6(::ffff:a.b.c.d)는 IPv4 로 본다. 영역(%eth0)은 뗀다. 판정할 수 없으면 밖이다.
func FromPrivateNetwork(remoteAddress string) bool {
	host, _, err := net.SplitHostPort(remoteAddress)
	if err != nil {
		return false
	}
	address, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	address = address.WithZone("").Unmap()
	for _, network := range privateNetworks {
		if network.Contains(address) {
			return true
		}
	}
	return false
}

// TrackingIDFor — 그 요청의 추적 ID 를 정한다: 연결 주소가 사설 대역 · 루프백이고 X-Request-Id 가 하나뿐이며
// 꼴(ValidRequestID)에 맞으면 그 값, 아니면 NewTrackingID(). X-Forwarded-For 같은 머리글의 주소는 보지 않는다.
func TrackingIDFor(request *http.Request) string {
	values := request.Header.Values(RequestIDHeader)
	if len(values) == 1 && ValidRequestID(values[0]) && FromPrivateNetwork(request.RemoteAddr) {
		return values[0]
	}
	return NewTrackingID()
}

type trackingIDKey struct{}

// TrackRequests — mux 전체에 씌우는 감싸개: 추적 ID 를 정해 요청의 context 에 두고, 응답 머리글 X-Request-Id 로 돌려준다
// (성공 · 4xx · 5xx · 경로 없음 모두 — 머리글은 핸들러 전에 쓴다). 핸들러의 panic 은 막지 않는다(지금처럼 net/http 까지 올라간다).
// logger 는 지금 쓰지 않는다(감싸개는 로그를 남기지 않는다) — 부르는 쪽의 꼴을 정해 둔 인자다.
func TrackRequests(logger *Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		trackingID := TrackingIDFor(request)
		writer.Header().Set(RequestIDHeader, trackingID)
		next.ServeHTTP(writer, request.WithContext(context.WithValue(request.Context(), trackingIDKey{}, trackingID)))
	})
}

// TrackingID — TrackRequests 가 정해 둔 그 요청의 추적 ID. 감싸개를 거치지 않은 요청이면 새 값.
func TrackingID(ctx context.Context) string {
	if trackingID, ok := ctx.Value(trackingIDKey{}).(string); ok {
		return trackingID
	}
	return NewTrackingID()
}

// JobForLog — 잇는 줄의 job= 에 싣는 일의 ID: 꼴(ValidRequestID)에 맞을 때만 그 값, 아니면 invalid(본문 · 경로의 날 값을 싣지 않는다).
func JobForLog(job string) string {
	if ValidRequestID(job) {
		return job
	}
	return "invalid"
}

// StatusRecorder — 핸들러가 낸 상태를 기억한다(잇는 줄 "-> <상태>"). 아무것도 쓰지 않으면 200.
type StatusRecorder struct {
	http.ResponseWriter
	status int
}

func RecordStatus(writer http.ResponseWriter) *StatusRecorder {
	return &StatusRecorder{ResponseWriter: writer}
}

func (recorder *StatusRecorder) WriteHeader(status int) {
	if recorder.status == 0 {
		recorder.status = status
	}
	recorder.ResponseWriter.WriteHeader(status)
}

func (recorder *StatusRecorder) Write(content []byte) (int, error) {
	if recorder.status == 0 {
		recorder.status = http.StatusOK
	}
	return recorder.ResponseWriter.Write(content)
}

// Unwrap — http.ResponseController 가 안의 writer 를 찾게 한다.
func (recorder *StatusRecorder) Unwrap() http.ResponseWriter { return recorder.ResponseWriter }

func (recorder *StatusRecorder) Status() int {
	if recorder.status == 0 {
		return http.StatusOK
	}
	return recorder.status
}

// Outcome — 잇는 줄의 "-> <상태>" 글자. 뼈대: 지금의 Status() 를 글자로 옮겼을 뿐이다(쓰지 않은 응답의 none 은 구현 조각이 채운다).
func (recorder *StatusRecorder) Outcome() string {
	return strconv.Itoa(recorder.Status())
}
