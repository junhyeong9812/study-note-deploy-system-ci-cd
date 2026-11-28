package shared

// 접속 줄의 method 칸(통신 규약 3.4 의 method 줄 · 3.1 — T4c 리뷰 루프 1 의 R1).
//   - 메서드가 ^[A-Z_-]{1,32}$ 일 때만 그대로 적는다(읽는 쪽 · 관측 서버가 받는 꼴과 같다).
//   - 그 밖(소문자 · 숫자가 든 토큰 · 33자 이상 · 그 밖의 토큰 글자)은 "" — 줄은 쓴다(한 줄 · 15칸 · 상태 · 경로 · request_id 는 그대로).

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestAccessLineMethodShape(t *testing.T) {
	kept := []string{"GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS", "M-SEARCH", "MY_METHOD", strings.Repeat("A", 32)}
	blanked := []string{"get", "Get", "M3TH0D", "GET1", strings.Repeat("A", 33), strings.Repeat("M", 36), "GET!", "GE.T", "G~T"}
	for _, method := range append(append([]string{}, kept...), blanked...) {
		want := ""
		for _, keep := range kept {
			if method == keep {
				want = method
			}
		}
		request := accessRequest(method, "/odd?q=x", "10.0.0.1:40000", map[string][]string{"X-Request-Id": {"trace-front-0901"}}, "")
		recorder, captured, escaped := serveWithAccessLines(t, "ci-master", accessTestMux(&sync.Map{}), request)
		if escaped != nil {
			t.Fatalf("%q: panic: %v", method, escaped)
		}
		line := onlyAccessLine(t, method, captured)
		if strings.Join(line.order, ",") != strings.Join(accessLineFieldOrder, ",") {
			t.Fatalf("%q: 15칸 그대로여야 한다: %v", method, line.order)
		}
		if line.textValue("method") != want {
			t.Errorf("%q: method 칸 %q (기대 %q)", method, line.textValue("method"), want)
		}
		if line.raw("status") != fmt.Sprint(recorder.Code) || line.textValue("path") != "/odd" || line.textValue("request_id") != "trace-front-0901" {
			t.Errorf("%q: 그 밖의 칸이 바뀌었다: %s", method, line.text)
		}
	}
}

// 실제 연결(날 소켓): Go 서버는 아무 토큰이나 메서드로 받아 핸들러까지 넘긴다 — 그 줄의 method 는 "" 이고 줄은 쓴다.
func TestAccessLineMethodOverRawSocket(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /x", func(writer http.ResponseWriter, _ *http.Request) { io.WriteString(writer, "x") })
	var status int
	captured := captureSharedLog(t, func() {
		server := httptest.NewServer(TrackRequests(NewLogger(), WriteAccessLines("ci-master", mux)))
		defer server.Close()
		connection, err := net.DialTimeout("tcp", server.Listener.Addr().String(), 2*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		defer connection.Close()
		_ = connection.SetDeadline(time.Now().Add(3 * time.Second))
		io.WriteString(connection, "get /x HTTP/1.1\r\nHost: tracked.test\r\nConnection: close\r\n\r\n")
		response, err := http.ReadResponse(bufio.NewReader(connection), nil)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, response.Body)
		response.Body.Close()
		status = response.StatusCode
		time.Sleep(100 * time.Millisecond) // 감싸개가 줄을 쓰도록
	})
	line := onlyAccessLine(t, "get /x", captured)
	if line.textValue("method") != "" || line.raw("status") != fmt.Sprint(status) || line.textValue("path") != "/x" || line.raw("loopback") != "true" {
		t.Fatalf("소문자 메서드의 줄: method 는 \"\" · 상태 %d · 경로 /x: %s", status, line.text)
	}
}
