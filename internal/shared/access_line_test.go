package shared

// 접속 줄(통신 규약 3.1 · 3.2 · 3.4 · 3.5, 호출자 2.2 · 3.3, 요청 ID 1.3).
//   - 받은 요청 하나에 정확히 한 줄 · 표준 출력 · JSON 한 줄(줄바꿈 하나로 끝남) · 로그의 앞머리(시각 · 수준) 없이 {"v": 로 시작.
//   - 칸과 차례: v · ts · request_id · caller · service · method · path · path_truncated · status · request_time ·
//     upstream_time · upstream_status · slot · bytes_sent · loopback(15번째).
//   - v 수 1 · ts 응답을 마친 때의 epoch 초(소수 셋째 자리의 수) · request_id 는 TrackRequests 가 정한 값(응답 머리글 · 핸들러가 본 값과 같다) ·
//     caller 는 CallerFor · service 는 감싸개에 준 이름 · method · path(URL.EscapedPath, 1024 바이트까지 — 넘으면 path_truncated) ·
//     status(쓰지 않았으면 200) · request_time(초, 소수 셋째 자리의 수) · upstream_time · upstream_status · slot 은 "" ·
//     bytes_sent 는 쓴 본문 바이트 · loopback 은 연결 주소가 127.0.0.0/8 · ::1(IPv4 를 담은 IPv6 는 IPv4 로 본다)인가.
//   - 넣지 않는 것: 쿼리 · 주소 · 리퍼러 · User-Agent · 쿠키 · 인증 · 비밀값 머리글 · 호스트 · 사용자 정보.
//   - 막지 않는다: 줄을 쓰지 못해도 응답은 그대로 · 로그의 잠금(Redis 로 보내는 길)에 기대지 않는다 · 줄은 로그 스트림(Redis)으로 가지 않는다.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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

var accessLineFieldOrder = []string{"v", "ts", "request_id", "caller", "service", "method", "path", "path_truncated", "status",
	"request_time", "upstream_time", "upstream_status", "slot", "bytes_sent", "loopback"}

var threeDecimalNumber = regexp.MustCompile(`^[0-9]+\.[0-9]{3}$`)

// accessLine — 접속 줄 하나를 칸의 차례와 날 값으로 읽은 것.
type accessLine struct {
	text   string
	order  []string
	values map[string]json.RawMessage
}

func (line accessLine) textValue(name string) string {
	var value string
	json.Unmarshal(line.values[name], &value)
	return value
}

func (line accessLine) raw(name string) string { return string(line.values[name]) }

// accessLinesIn — 표준 출력에서 {"v": 로 시작하는 줄을 모두 읽는다. 읽지 못하는 줄이 있으면 실패.
func accessLinesIn(t *testing.T, captured string) []accessLine {
	t.Helper()
	lines := []accessLine{}
	if captured != "" && !strings.HasSuffix(captured, "\n") {
		t.Fatalf("표준 출력이 줄바꿈으로 끝나지 않는다: %q", tailText(captured))
	}
	for _, text := range strings.Split(strings.TrimSuffix(captured, "\n"), "\n") {
		if !strings.HasPrefix(text, `{"v":`) {
			if strings.Contains(text, `{"v":`) {
				t.Fatalf("접속 줄이 다른 글과 한 줄에 섞였다: %q", text)
			}
			continue
		}
		decoder := json.NewDecoder(strings.NewReader(text))
		if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
			t.Fatalf("JSON 객체가 아니다: %q", text)
		}
		line := accessLine{text: text, values: map[string]json.RawMessage{}}
		for decoder.More() {
			token, err := decoder.Token()
			name, isName := token.(string)
			if err != nil || !isName {
				t.Fatalf("칸 이름을 읽지 못했다: %q", text)
			}
			var value json.RawMessage
			if err := decoder.Decode(&value); err != nil {
				t.Fatalf("칸 %s 의 값을 읽지 못했다: %q", name, text)
			}
			if _, duplicate := line.values[name]; duplicate {
				t.Fatalf("칸 %s 가 겹친다: %q", name, text)
			}
			line.order = append(line.order, name)
			line.values[name] = value
		}
		if token, err := decoder.Token(); err != nil || token != json.Delim('}') {
			t.Fatalf("객체가 닫히지 않았다: %q", text)
		}
		if _, err := decoder.Token(); err != io.EOF {
			t.Fatalf("객체 뒤에 글이 더 붙었다: %q", text)
		}
		lines = append(lines, line)
	}
	return lines
}

func tailText(text string) string {
	if len(text) > 200 {
		return text[len(text)-200:]
	}
	return text
}

// onlyAccessLine — 정확히 한 줄이어야 한다.
func onlyAccessLine(t *testing.T, label, captured string) accessLine {
	t.Helper()
	lines := accessLinesIn(t, captured)
	if len(lines) != 1 {
		t.Fatalf("%s: 요청 하나에 접속 줄은 정확히 하나여야 한다: %d줄\n%s", label, len(lines), captured)
	}
	return lines[0]
}

// serveWithAccessLines — main 처럼 TrackRequests(logger, WriteAccessLines(service, handler)) 로 한 번 부르고 표준 출력을 모은다.
func serveWithAccessLines(t *testing.T, service string, handler http.Handler, request *http.Request) (*httptest.ResponseRecorder, string, any) {
	t.Helper()
	recorder := httptest.NewRecorder()
	var escaped any
	captured := captureSharedLog(t, func() {
		wrapped := TrackRequests(NewLogger(), WriteAccessLines(service, handler))
		func() {
			defer func() { escaped = recover() }()
			wrapped.ServeHTTP(recorder, request)
		}()
	})
	return recorder, captured, escaped
}

func accessTestMux(seen *sync.Map) *http.ServeMux {
	mux := http.NewServeMux()
	remember := func(request *http.Request) { seen.Store(request.URL.Path, TrackingID(request.Context())) }
	mux.HandleFunc("GET /health", func(writer http.ResponseWriter, request *http.Request) {
		remember(request)
		WriteOK(writer, map[string]string{"mode": "master"})
	})
	mux.HandleFunc("POST /deploy", func(writer http.ResponseWriter, request *http.Request) {
		remember(request)
		io.Copy(io.Discard, request.Body)
		WriteAccepted(writer, map[string]string{"state": "accepted"})
	})
	mux.HandleFunc("GET /deploys/{id}", func(writer http.ResponseWriter, request *http.Request) {
		remember(request)
		WriteFail(writer, http.StatusUnauthorized, "unauthorized", "")
	})
	mux.HandleFunc("GET /silent", func(http.ResponseWriter, *http.Request) {}) // 아무것도 쓰지 않음 = 200 · 0바이트
	mux.HandleFunc("GET /plain", func(writer http.ResponseWriter, _ *http.Request) { io.WriteString(writer, "12345") })
	mux.HandleFunc("GET /broken", func(writer http.ResponseWriter, _ *http.Request) {
		WriteFail(writer, http.StatusInternalServerError, "store_failed", "")
	})
	return mux
}

func accessRequest(method, target, remoteAddress string, headers map[string][]string, body string) *http.Request {
	request := httptest.NewRequest(method, target, strings.NewReader(body))
	request.RemoteAddr = remoteAddress
	for name, values := range headers {
		for _, value := range values {
			request.Header.Add(name, value)
		}
	}
	return request
}

func TestCallerForMatchesNginxProtocolCaller(t *testing.T) {
	names := []string{"front", "backend", "llm-wrapper", "alert", "observe", "admin", "ci-master", "ci-agent"}
	for _, name := range names {
		for _, remoteAddress := range []string{"127.0.0.1:40000", "10.0.0.1:40000", "172.16.0.1:40000", "192.168.0.1:40000", "[::1]:40000", "[fd00::1]:40000", "[::ffff:10.0.0.1]:40000"} {
			if got := CallerFor(accessRequest("GET", "/health", remoteAddress, map[string][]string{"X-Caller-Service": {name}}, "")); got != name {
				t.Errorf("사설 연결 %s 의 %q 는 그대로: %q", remoteAddress, name, got)
			}
		}
		if got := CallerFor(accessRequest("GET", "/health", "203.0.113.7:40000", map[string][]string{"X-Caller-Service": {name}}, "")); got != "outside" {
			t.Errorf("바깥 연결의 %q 는 outside: %q", name, got)
		}
	}
	outside := []struct {
		label, remoteAddress string
		values               []string
	}{
		{"머리글 없음", "10.0.0.1:40000", nil},
		{"빈 값", "10.0.0.1:40000", []string{""}},
		{"목록 밖", "10.0.0.1:40000", []string{"evil"}},
		{"edge 는 머리글로 받지 않는다", "10.0.0.1:40000", []string{"edge"}},
		{"outside", "10.0.0.1:40000", []string{"outside"}},
		{"에이전트의 서버 이름", "10.0.0.1:40000", []string{"ci-agent-home"}},
		{"llm 의 배포 이름", "10.0.0.1:40000", []string{"llm"}},
		{"둘(둘 다 목록 안)", "10.0.0.1:40000", []string{"front", "admin"}},
		{"쉼표로 이은 둘", "10.0.0.1:40000", []string{"front, admin"}},
		{"문서용 대역", "198.51.100.7:40000", []string{"front"}},
		{"문서용 IPv6", "[2001:db8::7]:40000", []string{"front"}},
		{"IPv4 를 담은 바깥", "[::ffff:192.0.2.1]:40000", []string{"front"}},
		{"연결 주소를 모름", "", []string{"front"}},
	}
	for _, testCase := range outside {
		if got := CallerFor(accessRequest("GET", "/health", testCase.remoteAddress, map[string][]string{"X-Caller-Service": testCase.values}, "")); got != "outside" {
			t.Errorf("%s: outside 여야 한다: %q", testCase.label, got)
		}
	}
	// 바깥 연결이 주소 머리글로 사설인 체해도 연결 주소대로다
	spoofed := accessRequest("GET", "/health", "203.0.113.7:40000", map[string][]string{"X-Caller-Service": {"alert"}, "X-Forwarded-For": {"10.0.0.1"}, "X-Real-IP": {"127.0.0.1"}}, "")
	if got := CallerFor(spoofed); got != "outside" {
		t.Errorf("X-Forwarded-For 로 사설인 체한 연결: %q", got)
	}
}

// nginx 의 map 문자열 열쇠는 대소문자를 가리지 않는다(규약 8절의 실측: FRONT → front). 앱도 같은 판정을 한다.
func TestCallerForIgnoresCaseLikeNginxMap(t *testing.T) {
	for incoming, want := range map[string]string{"FRONT": "front", "Ci-Master": "ci-master", "ALERT": "alert", "LLM-Wrapper": "llm-wrapper"} {
		if got := CallerFor(accessRequest("GET", "/health", "10.0.0.1:40000", map[string][]string{"X-Caller-Service": {incoming}}, "")); got != want {
			t.Errorf("%q 는 %q 로 적는다: %q", incoming, want, got)
		}
	}
}

func TestAccessLineFieldsOrderAndTypes(t *testing.T) {
	seen := &sync.Map{}
	before := time.Now()
	request := accessRequest("GET", "/health", "10.0.0.1:40000", map[string][]string{"X-Request-Id": {"trace-front-0001"}, "X-Caller-Service": {"alert"}}, "")
	recorder, captured, escaped := serveWithAccessLines(t, "ci-master", accessTestMux(seen), request)
	after := time.Now()
	if escaped != nil {
		t.Fatalf("panic: %v", escaped)
	}
	if !strings.HasPrefix(captured, `{"v":1,"ts":`) {
		t.Fatalf("표준 출력의 줄이 {\"v\":1,\"ts\": 로 시작해야 한다(로그의 시각 · 수준이 앞에 붙지 않는다): %q", captured)
	}
	line := onlyAccessLine(t, "GET /health", captured)
	if strings.Join(line.order, ",") != strings.Join(accessLineFieldOrder, ",") {
		t.Fatalf("칸의 이름 · 차례가 3.1 · 3.4 와 다르다:\n받음 %v\n기대 %v", line.order, accessLineFieldOrder)
	}
	if line.raw("v") != "1" {
		t.Fatalf("v 는 수 1: %s", line.raw("v"))
	}
	if !threeDecimalNumber.MatchString(line.raw("ts")) {
		t.Fatalf("ts 는 소수 셋째 자리의 수(따옴표 없음): %s", line.raw("ts"))
	}
	var tsMilliseconds int64
	fmt.Sscanf(strings.Replace(line.raw("ts"), ".", "", 1), "%d", &tsMilliseconds)
	if tsMilliseconds < before.UnixMilli() || tsMilliseconds > after.UnixMilli() {
		t.Fatalf("ts 는 응답을 마친 때의 epoch 초: %s (요청 %d~%d ms)", line.raw("ts"), before.UnixMilli(), after.UnixMilli())
	}
	if line.textValue("request_id") != "trace-front-0001" || recorder.Result().Header.Get(RequestIDHeader) != "trace-front-0001" {
		t.Fatalf("request_id 는 응답 머리글과 같은 추적 ID: %q / %q", line.textValue("request_id"), recorder.Result().Header.Get(RequestIDHeader))
	}
	if seenID, _ := seen.Load("/health"); seenID != "trace-front-0001" {
		t.Fatalf("핸들러가 본 추적 ID 와 같아야 한다: %v", seenID)
	}
	want := map[string]string{"caller": `"alert"`, "service": `"ci-master"`, "method": `"GET"`, "path": `"/health"`, "path_truncated": "false",
		"status": "200", "upstream_time": `""`, "upstream_status": `""`, "slot": `""`, "loopback": "false"}
	for name, value := range want {
		if line.raw(name) != value {
			t.Errorf("%s 는 %s 여야 한다: %s", name, value, line.raw(name))
		}
	}
	if !threeDecimalNumber.MatchString(line.raw("request_time")) {
		t.Errorf("request_time 은 소수 셋째 자리의 수: %s", line.raw("request_time"))
	}
	if line.raw("bytes_sent") != fmt.Sprint(recorder.Body.Len()) || recorder.Body.Len() == 0 {
		t.Errorf("bytes_sent 는 쓴 본문 바이트(%d): %s", recorder.Body.Len(), line.raw("bytes_sent"))
	}
}

func TestAccessLineStatusBytesServiceAndLoopback(t *testing.T) {
	cases := []struct {
		label, service, method, target, remoteAddress string
		status                                        string
		bytes                                         string // "" 이면 응답 본문의 길이
		loopback                                      string
	}{
		{"접수 202", "ci-master", "POST", "/deploy", "10.0.0.1:40000", "202", "", "false"},
		{"인증 실패 401", "ci-master", "GET", "/deploys/gh-1", "127.0.0.1:40000", "401", "", "true"},
		{"경로 없음 404(mux)", "ci-agent", "GET", "/no-such-route", "127.0.0.1:40000", "404", "", "true"},
		{"메서드 405(mux)", "ci-agent", "DELETE", "/health", "[::1]:40000", "405", "", "true"},
		{"5xx", "ci-master", "GET", "/broken", "10.0.0.1:40000", "500", "", "false"},
		{"아무것도 쓰지 않음", "ci-agent", "GET", "/silent", "127.0.0.1:40000", "200", "0", "true"},
		{"Write 만", "ci-agent", "GET", "/plain", "[::ffff:127.0.0.1]:40000", "200", "5", "true"},
		{"HEAD", "ci-agent", "HEAD", "/health", "127.0.0.1:40000", "200", "", "true"},
		{"루프백 경계 밖", "ci-master", "GET", "/health", "128.0.0.1:40000", "200", "", "false"},
		{"127/8 의 끝", "ci-master", "GET", "/health", "127.255.255.254:40000", "200", "", "true"},
		{"바깥", "ci-master", "GET", "/health", "203.0.113.7:40000", "200", "", "false"},
	}
	for _, testCase := range cases {
		recorder, captured, escaped := serveWithAccessLines(t, testCase.service, accessTestMux(&sync.Map{}),
			accessRequest(testCase.method, testCase.target, testCase.remoteAddress, nil, `{"service":"llm"}`))
		if escaped != nil {
			t.Fatalf("%s: panic: %v", testCase.label, escaped)
		}
		line := onlyAccessLine(t, testCase.label, captured)
		bytesSent := testCase.bytes
		if bytesSent == "" {
			bytesSent = fmt.Sprint(recorder.Body.Len())
		}
		if line.raw("status") != testCase.status || fmt.Sprint(recorder.Code) != testCase.status {
			t.Errorf("%s: status %s (응답 %d, 기대 %s)", testCase.label, line.raw("status"), recorder.Code, testCase.status)
		}
		if line.raw("bytes_sent") != bytesSent {
			t.Errorf("%s: bytes_sent %s (기대 %s)", testCase.label, line.raw("bytes_sent"), bytesSent)
		}
		if line.textValue("service") != testCase.service || line.textValue("method") != testCase.method || line.raw("loopback") != testCase.loopback {
			t.Errorf("%s: service %s · method %s · loopback %s", testCase.label, line.raw("service"), line.raw("method"), line.raw("loopback"))
		}
		if line.textValue("caller") != "outside" {
			t.Errorf("%s: 머리글이 없으면 caller 는 outside: %s", testCase.label, line.raw("caller"))
		}
		if returned := recorder.Result().Header.Get(RequestIDHeader); line.textValue("request_id") != returned || !trackingIDPattern.MatchString(returned) {
			t.Errorf("%s: request_id(%s)가 응답 머리글(%q)과 다르다", testCase.label, line.raw("request_id"), returned)
		}
	}
}

// 경로: 첫 날것 ? 앞(URL.EscapedPath) · ; 와 %3F 는 남는다 · 1024 바이트까지 · 절대 주소 꼴도 경로만. 쿼리 · 주소 · 비밀값 · 사용자 정보는 줄에 없다.
func TestAccessLinePathAndForbiddenContent(t *testing.T) {
	markerHeaders := map[string][]string{
		"Referer": {"MARKER-referer"}, "User-Agent": {"MARKER-agent"}, "Cookie": {"session=MARKER-cookie"}, "Authorization": {"Bearer MARKER-authorization"},
		"X-Deploy-Secret": {"MARKER-secret"}, "X-Alert-Secret": {"MARKER-alert-secret"}, "X-Forwarded-For": {"203.0.113.9"}, "X-Real-IP": {"198.51.100.7"},
		"X-Caller-Service": {"front"}, "X-Request-Id": {"trace-front-0002"},
	}
	long := "/" + strings.Repeat("a", 1100)
	exact := "/" + strings.Repeat("b", 1023)
	cases := []struct {
		label, target, path, truncated string
	}{
		{"쿼리를 뗀다", "/search?q=MARKER-query", "/search", "false"},
		{"; 는 남는다", "/search;q=x", "/search;q=x", "false"},
		{"%3F 는 남는다", "/search%3Fq=x", "/search%3Fq=x", "false"},
		{"1100자 → 1024 바이트", long + "?q=MARKER-query", long[:1024], "true"},
		{"딱 1024 바이트", exact, exact, "false"},
		{"절대 주소 꼴(사용자 정보 · 호스트)", "http://MARKER-user:MARKER-password@MARKER-host.example/a/b?q=MARKER-query", "/a/b", "false"},
		{"따옴표 · 역슬래시", "/a%22b%5Cc", "/a%22b%5Cc", "false"},
		{"ASCII 밖 글자는 퍼센트 꼴", "/문서", "/%EB%AC%B8%EC%84%9C", "false"},
	}
	for _, testCase := range cases {
		request := accessRequest("GET", testCase.target, "10.0.0.1:40000", markerHeaders, "MARKER-body")
		_, captured, escaped := serveWithAccessLines(t, "ci-master", accessTestMux(&sync.Map{}), request)
		if escaped != nil {
			t.Fatalf("%s: panic: %v", testCase.label, escaped)
		}
		line := onlyAccessLine(t, testCase.label, captured)
		if line.textValue("path") != testCase.path || line.raw("path_truncated") != testCase.truncated {
			t.Errorf("%s: path %q truncated %s (기대 %q %s)", testCase.label, line.textValue("path"), line.raw("path_truncated"), testCase.path, testCase.truncated)
		}
		if strings.Contains(line.text, "MARKER") || strings.Contains(line.text, "?") || strings.Contains(line.text, "203.0.113.9") ||
			strings.Contains(line.text, "198.51.100.7") || strings.Contains(line.text, "10.0.0.1") {
			t.Errorf("%s: 넣지 않는 것이 줄에 있다: %s", testCase.label, line.text)
		}
		if line.textValue("caller") != "front" || line.textValue("request_id") != "trace-front-0002" {
			t.Errorf("%s: caller · request_id: %s %s", testCase.label, line.raw("caller"), line.raw("request_id"))
		}
	}
}

// 실제 연결: 따옴표 · 줄바꿈을 노린 머리글과 이상한 경로가 줄을 깨지 않고, 동시의 요청과 로그 줄이 섞여도 줄마다 온전하다.
func TestAccessLinesStayWholeUnderConcurrencyWithLogLines(t *testing.T) {
	const requests = 64
	logger := NewLogger()
	captured := captureSharedLog(t, func() {
		handler := TrackRequests(logger, WriteAccessLines("ci-agent", http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			logger.Log(TrackingID(request.Context()), "handled "+request.URL.EscapedPath(), "info") // 같은 표준 출력에 로그 줄도 쓴다
			io.WriteString(writer, "ok")
		})))
		server := httptest.NewServer(handler)
		defer server.Close()
		var group sync.WaitGroup
		for index := 0; index < requests; index++ {
			group.Add(1)
			go func(index int) {
				defer group.Done()
				connection, err := net.DialTimeout("tcp", server.Listener.Addr().String(), 2*time.Second)
				if err != nil {
					t.Error(err)
					return
				}
				defer connection.Close()
				_ = connection.SetDeadline(time.Now().Add(5 * time.Second))
				fmt.Fprintf(connection, "GET /path-%d/%%0A%%22x%%5C HTTP/1.1\r\nHost: tracked.test\r\nX-Caller-Service: ci-master\r\nUser-Agent: \"}MARKER\r\nConnection: close\r\n\r\n", index)
				response, err := http.ReadResponse(bufio.NewReader(connection), nil)
				if err != nil {
					t.Error(err)
					return
				}
				io.Copy(io.Discard, response.Body)
				response.Body.Close()
			}(index)
		}
		group.Wait()
	})
	lines := accessLinesIn(t, captured)
	if len(lines) != requests {
		t.Fatalf("요청 %d개에 접속 줄 %d줄", requests, len(lines))
	}
	identifiers := map[string]bool{}
	for _, line := range lines {
		identifiers[line.textValue("request_id")] = true
		if line.textValue("caller") != "ci-master" || line.raw("loopback") != "true" || strings.Contains(line.text, "MARKER") {
			t.Fatalf("줄의 값이 틀렸다: %s", line.text)
		}
		if !strings.Contains(captured, " "+line.textValue("request_id")+":") {
			t.Fatalf("같은 요청의 로그 줄과 request_id 가 다르다: %s", line.textValue("request_id"))
		}
	}
	if len(identifiers) != requests {
		t.Fatalf("요청마다 다른 request_id 여야 한다: %d", len(identifiers))
	}
}

// 줄을 쓰지 못해도(표준 출력이 닫힘) 응답은 그대로다 — 오류 · panic 으로 요청을 실패시키지 않는다.
func TestAccessLineWriteFailureDoesNotFailRequest(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	reader.Close()
	writer.Close() // 닫힌 파일 — 쓰기는 오류를 낸다
	original := os.Stdout
	os.Stdout = writer
	defer func() { os.Stdout = original }()
	recorder := httptest.NewRecorder()
	var escaped any
	func() {
		defer func() { escaped = recover() }()
		TrackRequests(NewLogger(), WriteAccessLines("ci-master", accessTestMux(&sync.Map{}))).ServeHTTP(recorder,
			accessRequest("POST", "/deploy", "10.0.0.1:40000", nil, "{}"))
	}()
	if escaped != nil {
		t.Fatalf("줄을 쓰지 못한 것이 panic 이 됐다: %v", escaped)
	}
	if recorder.Code != 202 || recorder.Body.String() != `{"data":{"state":"accepted"},"success":true}`+"\n" {
		t.Fatalf("응답이 바뀌었다: %d %q", recorder.Code, recorder.Body.String())
	}
}

// 줄은 로그의 잠금(Redis 로 보내는 길 — 300ms 연결 시도를 잠근 채 한다)에 기대지 않는다: 잠금이 잡혀 있어도 요청은 끝난다.
func TestAccessLineDoesNotWaitOnLoggerLock(t *testing.T) {
	logger := NewLogger()
	logger.mutex.Lock()
	defer logger.mutex.Unlock()
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		recorder := httptest.NewRecorder()
		TrackRequests(logger, WriteAccessLines("ci-master", accessTestMux(&sync.Map{}))).ServeHTTP(recorder, accessRequest("GET", "/health", "127.0.0.1:40000", nil, ""))
		done <- recorder
	}()
	select {
	case recorder := <-done:
		if recorder.Code != 200 {
			t.Fatalf("응답: %d", recorder.Code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("로그의 잠금이 잡혀 있는 동안 요청이 끝나지 않았다 — 접속 줄이 로그의 잠금을 기다린다")
	}
}

// 접속 줄은 로그 스트림(Redis XADD)으로 가지 않는다 — 에이전트가 도커 로그에서 읽는다. 로그 줄은 지금처럼 간다.
func TestAccessLineIsNotSentToLogStream(t *testing.T) {
	redis := newFakeRedis(t, "+OK\r\n")
	t.Setenv("LOG_REDIS_URL", "redis://"+redis.listener.Addr().String())
	logger := NewLogger()
	captured := captureSharedLog(t, func() {
		TrackRequests(logger, WriteAccessLines("ci-master", http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			logger.Log(TrackingID(request.Context()), "handled", "info")
			io.WriteString(writer, "ok")
		}))).ServeHTTP(httptest.NewRecorder(), accessRequest("GET", "/health", "127.0.0.1:40000", nil, ""))
	})
	onlyAccessLine(t, "GET /health", captured)
	deadline := time.Now().Add(2 * time.Second)
	for len(redis.received()) < 1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	commands := redis.received()
	if len(commands) != 1 {
		t.Fatalf("로그 스트림에는 로그 줄 하나만: %d %q", len(commands), commands)
	}
	for _, part := range commands[0] {
		if strings.Contains(part, `{"v":`) {
			t.Fatalf("접속 줄이 로그 스트림으로 갔다: %q", commands[0])
		}
	}
}

// 클라이언트가 도중에 끊어도 그 요청의 줄은 핸들러가 끝난 뒤 한 줄 남는다.
func TestAccessLineWrittenOnceWhenClientDisconnects(t *testing.T) {
	handlerDone := make(chan struct{})
	var captured string
	captured = captureSharedLog(t, func() {
		server := httptest.NewServer(TrackRequests(NewLogger(), WriteAccessLines("ci-agent", http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			io.Copy(io.Discard, request.Body) // 본문을 다 읽어야 net/http 가 끊김을 알아챈다
			select {
			case <-request.Context().Done(): // 끊길 때까지
			case <-time.After(3 * time.Second):
			}
			io.WriteString(writer, "late")
			close(handlerDone)
		}))))
		defer server.Close()
		ctx, cancel := context.WithCancel(context.Background())
		request, _ := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/agent/ci", bytes.NewReader([]byte("{}")))
		go func() {
			time.Sleep(100 * time.Millisecond)
			cancel()
		}()
		if response, err := http.DefaultClient.Do(request); err == nil {
			response.Body.Close()
		}
		select {
		case <-handlerDone:
		case <-time.After(3 * time.Second):
			t.Error("핸들러가 끝나지 않았다")
		}
		time.Sleep(100 * time.Millisecond) // 감싸개가 줄을 쓰도록
	})
	line := onlyAccessLine(t, "끊긴 연결", captured)
	if line.textValue("path") != "/agent/ci" || line.textValue("method") != "POST" {
		t.Fatalf("끊긴 요청의 줄: %s", line.text)
	}
}

// 감싸개는 핸들러의 panic 을 삼키지 않는다(T2 에서 정한 것 — 지금처럼 net/http 까지 올라간다).
func TestAccessLinesDoNotSwallowHandlerPanic(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /explode", func(http.ResponseWriter, *http.Request) { panic("boom") })
	_, _, escaped := serveWithAccessLines(t, "ci-master", mux, accessRequest("GET", "/explode", "127.0.0.1:40000", nil, ""))
	if escaped != "boom" {
		t.Fatalf("panic 이 그대로 올라가야 한다: %v", escaped)
	}
}

// 감싸개는 응답의 상태 · 본문 · 머리글을 바꾸지 않는다.
func TestAccessLinesChangeNothingInTheResponse(t *testing.T) {
	for _, target := range []string{"/health", "/plain", "/silent", "/broken", "/no-such-route", "/deploys/gh-1"} {
		bare := httptest.NewRecorder()
		TrackRequests(NewLogger(), accessTestMux(&sync.Map{})).ServeHTTP(bare, accessRequest("GET", target, "10.0.0.1:40000", map[string][]string{"X-Request-Id": {"trace-front-0003"}}, ""))
		tracked, _, escaped := serveWithAccessLines(t, "ci-master", accessTestMux(&sync.Map{}), accessRequest("GET", target, "10.0.0.1:40000", map[string][]string{"X-Request-Id": {"trace-front-0003"}}, ""))
		if escaped != nil {
			t.Fatalf("%s: panic: %v", target, escaped)
		}
		bareResult, trackedResult := bare.Result(), tracked.Result()
		if bare.Code != tracked.Code || bare.Body.String() != tracked.Body.String() || fmt.Sprint(bareResult.Header) != fmt.Sprint(trackedResult.Header) {
			t.Fatalf("%s: 응답이 바뀌었다: %d %v %q → %d %v %q", target, bare.Code, bareResult.Header, bare.Body.String(), tracked.Code, trackedResult.Header, tracked.Body.String())
		}
	}
}
