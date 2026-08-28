package shared

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"sync"
	"time"
)

// 로그 규약 — `requestId:server-name:message` (docs/logging.md 정본).
// stdout 항상 + Redis Stream XADD(실패 무해: 짧은 타임아웃·30s 백오프·에러 전량 흡수).
// 외부 redis 클라이언트 없이 RESP를 직접 쓴다 — XADD 한 명령이면 충분해서.

type Logger struct {
	serverName    string
	redisAddress  string // host:port ("" 이면 stdout만)
	stream        string
	mutex         sync.Mutex
	disabledUntil time.Time
}

func NewLogger() *Logger {
	serverName := os.Getenv("SERVER_NAME")
	if serverName == "" {
		serverName = "ci-" + os.Getenv("MODE")
	}
	redisAddress := ""
	if rawURL := os.Getenv("LOG_REDIS_URL"); rawURL != "" {
		if parsed, err := url.Parse(rawURL); err == nil && parsed.Host != "" {
			redisAddress = parsed.Host
		}
	}
	stream := os.Getenv("LOG_STREAM")
	if stream == "" {
		stream = "logs"
	}
	return &Logger{serverName: serverName, redisAddress: redisAddress, stream: stream}
}

func (logger *Logger) FormatLine(requestID, message string) string {
	return fmt.Sprintf("%s:%s:%s", requestID, logger.serverName, message)
}

func (logger *Logger) Log(requestID, message, level string) {
	line := logger.FormatLine(requestID, message)
	fmt.Printf("%s %s %s\n", time.Now().UTC().Format(time.RFC3339), level, line)
	logger.sendToRedis(level, line)
}

func (logger *Logger) sendToRedis(level, line string) {
	if logger.redisAddress == "" {
		return
	}
	logger.mutex.Lock()
	defer logger.mutex.Unlock()
	if time.Now().Before(logger.disabledUntil) {
		return
	}
	connection, err := net.DialTimeout("tcp", logger.redisAddress, 300*time.Millisecond)
	if err != nil {
		logger.disabledUntil = time.Now().Add(30 * time.Second) // 백오프 — 로그가 요청을 인질로 잡지 않는다
		return
	}
	defer connection.Close()
	_ = connection.SetDeadline(time.Now().Add(300 * time.Millisecond))
	// RESP: XADD <stream> MAXLEN ~ 10000 * level <level> line <line>
	command := respArray("XADD", logger.stream, "MAXLEN", "~", "10000", "*", "level", level, "line", line)
	if _, err := connection.Write([]byte(command)); err != nil {
		logger.disabledUntil = time.Now().Add(30 * time.Second)
		return
	}
	buffer := make([]byte, 128)
	_, _ = connection.Read(buffer) // 응답은 확인만 (실패해도 무해)
}

func respArray(parts ...string) string {
	result := fmt.Sprintf("*%d\r\n", len(parts))
	for _, part := range parts {
		result += fmt.Sprintf("$%d\r\n%s\r\n", len(part), part)
	}
	return result
}

func NewRequestID() string {
	return fmt.Sprintf("req-%x", time.Now().UnixNano())
}

// AcceptOrIssue — 인입 requestId 검증(규약 포맷을 깨는 값이면 재발행)
func AcceptOrIssue(incoming string) string {
	if incoming == "" || len(incoming) > 64 {
		return NewRequestID()
	}
	for _, character := range incoming {
		valid := character == '-' || character == '_' ||
			(character >= '0' && character <= '9') ||
			(character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z')
		if !valid {
			return NewRequestID()
		}
	}
	return incoming
}
