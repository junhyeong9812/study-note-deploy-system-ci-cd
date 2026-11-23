package shared

// Redis 스트림에 항목 하나를 넣는다(XADD). 로그(logger.go)와 같은 Redis 를 쓰지만, 이쪽은 응답을 확인해 실패를 돌려준다 —
// 넣지 못한 횟수를 부르는 쪽이 센다(자원 지표 — requirement-spec.md 2026-11-24 resource-metrics §2.3 · §10.2).
// 외부 redis 클라이언트 없이 RESP 를 직접 쓴다.

import (
	"context"
	"errors"
)

// StreamField — 스트림 항목의 칸 하나. 순서대로 XADD 에 실린다.
type StreamField struct {
	Name  string
	Value string
}

// StreamWriter — 스트림에 항목 하나를 넣는다. ctx 의 기한이 연결·쓰기·응답 읽기 전체에 걸린다.
// maxLength 는 근사 상한이다: XADD <stream> MAXLEN ~ <maxLength> * <칸들…>.
// Redis 가 오류 응답(-ERR …)을 주거나 닿지 못하거나 기한이 지나면 오류를 돌려준다.
type StreamWriter interface {
	Append(ctx context.Context, stream string, maxLength int, fields []StreamField) error
}

// RedisStream — StreamWriter 의 Redis 구현. 항목마다 연결한다(30초에 한 번 — 연결을 붙잡아 두지 않는다).
type RedisStream struct {
	address string // host:port
}

func NewRedisStream(address string) *RedisStream {
	return &RedisStream{address: address}
}

func (redis *RedisStream) Append(ctx context.Context, stream string, maxLength int, fields []StreamField) error {
	return errors.New("not implemented")
}

// RedisAddress — LOG_REDIS_URL 꼴(redis://host:port)에서 host:port. 비었거나 해석할 수 없으면 "".
func RedisAddress(rawURL string) string {
	return ""
}
