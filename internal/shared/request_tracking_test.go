package shared

// 추적 ID 의 판정 · 응답 머리글 · 감싸개(통신 규약 1.2 · 1.3 · 1.4 #10 · #11, 3.4 의 request_id 칸).
// 판정의 대역은 nginx 의 geo $from_private 와 같다:
//   127.0.0.0/8 · 10.0.0.0/8 · 172.16.0.0/12 · 192.168.0.0/16 · ::1/128 · fc00::/7 · fe80::/10
// 그리고 IPv4 를 담은 IPv6(::ffff:a.b.c.d)는 IPv4 로 본다(nginx 의 geo 도 그렇게 찾는다). 꼴은 ^[A-Za-z0-9_-]{1,64}$.
// 판정에 쓰는 주소는 연결 주소(RemoteAddr)뿐이다 — X-Forwarded-For · X-Real-IP · Forwarded 는 보지 않는다.

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

var trackingIDPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

// captureSharedLog — 그동안 표준 출력에 찍힌 로그 줄.
func captureSharedLog(t *testing.T, run func()) string {
	t.Helper()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	original := os.Stdout
	os.Stdout = writer
	captured := make(chan string)
	go func() {
		content, _ := io.ReadAll(reader)
		captured <- string(content)
	}()
	func() {
		defer func() { os.Stdout = original }()
		run()
	}()
	writer.Close()
	return <-captured
}

// serveCatching — 감싸개를 거쳐 한 번 부른다. 감싸개 밖으로 panic 이 새면 그 값을 돌려준다(테스트 바이너리를 죽이지 않게).
func serveCatching(handler http.Handler, request *http.Request) (recorder *httptest.ResponseRecorder, escaped any) {
	recorder = httptest.NewRecorder()
	func() {
		defer func() { escaped = recover() }()
		handler.ServeHTTP(recorder, request)
	}()
	return recorder, escaped
}

func trackingRequest(method, target, remoteAddress string, incoming ...string) *http.Request {
	request := httptest.NewRequest(method, target, nil)
	request.RemoteAddr = remoteAddress
	for _, value := range incoming {
		request.Header.Add(RequestIDHeader, value)
	}
	return request
}

func TestNewTrackingIDIsThirtyTwoLowercaseHexAndFreshEveryCall(t *testing.T) {
	seen := map[string]bool{}
	for index := 0; index < 2000; index++ {
		identifier := NewTrackingID()
		if !trackingIDPattern.MatchString(identifier) {
			t.Fatalf("16진수 소문자 32자가 아니다: %q", identifier)
		}
		if seen[identifier] {
			t.Fatalf("같은 값이 두 번 나왔다(%d번째): %q", index, identifier)
		}
		seen[identifier] = true
	}
}

// 일의 ID 를 만드는 NewRequestID(req-<나노초 16진수>) · AcceptOrIssue 는 그대로다 — 접수가 본문 request_id 없이 오면 이것이 일의 ID 가 된다.
func TestJobIdentifierIssuingStaysAsBefore(t *testing.T) {
	if issued := NewRequestID(); !regexp.MustCompile(`^req-[0-9a-f]+$`).MatchString(issued) {
		t.Fatalf("일의 ID 를 만드는 꼴이 바뀌었다: %q", issued)
	}
	if AcceptOrIssue("gh-77-1") != "gh-77-1" || !strings.HasPrefix(AcceptOrIssue("has:colon"), "req-") {
		t.Fatal("AcceptOrIssue(일의 ID 의 판정)가 바뀌었다")
	}
}

func TestFromPrivateNetworkMatchesNginxGeoRanges(t *testing.T) {
	inside := []string{
		"127.0.0.1:40000", "127.255.255.255:40000",
		"10.0.0.0:40000", "10.0.0.1:40000", "10.255.255.255:40000",
		"172.16.0.0:40000", "172.20.0.1:40000", "172.31.255.255:40000",
		"192.168.0.0:40000", "192.168.0.1:40000", "192.168.255.255:40000",
		"[::1]:40000",
		"[fc00::]:40000", "[fd00::1]:40000", "[fdff:ffff:ffff:ffff:ffff:ffff:ffff:ffff]:40000",
		"[fe80::]:40000", "[fe80::1]:40000", "[febf:ffff:ffff:ffff:ffff:ffff:ffff:ffff]:40000",
		"[fe80::1%eth0]:40000", // 영역이 붙은 링크 로컬 — 영역을 떼고 본다
		// IPv4 를 담은 IPv6 — IPv4 로 본다
		"[::ffff:10.0.0.1]:40000", "[::ffff:127.0.0.1]:40000", "[::ffff:172.16.0.1]:40000", "[::ffff:192.168.0.1]:40000",
	}
	outside := []string{
		// 문서용 대역(인터넷의 예)
		"192.0.2.1:40000", "198.51.100.1:40000", "203.0.113.1:40000", "[2001:db8::1]:40000",
		"[::ffff:192.0.2.1]:40000", "[::ffff:203.0.113.1]:40000",
		// 사설 대역 바로 바깥의 경계
		"11.0.0.0:40000", "128.0.0.0:40000",
		"172.15.255.255:40000", "172.32.0.0:40000", "[::ffff:172.32.0.1]:40000",
		"192.167.255.255:40000", "192.169.0.0:40000",
		"[::2]:40000", "[fbff:ffff:ffff:ffff:ffff:ffff:ffff:ffff]:40000", "[fe00::]:40000", "[fec0::]:40000",
		// nginx 의 목록에 없는 특수 대역 — 밖으로 본다
		"169.254.0.1:40000", "100.64.0.1:40000", "0.0.0.0:40000", "[::]:40000",
		// IPv4 를 담은 다른 꼴 — ::ffff: 만 IPv4 로 본다
		"[::a00:1]:40000", "[64:ff9b::a00:1]:40000", "[2002:a00:1::1]:40000",
		// 주소가 아닌 값 · 포트 없는 값 — 판정할 수 없으면 밖이다
		"", "garbage", "localhost:40000", "10.0.0.1", "@",
	}
	for _, address := range inside {
		if !FromPrivateNetwork(address) {
			t.Errorf("사설 대역 · 루프백인데 밖으로 봤다: %q", address)
		}
	}
	for _, address := range outside {
		if FromPrivateNetwork(address) {
			t.Errorf("밖인데 사설 대역으로 봤다: %q", address)
		}
	}
}

func TestTrackingIDForTrustsOnlyPrivatePeersWithValidShape(t *testing.T) {
	accepted := []struct {
		remoteAddress string
		incoming      string
	}{
		{"127.0.0.1:40000", "trace-front-0001"},
		{"10.0.0.1:40000", "gh-12345-1"},
		{"172.16.0.1:40000", "req-3f9a0c1b2d4e"},
		{"192.168.0.1:40000", "0123456789abcdef0123456789abcdef"},
		{"[::1]:40000", "A_b-9"},
		{"[fd00::1]:40000", "x"},
		{"[::ffff:10.0.0.1]:40000", "trace-mapped-1"},
		{"10.0.0.1:40000", strings.Repeat("a", 64)},
	}
	for _, accept := range accepted {
		if got := TrackingIDFor(trackingRequest("GET", "/status", accept.remoteAddress, accept.incoming)); got != accept.incoming {
			t.Errorf("%s 에서 온 꼴 맞는 %q 를 받아야 한다: %q", accept.remoteAddress, accept.incoming, got)
		}
	}

	rejected := []struct {
		label         string
		remoteAddress string
		incoming      []string
	}{
		{"문서용 대역에서 온 값", "192.0.2.1:40000", []string{"trace-front-0001"}},
		{"문서용 IPv6 에서 온 값", "[2001:db8::1]:40000", []string{"trace-front-0001"}},
		{"IPv4 를 담은 IPv6 의 바깥", "[::ffff:203.0.113.1]:40000", []string{"trace-front-0001"}},
		{"경계 밖 172.32.0.0", "172.32.0.0:40000", []string{"trace-front-0001"}},
		{"연결 주소 없음", "", []string{"trace-front-0001"}},
		{"머리글 없음", "10.0.0.1:40000", nil},
		{"빈 값", "10.0.0.1:40000", []string{""}},
		{"65자", "10.0.0.1:40000", []string{strings.Repeat("a", 65)}},
		{"콜론(로그 줄의 구분자)", "10.0.0.1:40000", []string{"trace:forged"}},
		{"공백", "10.0.0.1:40000", []string{"trace forged"}},
		{"점", "10.0.0.1:40000", []string{"trace.forged"}},
		{"쉼표로 이은 둘", "10.0.0.1:40000", []string{"first,second"}},
		{"ASCII 밖 글자", "10.0.0.1:40000", []string{"추적-1"}},
		{"머리글이 둘(둘 다 꼴은 맞음)", "10.0.0.1:40000", []string{"trace-first", "trace-second"}},
		{"머리글이 둘(같은 값)", "127.0.0.1:40000", []string{"trace-same", "trace-same"}},
	}
	for _, reject := range rejected {
		request := trackingRequest("GET", "/status", reject.remoteAddress, reject.incoming...)
		got := TrackingIDFor(request)
		if !trackingIDPattern.MatchString(got) {
			t.Errorf("%s: 버리고 16진수 소문자 32자를 새로 만들어야 한다: %q", reject.label, got)
		}
		for _, value := range reject.incoming {
			if value != "" && strings.Contains(got, value) {
				t.Errorf("%s: 받은 값 %q 가 남았다: %q", reject.label, value, got)
			}
		}
	}
}

// 주소를 말하는 머리글은 판정에 쓰지 않는다 — 바깥이 사설 대역이라고 적어 보내도, 사설 연결이 바깥이라고 적어도 연결 주소대로다.
func TestTrackingIDForIgnoresForwardedAddressHeaders(t *testing.T) {
	spoofing := trackingRequest("GET", "/status", "203.0.113.7:40000", "trace-forged-1")
	spoofing.Header.Set("X-Forwarded-For", "10.0.0.1")
	spoofing.Header.Set("X-Real-IP", "127.0.0.1")
	spoofing.Header.Set("Forwarded", "for=10.0.0.1")
	spoofing.Header.Set("X-Client-IP", "10.0.0.1")
	if got := TrackingIDFor(spoofing); got == "trace-forged-1" || !trackingIDPattern.MatchString(got) {
		t.Fatalf("바깥 연결이 X-Forwarded-For 로 사설 대역인 체해 ID 를 심었다: %q", got)
	}

	private := trackingRequest("GET", "/status", "10.0.0.1:40000", "trace-front-0002")
	private.Header.Set("X-Forwarded-For", "203.0.113.7")
	private.Header.Set("Forwarded", "for=203.0.113.7")
	if got := TrackingIDFor(private); got != "trace-front-0002" {
		t.Fatalf("사설 연결의 ID 는 X-Forwarded-For 와 상관없이 받아야 한다: %q", got)
	}
}

func TestTrackRequestsReturnsTrackingIDOnEveryResponse(t *testing.T) {
	var mutex sync.Mutex
	seenByHandler := map[string]string{}
	remember := func(request *http.Request) {
		mutex.Lock()
		seenByHandler[request.URL.Path] = TrackingID(request.Context())
		mutex.Unlock()
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /ok", func(writer http.ResponseWriter, request *http.Request) {
		remember(request)
		WriteOK(writer, map[string]string{"answer": "yes"})
	})
	mux.HandleFunc("POST /accepted", func(writer http.ResponseWriter, request *http.Request) {
		remember(request)
		WriteAccepted(writer, map[string]string{"state": "accepted"})
	})
	mux.HandleFunc("GET /unauthorized", func(writer http.ResponseWriter, request *http.Request) {
		remember(request)
		WriteFail(writer, http.StatusUnauthorized, "unauthorized", "")
	})
	mux.HandleFunc("GET /invalid", func(writer http.ResponseWriter, request *http.Request) {
		remember(request)
		WriteFail(writer, http.StatusUnprocessableEntity, "invalid_request", "")
	})
	mux.HandleFunc("GET /broken", func(writer http.ResponseWriter, request *http.Request) {
		remember(request)
		WriteFail(writer, http.StatusInternalServerError, "store_failed", "")
	})
	mux.HandleFunc("GET /gateway", func(writer http.ResponseWriter, request *http.Request) {
		remember(request)
		WriteFail(writer, http.StatusBadGateway, "agent_unreachable", "")
	})
	mux.HandleFunc("GET /silent", func(writer http.ResponseWriter, request *http.Request) { remember(request) }) // 아무것도 쓰지 않음 = 200
	handler := TrackRequests(NewLogger(), mux)

	cases := []struct {
		method, path string
		status       int
	}{
		{"GET", "/ok", 200}, {"POST", "/accepted", 202}, {"GET", "/unauthorized", 401}, {"GET", "/invalid", 422},
		{"GET", "/broken", 500}, {"GET", "/gateway", 502}, {"GET", "/silent", 200},
		{"GET", "/no-such-route", 404}, // mux 가 내는 404
		{"DELETE", "/ok", 405},         // mux 가 내는 405
	}
	for _, testCase := range cases {
		// 사설 연결의 꼴 맞는 값 — 그대로 돌려준다
		recorder, escaped := serveCatching(handler, trackingRequest(testCase.method, testCase.path, "10.0.0.1:40000", "trace-front-0003"))
		if escaped != nil {
			t.Fatalf("%s %s: panic 이 감싸개 밖으로 샜다: %v", testCase.method, testCase.path, escaped)
		}
		if recorder.Code != testCase.status {
			t.Fatalf("%s %s: 상태가 바뀌었다 %d (기대 %d)", testCase.method, testCase.path, recorder.Code, testCase.status)
		}
		if got := recorder.Result().Header.Values(RequestIDHeader); len(got) != 1 || got[0] != "trace-front-0003" {
			t.Errorf("%s %s (%d): 응답 머리글이 받은 추적 ID 하나여야 한다: %q", testCase.method, testCase.path, testCase.status, got)
		}
		mutex.Lock()
		seen, handled := seenByHandler[testCase.path]
		delete(seenByHandler, testCase.path)
		mutex.Unlock()
		if handled && testCase.status != 404 && testCase.status != 405 && seen != "trace-front-0003" {
			t.Errorf("%s %s: 핸들러가 본 추적 ID(%q)가 응답 머리글과 다르다", testCase.method, testCase.path, seen)
		}

		// 바깥 연결의 값 — 버리고 새로 만든 값을 돌려준다
		recorder, escaped = serveCatching(handler, trackingRequest(testCase.method, testCase.path, "192.0.2.1:40000", "trace-forged-2"))
		if escaped != nil {
			t.Fatalf("%s %s: panic 이 감싸개 밖으로 샜다: %v", testCase.method, testCase.path, escaped)
		}
		returned := recorder.Result().Header.Get(RequestIDHeader)
		if !trackingIDPattern.MatchString(returned) {
			t.Errorf("%s %s (%d): 바깥의 값을 버리고 새 32자를 돌려줘야 한다: %q", testCase.method, testCase.path, testCase.status, returned)
		}
		mutex.Lock()
		seen, handled = seenByHandler[testCase.path]
		delete(seenByHandler, testCase.path)
		mutex.Unlock()
		if handled && seen != returned {
			t.Errorf("%s %s: 핸들러가 본 추적 ID(%q)와 응답 머리글(%q)이 다르다", testCase.method, testCase.path, seen, returned)
		}
	}
}

// 감싸개는 응답의 상태 · 본문 · 다른 머리글을 바꾸지 않는다 — X-Request-Id 하나만 더한다.
func TestTrackRequestsChangesNothingButTheRequestIDHeader(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /plain", func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Cache-Control", "no-store")
		writer.Header().Set("X-Content-Type-Options", "nosniff")
		writer.Header().Set("Content-Type", "text/plain; charset=utf-8")
		writer.WriteHeader(http.StatusTeapot)
		io.WriteString(writer, "plain body\n")
	})
	mux.HandleFunc("GET /envelope", func(writer http.ResponseWriter, _ *http.Request) {
		WriteFail(writer, http.StatusConflict, "agent_busy", "")
	})
	for _, path := range []string{"/plain", "/envelope", "/no-such-route"} {
		bare := httptest.NewRecorder()
		mux.ServeHTTP(bare, trackingRequest("GET", path, "10.0.0.1:40000", "trace-front-0004"))
		tracked, escaped := serveCatching(TrackRequests(NewLogger(), mux), trackingRequest("GET", path, "10.0.0.1:40000", "trace-front-0004"))
		if escaped != nil {
			t.Fatalf("%s: panic: %v", path, escaped)
		}
		if tracked.Code != bare.Code || tracked.Body.String() != bare.Body.String() {
			t.Fatalf("%s: 상태 · 본문이 바뀌었다: %d %q → %d %q", path, bare.Code, bare.Body.String(), tracked.Code, tracked.Body.String())
		}
		trackedHeaders := tracked.Header().Clone()
		if trackedHeaders.Get(RequestIDHeader) != "trace-front-0004" {
			t.Fatalf("%s: X-Request-Id 가 없다: %v", path, trackedHeaders)
		}
		trackedHeaders.Del(RequestIDHeader)
		for name, values := range bare.Header() {
			if strings.Join(trackedHeaders.Values(name), "\x00") != strings.Join(values, "\x00") {
				t.Fatalf("%s: 머리글 %s 가 바뀌었다: %q → %q", path, name, values, trackedHeaders.Values(name))
			}
		}
		if len(trackedHeaders) != len(bare.Header()) {
			t.Fatalf("%s: X-Request-Id 밖의 머리글이 더해졌다: %v → %v", path, bare.Header(), trackedHeaders)
		}
	}
}

// 감싸개는 핸들러의 panic 을 삼키지 않는다 — 지금처럼 net/http 까지 올라간다(감싸개가 실패의 꼴을 바꾸지 않는다).
func TestTrackRequestsDoesNotSwallowHandlerPanic(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /explode", func(http.ResponseWriter, *http.Request) { panic("boom") })
	recorder, escaped := serveCatching(TrackRequests(NewLogger(), mux), trackingRequest("GET", "/explode", "10.0.0.1:40000", "trace-front-0005"))
	if escaped != "boom" {
		t.Fatalf("핸들러의 panic 이 감싸개를 지나 그대로 올라가야 한다: %v", escaped)
	}
	if recorder.Body.Len() != 0 || recorder.Code != http.StatusOK {
		t.Fatalf("감싸개가 panic 대신 응답을 썼다: %d %q", recorder.Code, recorder.Body.String())
	}
}

// 실제 연결: 머리글 이름은 대소문자와 상관없이 읽고(Go 가 정규화한다), 루프백 연결의 값은 받으며, 같은 머리글이 둘이면 버린다.
func TestTrackRequestsOverRealLoopbackConnection(t *testing.T) {
	server := httptest.NewServer(TrackRequests(NewLogger(), http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		io.WriteString(writer, TrackingID(request.Context()))
	})))
	defer server.Close()
	exchange := func(headerLines string) (string, string) {
		t.Helper()
		connection, err := net.DialTimeout("tcp", server.Listener.Addr().String(), 2*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		defer connection.Close()
		_ = connection.SetDeadline(time.Now().Add(3 * time.Second))
		io.WriteString(connection, "GET /anything HTTP/1.1\r\nHost: tracked.test\r\n"+headerLines+"Connection: close\r\n\r\n")
		response, err := http.ReadResponse(bufio.NewReader(connection), nil)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		body, _ := io.ReadAll(response.Body)
		return response.Header.Get(RequestIDHeader), string(body)
	}
	if header, body := exchange("x-rEQUEST-iD: trace-lower-1\r\n"); header != "trace-lower-1" || body != "trace-lower-1" {
		t.Fatalf("머리글 이름의 대소문자와 상관없이 루프백의 값을 받아야 한다: 머리글 %q 핸들러 %q", header, body)
	}
	if header, body := exchange("X-Request-Id: trace-first\r\nx-request-id: trace-second\r\n"); !trackingIDPattern.MatchString(header) || body != header {
		t.Fatalf("머리글이 둘이면 버리고 새로 만들어야 한다: 머리글 %q 핸들러 %q", header, body)
	}
	if header, _ := exchange(""); !trackingIDPattern.MatchString(header) {
		t.Fatalf("머리글이 없으면 새로 만들어야 한다: %q", header)
	}
}

// 알림 서버로 보내기는 들어온 요청 없이 시작되는 호출이다 — 보낼 때마다 새 추적 ID 를 싣고, Send 의 첫 인자(로그의 ID)를 머리글로 올리지 않는다.
func TestAlertSenderCarriesFreshTrackingIDOnEverySend(t *testing.T) {
	received := make(chan http.Header, 8)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		io.Copy(io.Discard, request.Body)
		received <- request.Header.Clone()
		writer.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()
	t.Setenv("ALERT_EVENTS_URL", server.URL)
	t.Setenv("ALERT_SECRET", "test-value")
	sender := NewAlertSender("ci-cd", NewLogger())
	seen := map[string]bool{}
	for index := 0; index < 3; index++ {
		sender.Send("gh-1-1", "deploy_failed", "warning", "deploy-failed:llm:abc", "배포 실패 llm", "detail")
		select {
		case headers := <-received:
			values := headers.Values(RequestIDHeader)
			if len(values) != 1 || !trackingIDPattern.MatchString(values[0]) {
				t.Fatalf("알림 호출에 16진수 소문자 32자의 추적 ID 하나를 실어야 한다: %q", values)
			}
			if seen[values[0]] {
				t.Fatalf("보낼 때마다 새 값이어야 한다 — 같은 값이 다시 나왔다: %q", values[0])
			}
			seen[values[0]] = true
			if headers.Get("X-Alert-Secret") != "test-value" || headers.Get("Content-Type") != "application/json" {
				t.Fatalf("알림 호출의 기존 머리글이 바뀌었다: %v", headers)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("사건이 도착하지 않았다")
		}
	}
}

// 보내지 못했을 때의 로그 줄은 지금처럼 Send 의 첫 인자로 남는다(일의 ID 로 배포 하나의 줄을 찾는 길이 그대로).
// 보내기는 따로 도는 goroutine 이라 표준 출력을 바꿔 끼우지 않고 로그 스트림(가짜 Redis)으로 본다.
func TestAlertSenderFailureLogKeepsCallerLogID(t *testing.T) {
	t.Setenv("SERVER_NAME", "ci-master")
	redis := newFakeRedis(t, "+OK\r\n")
	t.Setenv("LOG_REDIS_URL", "redis://"+redis.listener.Addr().String())
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		io.Copy(io.Discard, request.Body)
		writer.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	t.Setenv("ALERT_EVENTS_URL", server.URL)
	NewAlertSender("ci-cd", NewLogger()).Send("gh-9-1", "deploy_failed", "warning", "k", "t", "d")
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		for _, command := range redis.received() {
			for _, part := range command {
				if part == "gh-9-1:ci-master:alert not sent: status 503 Service Unavailable" {
					return
				}
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("보내지 못한 줄이 Send 의 첫 인자로 남아야 한다: %q", redis.received())
}

// StatusRecorder — 잇는 줄의 "-> <상태>" 를 정한다: 아무것도 쓰지 않았으면 none, Write 만 했으면 200(net/http 가 200 을 내므로),
// WriteHeader 를 불렀으면 그 첫 상태. 안의 writer 에는 그대로 넘긴다.
func TestStatusRecorderOutcome(t *testing.T) {
	untouched := RecordStatus(httptest.NewRecorder())
	if got := untouched.Outcome(); got != "none" {
		t.Fatalf("아무것도 쓰지 않았으면 none: %q", got)
	}

	inner := httptest.NewRecorder()
	writeOnly := RecordStatus(inner)
	io.WriteString(writeOnly, "body")
	if got := writeOnly.Outcome(); got != "200" || inner.Code != 200 || inner.Body.String() != "body" {
		t.Fatalf("Write 만 했으면 200: %q (안 %d %q)", got, inner.Code, inner.Body.String())
	}

	inner = httptest.NewRecorder()
	headerThenWrite := RecordStatus(inner)
	headerThenWrite.WriteHeader(http.StatusNotFound)
	io.WriteString(headerThenWrite, "missing")
	if got := headerThenWrite.Outcome(); got != "404" || inner.Code != 404 || inner.Body.String() != "missing" {
		t.Fatalf("WriteHeader(404) 뒤 Write 면 404: %q (안 %d %q)", got, inner.Code, inner.Body.String())
	}

	headerOnly := RecordStatus(httptest.NewRecorder())
	headerOnly.WriteHeader(http.StatusAccepted)
	if got := headerOnly.Outcome(); got != "202" {
		t.Fatalf("WriteHeader(202) 만 했으면 202: %q", got)
	}

	via := RecordStatus(httptest.NewRecorder())
	WriteFail(via, http.StatusUnprocessableEntity, "invalid_request", "")
	if got := via.Outcome(); got != "422" {
		t.Fatalf("WriteFail(422) 는 422: %q", got)
	}
}
