package shared

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"time"
)

// AlertSender — 알림 서버에 "사람이 알아야 할 일"을 보낸다(배포 실패, 두 자리가 남았을 수 있는 상황).
// 보내는 일은 배포와 분리한다: 따로 돌고, 실패해도 로그만 남기며 배포 결과를 바꾸지 않는다.
type AlertSender struct {
	url    string // env ALERT_EVENTS_URL — 비어 있으면 보내지 않는다
	secret string // env ALERT_SECRET
	source string
	logger *Logger
	client *http.Client
	// callerService — 보낼 때 X-Caller-Service 로 싣는 이름(WithCallerService 로 정한다 — 비어 있으면 싣지 않는다)
	callerService string
}

func NewAlertSender(source string, logger *Logger) *AlertSender {
	return &AlertSender{url: os.Getenv("ALERT_EVENTS_URL"), secret: os.Getenv("ALERT_SECRET"),
		source: source, logger: logger, client: &http.Client{Timeout: 3 * time.Second}}
}

// WithCallerService — 이 보내기가 X-Caller-Service 로 실을 부르는 쪽의 규약 이름을 정한다(만든 패키지가 자기 상수를 준다). 같은 보내기를 돌려준다.
func (sender *AlertSender) WithCallerService(callerService string) *AlertSender {
	sender.callerService = callerService
	return sender
}

// Send — severity: info·warning·critical. key 가 같으면 알림 서버가 10분에 한 번만 발송한다.
func (sender *AlertSender) Send(requestID, kind, severity, key, title, detail string) {
	if sender == nil || sender.url == "" {
		return
	}
	payload, _ := json.Marshal(map[string]string{
		"source": sender.source, "kind": kind, "severity": severity, "key": key, "title": title, "detail": tailOf(detail, 1200),
	})
	go func() {
		request, err := http.NewRequest(http.MethodPost, sender.url, bytes.NewReader(payload))
		if err != nil {
			sender.logger.Log(requestID, "alert not sent: "+err.Error(), "warning")
			return
		}
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("X-Alert-Secret", sender.secret)
		request.Header.Set(RequestIDHeader, NewTrackingID()) // 들어온 요청 없이 시작되는 호출 — 보낼 때마다 새 추적 ID(로그의 ID 는 부른 쪽의 것 그대로)
		if sender.callerService != "" {
			request.Header.Set(CallerServiceHeader, sender.callerService)
		}
		response, err := sender.client.Do(request)
		if err != nil {
			sender.logger.Log(requestID, "alert not sent: "+err.Error(), "warning")
			return
		}
		response.Body.Close()
		if response.StatusCode >= 300 {
			sender.logger.Log(requestID, "alert not sent: status "+response.Status, "warning")
		}
	}()
}

func tailOf(input string, limit int) string {
	if len(input) <= limit {
		return input
	}
	return input[len(input)-limit:]
}
