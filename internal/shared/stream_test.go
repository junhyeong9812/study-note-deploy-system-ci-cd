package shared

// Redis 스트림 전송 — 기준: requirement-spec.md(2026-11-24 resource-metrics) §2.3 · §10.2 · §11.1.
// 정한 세부: 항목마다 연결하고 응답을 읽은 뒤 닫는다. 오류 응답(-…) · 연결 실패 · 기한 초과는 오류다. ctx 의 기한이 연결 · 쓰기 · 읽기 전체에 걸린다.

import (
	"bufio"
	"context"
	"io"
	"net"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeRedis struct {
	listener net.Listener
	reply    string // 응답 그대로 — "" 면 답하지 않고 연결이 닫힐 때까지 붙잡는다
	mutex    sync.Mutex
	commands [][]string
	closed   chan time.Duration // 명령을 받은 뒤 부른 쪽이 연결을 닫기까지
}

func newFakeRedis(t *testing.T, reply string) *fakeRedis {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	redis := &fakeRedis{listener: listener, reply: reply, closed: make(chan time.Duration, 8)}
	go func() {
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			go redis.serve(connection)
		}
	}()
	t.Cleanup(func() { listener.Close() })
	return redis
}

func (redis *fakeRedis) serve(connection net.Conn) {
	defer connection.Close()
	_ = connection.SetDeadline(time.Now().Add(5 * time.Second))
	reader := bufio.NewReader(connection)
	command, err := readCommand(reader)
	if err != nil {
		return
	}
	redis.mutex.Lock()
	redis.commands = append(redis.commands, command)
	redis.mutex.Unlock()
	receivedAt := time.Now()
	if redis.reply != "" {
		_, _ = connection.Write([]byte(redis.reply))
	}
	if _, err := reader.ReadByte(); err == io.EOF { // 부른 쪽이 닫을 때까지
		redis.closed <- time.Since(receivedAt)
	}
}

// readCommand — RESP 배열(*N\r\n 뒤에 $len\r\n내용\r\n 이 N 개). 길이는 바이트다.
func readCommand(reader *bufio.Reader) ([]string, error) {
	header, err := reader.ReadString('\n')
	if err != nil || !strings.HasPrefix(header, "*") {
		return nil, io.ErrUnexpectedEOF
	}
	count, err := strconv.Atoi(strings.TrimSuffix(header[1:], "\r\n"))
	if err != nil {
		return nil, err
	}
	parts := make([]string, 0, count)
	for range count {
		lengthLine, err := reader.ReadString('\n')
		if err != nil || !strings.HasPrefix(lengthLine, "$") {
			return nil, io.ErrUnexpectedEOF
		}
		length, err := strconv.Atoi(strings.TrimSuffix(lengthLine[1:], "\r\n"))
		if err != nil {
			return nil, err
		}
		content := make([]byte, length+2)
		if _, err := io.ReadFull(reader, content); err != nil || string(content[length:]) != "\r\n" {
			return nil, io.ErrUnexpectedEOF
		}
		parts = append(parts, string(content[:length]))
	}
	return parts, nil
}

func (redis *fakeRedis) received() [][]string {
	redis.mutex.Lock()
	defer redis.mutex.Unlock()
	return append([][]string(nil), redis.commands...)
}

func TestRedisStreamAppendsXaddWithApproximateMaxLength(t *testing.T) {
	redis := newFakeRedis(t, "$15\r\n1763942400000-0\r\n")
	body := "{\"name\":\"한글 \\r\\n\",\"raw\":\"줄\r\n바꿈\"}" // 칸 값은 바이트 길이로 실린다(줄바꿈 · 여러 바이트 글자)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := NewRedisStream(redis.listener.Addr().String()).Append(ctx, "metrics", 20000,
		[]StreamField{{"kind", "resource"}, {"v", "1"}, {"server", "home-9"}, {"at", "1763942400000"}, {"body", body}})
	if err != nil {
		t.Fatalf("정상 응답인데 오류: %v", err)
	}
	commands := redis.received()
	want := []string{"XADD", "metrics", "MAXLEN", "~", "20000", "*", "kind", "resource", "v", "1", "server", "home-9", "at", "1763942400000", "body", body}
	if len(commands) != 1 || !reflect.DeepEqual(commands[0], want) {
		t.Fatalf("XADD 명령이 다르다:\n got %q\nwant %q", commands, want)
	}
	select {
	case <-redis.closed:
	case <-time.After(time.Second):
		t.Fatal("항목을 넣은 뒤 연결을 닫지 않았다")
	}
}

func TestRedisStreamReportsErrorReply(t *testing.T) {
	redis := newFakeRedis(t, "-WRONGTYPE Operation against a key holding the wrong kind of value\r\n")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := NewRedisStream(redis.listener.Addr().String()).Append(ctx, "metrics", 20000, []StreamField{{"kind", "resource"}}); err == nil {
		t.Fatal("오류 응답을 성공으로 셌다")
	}
	if commands := redis.received(); len(commands) != 1 || commands[0][0] != "XADD" {
		t.Fatalf("XADD 를 보내고 그 응답으로 판정해야 한다: %q", commands)
	}
}

func TestRedisStreamHonorsContextDeadline(t *testing.T) {
	redis := newFakeRedis(t, "") // 받고 답하지 않는다
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	startedAt := time.Now()
	err := NewRedisStream(redis.listener.Addr().String()).Append(ctx, "metrics", 20000, []StreamField{{"kind", "resource"}})
	if err == nil {
		t.Fatal("답이 없는데 성공으로 셌다")
	}
	if elapsed := time.Since(startedAt); elapsed > time.Second {
		t.Fatalf("ctx 의 기한(200ms)을 넘겨 기다렸다: %v", elapsed)
	}
	if commands := redis.received(); len(commands) != 1 {
		t.Fatalf("명령을 보낸 뒤 응답을 기다리다 기한에 끊겨야 한다: %q", commands)
	}
}

func TestRedisStreamReportsUnreachableServer(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	listener.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	startedAt := time.Now()
	if err := NewRedisStream(address).Append(ctx, "metrics", 20000, []StreamField{{"kind", "resource"}}); err == nil {
		t.Fatal("닿지 못했는데 성공으로 셌다")
	}
	if elapsed := time.Since(startedAt); elapsed > time.Second {
		t.Fatalf("닫힌 포트에 너무 오래 매달렸다: %v", elapsed)
	}
}

func TestRedisAddressFromURL(t *testing.T) {
	cases := map[string]string{
		"redis://10.0.0.5:6379":        "10.0.0.5:6379",
		"redis://:secret@cache:6380/0": "cache:6380",
		"":                             "",
		"redis://":                     "",
		"::not a url":                  "",
	}
	for raw, want := range cases {
		if got := RedisAddress(raw); got != want {
			t.Fatalf("RedisAddress(%q) = %q, want %q", raw, got, want)
		}
	}
}
