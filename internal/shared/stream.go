package shared

// Redis 스트림에 항목 하나를 넣는다(XADD). 로그(logger.go)와 같은 Redis 를 쓰지만, 이쪽은 응답을 확인해 실패를 돌려준다 —
// 넣지 못한 횟수를 부르는 쪽이 센다(자원 지표 — requirement-spec.md 2026-11-24 resource-metrics §2.3 · §10.2).
// 외부 redis 클라이언트 없이 RESP 를 직접 쓴다.

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
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
	var dialer net.Dialer
	connection, err := dialer.DialContext(ctx, "tcp", redis.address)
	if err != nil {
		return fmt.Errorf("redis connect: %w", err)
	}
	defer connection.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = connection.SetDeadline(deadline)
	}
	stop := context.AfterFunc(ctx, func() { connection.Close() }) // 기한 없이 취소만 되는 ctx 도 쓰기·읽기를 푼다
	defer stop()

	parts := []string{"XADD", stream, "MAXLEN", "~", strconv.Itoa(maxLength), "*"}
	for _, field := range fields {
		parts = append(parts, field.Name, field.Value)
	}
	if _, err := connection.Write([]byte(respArray(parts...))); err != nil {
		return fmt.Errorf("redis write: %w", err)
	}
	reply, err := bufio.NewReader(connection).ReadString('\n')
	if err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("redis reply: %w", ctx.Err())
		}
		return fmt.Errorf("redis reply: %w", err)
	}
	if strings.HasPrefix(reply, "-") {
		return errors.New("redis error reply: " + strings.TrimSpace(reply[1:]))
	}
	return nil
}

// RedisAddress — LOG_REDIS_URL 꼴(redis://host:port)에서 host:port. 비었거나 해석할 수 없으면 "".
func RedisAddress(rawURL string) string {
	if rawURL == "" {
		return ""
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return parsed.Host
}
