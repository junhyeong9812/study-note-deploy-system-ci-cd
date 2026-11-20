package shared

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestAlertSenderPostsEventWithSecret(t *testing.T) {
	received := make(chan map[string]string, 1)
	var secret string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		secret = request.Header.Get("X-Alert-Secret")
		body, _ := io.ReadAll(request.Body)
		var event map[string]string
		json.Unmarshal(body, &event)
		received <- event
		writer.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()
	t.Setenv("ALERT_EVENTS_URL", server.URL)
	t.Setenv("ALERT_SECRET", "test-value")
	NewAlertSender("ci-cd", NewLogger()).Send("gh-1-1", "deploy_failed", "warning", "deploy-failed:llm:abc", "배포 실패 llm", strings.Repeat("x", 3000))
	select {
	case event := <-received:
		if secret != "test-value" || event["source"] != "ci-cd" || event["kind"] != "deploy_failed" || event["key"] != "deploy-failed:llm:abc" {
			t.Fatalf("사건 내용이 다르다: secret=%q %v", secret, event)
		}
		if len(event["detail"]) != 1200 {
			t.Fatalf("긴 출력은 끝부분 1200자만 보내야 한다: %d", len(event["detail"]))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("사건이 도착하지 않았다")
	}
}

func TestAlertSenderDoesNothingWhenNotConfigured(t *testing.T) {
	t.Setenv("ALERT_EVENTS_URL", "")
	NewAlertSender("ci-cd", NewLogger()).Send("gh-1-1", "deploy_failed", "warning", "k", "t", "d") // 예외·대기 없이 끝나야 한다
	var missing *AlertSender
	missing.Send("gh-1-1", "deploy_failed", "warning", "k", "t", "d")
}

func TestAlertSenderFailureDoesNotBlockCaller(t *testing.T) {
	stalled := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { time.Sleep(10 * time.Second) }))
	defer stalled.CloseClientConnections()
	t.Setenv("ALERT_EVENTS_URL", stalled.URL)
	started := time.Now()
	NewAlertSender("ci-cd", NewLogger()).Send("gh-1-1", "deploy_failed", "warning", "k", "t", "d")
	if time.Since(started) > 200*time.Millisecond {
		t.Fatalf("보내는 일이 호출한 쪽을 붙잡았다: %v", time.Since(started))
	}
}
