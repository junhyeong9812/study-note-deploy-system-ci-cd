package shared

// 배포 실패 출력의 가림 · 크기 상한 — 기준: requirement-spec.md(2026-11-25 admin-incident-assist) §2 · §15.
//
// 정한 계약(이 파일이 고정한다):
//   - 차례: 글자 정리 → 가림 → 자르기. 자르기는 가린 뒤의 바이트로 잰다(비밀값이 자른 경계에 걸쳐 반쪽이 남지 않는다).
//   - 글자 정리: 깨진 UTF-8 → U+FFFD · ANSI 제어열(CSI `ESC [ … 끝 글자` · OSC `ESC ] … BEL|ESC \` · 그 밖의 `ESC 글자`)은 지운다 ·
//     `\r\n` 과 홀로 선 `\r` 은 줄바꿈 하나 · 탭과 줄바꿈 밖의 제어 문자(NUL 포함 0x00–0x1F · 0x7F)는 U+FFFD.
//   - 가림은 줄 단위로 한다. 줄바꿈 없이 64KB 를 넘는 줄은 64KB 마다 나누되, 나누는 자리는 그 조각의 마지막 4KB 안의 마지막 공백 · 탭 뒤
//     (없으면 64KB 의 글자 경계).
//   - 내부 주소(private_ip): IPv4 10/8 · 172.16/12 · 192.168/16 · 100.64/10 · 169.254/16, IPv6 fc00::/7 · fe80::/10(첫 묶음이 네 자리로 적힌 것 —
//     `%zone` 포함). 문서용 대역(192.0.2/24 · 198.51.100/24 · 203.0.113/24 · 2001:db8::/32)과 127.0.0.1 · ::1 은 남긴다.
//   - 가린 자리는 `[가림:<종류>]` 로 바뀐다. 종류: token · hex · pem · authorization · secret_field · url_credential · private_ip · email.
//     이미 `[가림:` 으로 시작하는 값은 다시 가리지도 세지도 않는다(같은 것을 두 번 거쳐도 글과 횟수가 그대로).
//   - 상한: 가린 뒤 FailureOutputLimit(256KB) 이하면 그대로. 넘으면 앞(FailureOutputHeadLimit 32KB 이하 — 그 안의 마지막 줄바꿈까지,
//     줄바꿈이 없으면 글자 경계) + 표시 줄 `[가운데 <N>바이트를 버렸다]` + 끝(줄의 처음부터 — 그 창에 줄바꿈이 없으면 글자 경계).
//     표시 줄은 언제나 한 줄을 차지한다(앞이 줄바꿈으로 끝나지 않으면 앞에 줄바꿈 하나를 더 넣는다). 전체는 256KB 이하.
//     N 은 버린 가린 뒤의 바이트 수이고 OmittedBytes 와 같다.

import (
	"bytes"
	"fmt"
	"math/rand"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"unicode/utf8"
)

// ---- 가짜 비밀값 — 실제 꼴을 흉내 낸 무작위 글자. 소스에 비밀값 꼴의 글자를 그대로 두지 않는다(접두어도 나눠 이어 붙인다) ----

const alphanumeric = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
const lowerHex = "0123456789abcdef"

func fakeCharacters(seed int64, length int, alphabet string) string {
	generator := rand.New(rand.NewSource(seed))
	var builder strings.Builder
	for range length {
		builder.WriteByte(alphabet[generator.Intn(len(alphabet))])
	}
	return builder.String()
}

func fakeGithubToken(seed int64) string { return "gh" + "p_" + fakeCharacters(seed, 36, alphanumeric) }
func fakeOpenAIStyleKey(seed int64) string {
	return "s" + "k-proj-" + fakeCharacters(seed, 40, alphanumeric)
}
func fakeAccessKeyID(seed int64) string {
	return "AK" + "IA" + fakeCharacters(seed, 16, "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567")
}

const base64URL = alphanumeric + "-_"

func fakeJWT(seed int64) string {
	return "ey" + "J" + fakeCharacters(seed, 30, base64URL) + ".ey" + "J" + fakeCharacters(seed+1, 60, base64URL) + "." + fakeCharacters(seed+2, 43, base64URL)
}

// noLongRun — 글 안에 글자 · 숫자가 minimum 개 이상 이어진 덩어리가 없는지. 가린 글에는 가짜 비밀값의 조각이 남으면 안 된다.
var alphanumericRun = regexp.MustCompile(`[A-Za-z0-9]+`)

func longestRun(text string) string {
	longest := ""
	for _, run := range alphanumericRun.FindAllString(text, -1) {
		if len(run) > len(longest) {
			longest = run
		}
	}
	return longest
}

// requireNoFragment — 비밀값의 어떤 12글자 조각도 글에 없다.
func requireNoFragment(t *testing.T, text, secret string) {
	t.Helper()
	for start := 0; start+12 <= len(secret); start++ {
		if fragment := secret[start : start+12]; strings.Contains(text, fragment) {
			t.Fatalf("가린 글에 비밀값의 조각 %q 가 남았다:\n%s", fragment, text)
		}
	}
}

func mask(kind string) string { return "[가림:" + kind + "]" }

// ---- 1. 키 꼴 ----

func TestCaptureRedactsKeyShapes(t *testing.T) {
	privateKeyBody := []string{fakeCharacters(31, 64, alphanumeric+"+/"), fakeCharacters(32, 64, alphanumeric+"+/"), fakeCharacters(33, 20, alphanumeric+"+/") + "=="}
	cases := []struct {
		name, input, secret, kind string
		kept                      []string
	}{
		{"GitHub 개인 토큰", "remote: using " + fakeGithubToken(1) + " for fetch", fakeGithubToken(1), "token", []string{"remote: using ", " for fetch"}},
		{"GitHub OAuth 토큰", "GH_OAUTH=" + "gh" + "o_" + fakeCharacters(2, 36, alphanumeric) + " ok", "gh" + "o_" + fakeCharacters(2, 36, alphanumeric), "", []string{" ok"}},
		{"GitHub 세분화 토큰", "token " + "github" + "_pat_" + fakeCharacters(3, 22, alphanumeric) + "_" + fakeCharacters(4, 59, alphanumeric) + " end",
			"github" + "_pat_" + fakeCharacters(3, 22, alphanumeric) + "_" + fakeCharacters(4, 59, alphanumeric), "token", []string{" end"}},
		{"sk- 키", "calling api with " + fakeOpenAIStyleKey(5) + " now", fakeOpenAIStyleKey(5), "token", []string{"calling api with ", " now"}},
		{"sk-ant 꼴 키", "key " + "s" + "k-ant-api03-" + fakeCharacters(6, 80, base64URL) + " done", "s" + "k-ant-api03-" + fakeCharacters(6, 80, base64URL), "token", []string{" done"}},
		{"AKIA 키", "aws id " + fakeAccessKeyID(7) + " region", fakeAccessKeyID(7), "token", []string{"aws id ", " region"}},
		{"JWT", "cookie " + fakeJWT(8) + " expired", fakeJWT(8), "token", []string{"cookie ", " expired"}},
		{"긴 16진(64자)", "seed " + fakeCharacters(9, 64, lowerHex) + " used", fakeCharacters(9, 64, lowerHex), "hex", []string{"seed ", " used"}},
		{"긴 16진(대문자 섞임)", "value=" + strings.ToUpper(fakeCharacters(10, 48, lowerHex)) + " x", strings.ToUpper(fakeCharacters(10, 48, lowerHex)), "", []string{" x"}},
		{"PEM 여러 줄", "loading key\n-----BEGIN " + "RSA PRIVATE KEY-----\n" + strings.Join(privateKeyBody, "\n") + "\n-----END " + "RSA PRIVATE KEY-----\nnext step",
			privateKeyBody[0], "pem", []string{"loading key\n", "next step"}},
		{"PEM 한 줄(JSON 안의 \\n)", `{"key":"-----BEGIN ` + `PRIVATE KEY-----\n` + privateKeyBody[1] + `\n-----END ` + `PRIVATE KEY-----","id":7}`,
			privateKeyBody[1], "pem", []string{`{"key":"`, `","id":7}`}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			captured := CaptureOutput(testCase.input)
			requireNoFragment(t, captured.Text, testCase.secret)
			if testCase.kind != "" && !strings.Contains(captured.Text, mask(testCase.kind)) {
				t.Fatalf("%s 로 가려야 한다: %q", mask(testCase.kind), captured.Text)
			}
			if !strings.Contains(captured.Text, "[가림:") {
				t.Fatalf("가린 자리가 없다: %q", captured.Text)
			}
			for _, kept := range testCase.kept {
				if !strings.Contains(captured.Text, kept) {
					t.Fatalf("둘레의 글 %q 가 사라졌다: %q", kept, captured.Text)
				}
			}
		})
	}
}

func TestCaptureRemovesWholePemBlockAndKeepsFollowingLines(t *testing.T) {
	body := fakeCharacters(41, 64, alphanumeric+"+/")
	input := "before\n-----BEGIN " + "OPENSSH PRIVATE KEY-----\n" + body + "\n" + body[:40] + "\n-----END " + "OPENSSH PRIVATE KEY-----\nafter 1\nafter 2\n"
	want := "before\n" + mask("pem") + "\nafter 1\nafter 2\n"
	if captured := CaptureOutput(input); captured.Text != want {
		t.Fatalf("PEM 덩어리는 표시 한 줄로 바뀌고 앞뒤 줄은 그대로여야 한다:\n got %q\nwant %q", captured.Text, want)
	}
}

// ---- 2. 인증 머리 · 비밀 칸 · URL 의 자격 ----

func TestCaptureRedactsAuthorizationAndSecretFields(t *testing.T) {
	value := fakeCharacters(11, 24, alphanumeric)
	cases := []struct{ name, input, want string }{
		{"Authorization Bearer", "> Authorization: Bearer " + value, "> Authorization: Bearer " + mask("authorization")},
		{"소문자 · = · Basic", "authorization=Basic " + value + " next", "authorization=Basic " + mask("authorization") + " next"},
		{"방식 없는 Authorization", "AUTHORIZATION: " + value, "AUTHORIZATION: " + mask("authorization")},
		{"curl 의 머리", `curl -H "Authorization: token ` + value + `" https://api.example.com`, `curl -H "Authorization: token ` + mask("authorization") + `" https://api.example.com`},
		{"홀로 선 Bearer", "sending bearer " + value + " to upstream", "sending bearer " + mask("authorization") + " to upstream"},
		{"password=", "password=" + value + " user=ci", "password=" + mask("secret_field") + " user=ci"},
		{"PASSWORD: ", "DB PASSWORD: " + value, "DB PASSWORD: " + mask("secret_field")},
		{"환경 변수와 따옴표", `DB_PASSWORD="` + value + `"`, `DB_PASSWORD="` + mask("secret_field") + `"`},
		{"작은따옴표", `'client_secret': '` + value + `', 'scope': 'read'`, `'client_secret': '` + mask("secret_field") + `', 'scope': 'read'`},
		{"JSON", `{"token":"` + value + `","user":"ci"}`, `{"token":"` + mask("secret_field") + `","user":"ci"}`},
		{"빈칸을 둔 =", "client_secret = " + value, "client_secret = " + mask("secret_field")},
		{"api_key", "api_key: " + value, "api_key: " + mask("secret_field")},
		{"우리 머리글", "X-Deploy-Secret: " + value, "X-Deploy-Secret: " + mask("secret_field")},
		{"점으로 이은 설정 이름", "spring.datasource.password=" + value, "spring.datasource.password=" + mask("secret_field")},
		{"질의 문자열", "GET /callback?token=" + value + "&page=2 HTTP/1.1", "GET /callback?token=" + mask("secret_field") + "&page=2 HTTP/1.1"},
		{"빈 값은 그대로", `{"password":"","user":"ci"}`, `{"password":"","user":"ci"}`},
		{"URL 안의 user:pass", "cloning https://deployer:" + value + "@git.example.com/app.git", "cloning https://" + mask("url_credential") + "@git.example.com/app.git"},
		{"비밀번호만 있는 URL", "redis://:" + value + "@cache:6380/0", "redis://" + mask("url_credential") + "@cache:6380/0"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if captured := CaptureOutput(testCase.input); captured.Text != testCase.want {
				t.Fatalf("가린 결과가 다르다:\n got %q\nwant %q", captured.Text, testCase.want)
			}
		})
	}
}

// ---- 3. 사설 대역 IPv4 · 메일 ----

func TestCaptureRedactsPrivateIPv4AndKeepsDocumentationRanges(t *testing.T) {
	cases := []struct{ input, want string }{
		{"dial tcp 10.0.0.1:5432: connect: connection refused", "dial tcp " + mask("private_ip") + ":5432: connect: connection refused"},
		{"upstream 172.16.0.1 and 172.31.0.1", "upstream " + mask("private_ip") + " and " + mask("private_ip")},
		{"host=192.168.0.20,", "host=" + mask("private_ip") + ","},
		{"(10.255.255.254)", "(" + mask("private_ip") + ")"},
		// 문서용 대역 · 되돌이 주소는 가리지 않는다
		{"connecting to 192.0.2.10:5432", "connecting to 192.0.2.10:5432"},
		{"via 198.51.100.7 and 203.0.113.9", "via 198.51.100.7 and 203.0.113.9"},
		{"listening on 127.0.0.1:8080", "listening on 127.0.0.1:8080"},
		// 점으로 이은 더 긴 수의 일부는 주소가 아니다
		{"version 3.10.0.1.4 released", "version 3.10.0.1.4 released"},
	}
	for _, testCase := range cases {
		if captured := CaptureOutput(testCase.input); captured.Text != testCase.want {
			t.Fatalf("주소 가림이 다르다:\n got %q\nwant %q", captured.Text, testCase.want)
		}
	}
}

func TestCaptureRedactsWiderInternalRanges(t *testing.T) {
	ip := mask("private_ip")
	cases := []struct{ input, want string }{
		{"tailnet peer 100.64.0.1 up", "tailnet peer " + ip + " up"},
		{"peer 100.127.255.254:41641", "peer " + ip + ":41641"},
		{"metadata 169.254.169.254/latest", "metadata " + ip + "/latest"},
		{"link fe80::1ff:fe23:4567:890a%eth0 ready", "link " + ip + " ready"},
		{"ula [fd12:3456:789a:1::1]:8080", "ula [" + ip + "]:8080"},
		{"ula fc00::1 and FDAB:0:0:1::2", "ula " + ip + " and " + ip},
		{"link-local feb0::1", "link-local " + ip},
		// 경계 밖 · 문서용 · 되돌이는 남긴다
		{"edge 100.63.255.255 and 100.128.0.1", "edge 100.63.255.255 and 100.128.0.1"},
		{"doc 2001:db8::1 and 2001:db8:0:1::5", "doc 2001:db8::1 and 2001:db8:0:1::5"},
		{"listening on [::1]:8080", "listening on [::1]:8080"},
		{"not link-local fec0::1", "not link-local fec0::1"},
		// IPv6 처럼 보이는 정상 글
		{"took 12:34:56 at fd01", "took 12:34:56 at fd01"},
		{"mac fc:aa:14:2b:3c:4d and fe:80:00:11:22:33", "mac fc:aa:14:2b:3c:4d and fe:80:00:11:22:33"},
		{"fdab: unexpected token", "fdab: unexpected token"},
		{"func fe80ab(x int) map[string]int", "func fe80ab(x int) map[string]int"},
		{"hash fdab" + fakeCharacters(36, 30, lowerHex) + " ok", "hash fdab" + fakeCharacters(36, 30, lowerHex) + " ok"},
		{"key fd00:value", "key fd00:value"},
	}
	for _, testCase := range cases {
		if captured := CaptureOutput(testCase.input); captured.Text != testCase.want {
			t.Fatalf("내부 주소 가림이 다르다:\n got %q\nwant %q", captured.Text, testCase.want)
		}
	}
}

func TestCaptureRedactsEmailAndKeepsPackageNotation(t *testing.T) {
	cases := []struct{ input, want string }{
		{"Author: Deploy Bot <deploy.bot+ci@example.com>", "Author: Deploy Bot <" + mask("email") + ">"},
		{"notify ops-team@mail.example.org now", "notify " + mask("email") + " now"},
		{"npm WARN deprecated @types/node@20.1.0", "npm WARN deprecated @types/node@20.1.0"},
		{"pulled app@sha256:" + fakeCharacters(12, 64, lowerHex), "pulled app@sha256:" + fakeCharacters(12, 64, lowerHex)},
	}
	for _, testCase := range cases {
		if captured := CaptureOutput(testCase.input); captured.Text != testCase.want {
			t.Fatalf("메일 가림이 다르다:\n got %q\nwant %q", captured.Text, testCase.want)
		}
	}
}

// ---- 4. 정상 글은 지나치게 지우지 않는다 ----

func TestCaptureKeepsOrdinaryBuildOutputUnchanged(t *testing.T) {
	commitSha := fakeCharacters(13, 40, lowerHex)
	digest := fakeCharacters(14, 64, lowerHex)
	ordinary := strings.Join([]string{
		"#12 [test 4/5] RUN go vet ./... && go test ./...",
		"--- FAIL: TestPasswordReset (0.01s)",
		"    internal/auth/token_test.go:42: expected 200, got 500",
		"    src/secret_store.py:17: AssertionError",
		"FAILED tests/test_password.py::test_reset - assert 1 == 2",
		"FAIL\tgithub.com/example/app/internal/auth\t0.123s",
		"com.example.TokenServiceTest > refresh() FAILED",
		"HEAD is now at " + commitSha[:7] + " fix: queue order",
		"commit " + commitSha,
		"#15 writing image sha256:" + digest + " done",
		"go: downloading golang.org/x/sys v0.22.0",
		"built /srv/app/build/output.txt in 1.23s",
		"risk-assessment-module-tests passed",
		"Step 5/12 : RUN npm ci --no-audit",
		"connecting to 192.0.2.10:5432",
		"\tat com.example.Queue.order(Queue.java:88)",
		"panic: runtime error: index out of range [3] with length 3",
		"goroutine 1 [running]: main.main() 0xc000012345",
		"테스트 실패 — 기대 3, 받음 2 🙂",
		"",
	}, "\n")
	captured := CaptureOutput(ordinary)
	if captured.Text != ordinary {
		t.Fatalf("정상 출력이 바뀌었다:\n got %q\nwant %q", captured.Text, ordinary)
	}
	if len(captured.Redactions) != 0 {
		t.Fatalf("가린 것이 없어야 한다: %v", captured.Redactions)
	}
	if captured.Truncated || captured.OmittedBytes != 0 {
		t.Fatalf("상한 밑인데 잘렸다: %+v", captured)
	}
}

func TestCaptureLongHexBoundaryKeepsCommitShaAndDigests(t *testing.T) {
	cases := []struct{ input, want string }{
		{"sha " + fakeCharacters(15, 40, lowerHex), "sha " + fakeCharacters(15, 40, lowerHex)},                   // 커밋 SHA(40자) 는 남긴다
		{"value " + fakeCharacters(16, 41, lowerHex), "value " + mask("hex")},                                    // 41자부터 가린다
		{"value " + fakeCharacters(17, 64, lowerHex) + ".", "value " + mask("hex") + "."},                        // 64자
		{"image sha256:" + fakeCharacters(18, 64, lowerHex), "image sha256:" + fakeCharacters(18, 64, lowerHex)}, // 다이제스트 표기는 남긴다
		{"blob sha512:" + fakeCharacters(19, 128, lowerHex), "blob sha512:" + fakeCharacters(19, 128, lowerHex)},
	}
	for _, testCase := range cases {
		if captured := CaptureOutput(testCase.input); captured.Text != testCase.want {
			t.Fatalf("16진의 경계가 다르다:\n got %q\nwant %q", captured.Text, testCase.want)
		}
	}
}

func TestCaptureCountsRedactionsByKind(t *testing.T) {
	input := fakeGithubToken(20) + " and " + fakeGithubToken(21) + "\nmail ci@example.com from 10.0.0.1\npassword=" + fakeCharacters(22, 12, alphanumeric) + "\n"
	captured := CaptureOutput(input)
	want := map[string]int{"token": 2, "email": 1, "private_ip": 1, "secret_field": 1}
	if fmt.Sprint(captured.Redactions) != fmt.Sprint(want) {
		t.Fatalf("가린 횟수가 다르다: %v, 기대 %v", captured.Redactions, want)
	}
}

func TestCaptureTwiceChangesNothing(t *testing.T) {
	input := strings.Join([]string{
		"Authorization: Bearer " + fakeCharacters(23, 30, alphanumeric),
		"password=" + fakeCharacters(24, 16, alphanumeric) + " token: " + fakeGithubToken(25),
		"https://user:" + fakeCharacters(26, 12, alphanumeric) + "@git.example.com/x.git",
		"10.0.0.1 ci@example.com " + fakeCharacters(27, 64, lowerHex) + " " + fakeJWT(28),
	}, "\n")
	first := CaptureOutput(input)
	second := CaptureOutput(first.Text)
	if second.Text != first.Text {
		t.Fatalf("가린 글을 다시 거치면 바뀌지 않아야 한다:\n first %q\nsecond %q", first.Text, second.Text)
	}
	for kind, count := range second.Redactions {
		if count != 0 {
			t.Fatalf("이미 가린 자리를 다시 셌다: %s=%d", kind, count)
		}
	}
	if len(first.Redactions) == 0 {
		t.Fatalf("처음에는 가린 것이 있어야 한다: %q", first.Text)
	}
}

// ---- 5. 글자 정리 ----

func TestCaptureCleansTerminalControlSequences(t *testing.T) {
	cases := []struct{ name, input, want string }{
		{"색 코드", "\x1b[1;31m--- FAIL\x1b[0m: TestQueue\n", "--- FAIL: TestQueue\n"},
		{"OSC 제목", "\x1b]0;building\x07step 1\n", "step 1\n"},
		{"CRLF", "line one\r\nline two\r\n", "line one\nline two\n"},
		{"홀로 선 CR", "progress 10%\rprogress 100%\n", "progress 10%\nprogress 100%\n"},
		{"NUL · 그 밖의 제어", "a\x00b\x07c\x7fd\te\n", "a�b�c�d\te\n"},
		{"깨진 UTF-8", "ok \xff\xfe end\n", "ok � end\n"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if captured := CaptureOutput(testCase.input); captured.Text != testCase.want {
				t.Fatalf("글자 정리가 다르다:\n got %q\nwant %q", captured.Text, testCase.want)
			}
		})
	}
}

func TestCaptureRedactsSecretFieldSplitByColorCodes(t *testing.T) {
	value := fakeCharacters(29, 20, alphanumeric)
	captured := CaptureOutput("\x1b[33mpass\x1b[0mword=" + value + "\n")
	if captured.Text != "password="+mask("secret_field")+"\n" {
		t.Fatalf("색 코드를 지운 뒤에 가려야 한다: %q", captured.Text)
	}
}

// ---- 6. 흘려 받기 — 글자 · 비밀값이 쓰기 사이에서 나뉘어도 ----

func TestCaptureJoinsCharactersSplitAcrossWrites(t *testing.T) {
	input := "한글 출력 🙂 끝\r\n다음 줄\n"
	capture := NewOutputCapture()
	for index := range len(input) {
		if written, err := capture.Write([]byte{input[index]}); err != nil || written != 1 {
			t.Fatalf("쓰기는 늘 받은 만큼 받는다: %d %v", written, err)
		}
	}
	if captured := capture.Finish(); captured.Text != "한글 출력 🙂 끝\n다음 줄\n" {
		t.Fatalf("나뉘어 들어온 글자가 깨졌다: %q", captured.Text)
	}
}

func TestCaptureRedactsSecretSplitAcrossWrites(t *testing.T) {
	token := fakeGithubToken(30)
	line := "using " + token + " for fetch\n"
	capture := NewOutputCapture()
	middle := strings.Index(line, token) + len(token)/2
	capture.Write([]byte(line[:middle]))
	capture.Write([]byte(line[middle:]))
	if captured := capture.Finish(); captured.Text != "using "+mask("token")+" for fetch\n" {
		t.Fatalf("쓰기 사이에서 나뉜 비밀값을 가리지 못했다: %q", captured.Text)
	}
}

func TestCaptureRedactsSecretAtLongLinePieceBoundary(t *testing.T) {
	// 줄바꿈 없는 100KB 한 줄 — 64KB 조각의 경계(65536)에 토큰이 걸친다. 전체가 상한 밑이라 모두 남는다.
	token := fakeGithubToken(34)
	var line strings.Builder
	for line.Len() < 65536-20 {
		line.WriteString("abcde ")
	}
	line.WriteString(token)
	for line.Len() < 100_000 {
		line.WriteString(" abcde")
	}
	captured := CaptureOutput(line.String())
	requireNoFragment(t, captured.Text, token)
	if !strings.Contains(captured.Text, mask("token")) || captured.Truncated {
		t.Fatalf("조각의 경계에 걸친 토큰을 가려야 한다(잘리지 않고): truncated=%v", captured.Truncated)
	}
}

// ---- 7. 크기 상한 ----

// splitAtMarker — 잘린 글을 앞 · 표시 줄의 N · 끝으로 나눈다. raw 는 가릴 것이 없는 원래 글(가린 뒤 = 원래).
var markerLine = regexp.MustCompile(`\[가운데 ([0-9]+)바이트를 버렸다\]\n`)

func splitAtMarker(t *testing.T, text, raw string) (head string, omitted int, tail string) {
	t.Helper()
	locations := markerLine.FindAllStringSubmatchIndex(text, -1)
	if len(locations) != 1 {
		t.Fatalf("표시 줄은 정확히 하나여야 한다: %d개", len(locations))
	}
	location := locations[0]
	head, tail = text[:location[0]], text[location[1]:]
	fmt.Sscan(text[location[2]:location[3]], &omitted)
	if !strings.HasSuffix(head, "\n") && head != "" {
		t.Fatalf("표시 줄은 줄의 처음에 있어야 한다: 앞의 끝 %q", head[max(0, len(head)-20):])
	}
	if !strings.HasPrefix(raw, head) { // 앞이 줄의 중간에서 끝났으면 표시 줄 앞에 줄바꿈 하나를 더 넣었다
		head = strings.TrimSuffix(head, "\n")
		if !strings.HasPrefix(raw, head) {
			t.Fatal("앞은 원래 글의 처음이어야 한다")
		}
	}
	if !strings.HasSuffix(raw, tail) {
		t.Fatal("끝은 원래 글의 끝이어야 한다")
	}
	if len(head)+omitted+len(tail) != len(raw) {
		t.Fatalf("앞 %d + 버린 %d + 끝 %d ≠ 원래 %d", len(head), omitted, len(tail), len(raw))
	}
	return head, omitted, tail
}

func numberedLines(totalBytes int) string {
	var builder strings.Builder
	for number := 0; builder.Len() < totalBytes; number++ {
		fmt.Fprintf(&builder, "line %07d step output ok\n", number)
	}
	return builder.String()
}

func TestCaptureUnderLimitKeepsEverything(t *testing.T) {
	raw := numberedLines(FailureOutputLimit - 100)
	captured := CaptureOutput(raw)
	if captured.Text != raw || captured.Truncated || captured.OmittedBytes != 0 {
		t.Fatalf("상한 밑은 그대로 남아야 한다: truncated=%v omitted=%d len=%d", captured.Truncated, captured.OmittedBytes, len(captured.Text))
	}
	exact := strings.Repeat("x", FailureOutputLimit-1) + "\n" // 정확히 상한
	if captured := CaptureOutput(exact); captured.Text != exact || captured.Truncated {
		t.Fatalf("정확히 상한이면 자르지 않는다: truncated=%v len=%d", captured.Truncated, len(captured.Text))
	}
}

func TestCaptureOverLimitKeepsHeadAndTailOnLineBoundaries(t *testing.T) {
	raw := numberedLines(1 << 20)
	captured := CaptureOutput(raw)
	if !captured.Truncated || len(captured.Text) > FailureOutputLimit {
		t.Fatalf("1MB 는 상한 안으로 잘려야 한다: truncated=%v len=%d", captured.Truncated, len(captured.Text))
	}
	head, omitted, tail := splitAtMarker(t, captured.Text, raw)
	if len(head) > FailureOutputHeadLimit || !strings.HasSuffix(head, "\n") || len(head) < FailureOutputHeadLimit-64 {
		t.Fatalf("앞은 32KB 안의 마지막 줄 끝까지: %d바이트", len(head))
	}
	if !strings.HasSuffix(raw[:len(raw)-len(tail)], "\n") {
		t.Fatal("끝은 줄의 처음에서 시작해야 한다")
	}
	if int64(omitted) != captured.OmittedBytes {
		t.Fatalf("표시 줄의 N(%d) 과 OmittedBytes(%d) 가 다르다", omitted, captured.OmittedBytes)
	}
	if len(captured.Text) < FailureOutputLimit-128 {
		t.Fatalf("끝의 몫을 다 쓰지 않았다: %d바이트", len(captured.Text))
	}
}

func TestCaptureCutsLongLineOnCharacterBoundary(t *testing.T) {
	for name, unit := range map[string]string{"한글": "가나다라마바사", "이모지": "🙂🚀✅", "섞임": "a한🙂"} {
		t.Run(name, func(t *testing.T) {
			raw := strings.Repeat(unit, (1<<20)/len(unit)) // 줄바꿈 없는 1MB 한 줄
			captured := CaptureOutput(raw)
			if !utf8.ValidString(captured.Text) || strings.ContainsRune(captured.Text, utf8.RuneError) {
				t.Fatal("글자의 중간에서 잘렸다(깨진 바이트 · U+FFFD)")
			}
			if len(captured.Text) > FailureOutputLimit || !captured.Truncated {
				t.Fatalf("상한 안으로 잘려야 한다: %d", len(captured.Text))
			}
			head, _, _ := splitAtMarker(t, captured.Text, raw)
			if len(head) > FailureOutputHeadLimit || len(head) < FailureOutputHeadLimit-4 {
				t.Fatalf("줄바꿈이 없으면 앞은 32KB 의 글자 경계까지: %d", len(head))
			}
		})
	}
}

func TestCaptureRedactsBeforeCutting(t *testing.T) {
	// 줄바꿈 없이 토큰만 빽빽이 — 앞 · 끝의 자르는 자리는 거의 늘 토큰 안에 떨어진다. 자른 뒤에 가리면 반쪽이 남는다.
	var builder strings.Builder
	for seed := int64(100); builder.Len() < 1_200_000; seed++ {
		builder.WriteString(fakeGithubToken(seed))
		builder.WriteByte(' ')
	}
	captured := CaptureOutput(builder.String())
	if !captured.Truncated {
		t.Fatal("상한을 넘는 입력이다")
	}
	if run := longestRun(captured.Text); len(run) >= 12 {
		t.Fatalf("토큰의 조각이 남았다: %q", run)
	}
	if !strings.Contains(captured.Text, mask("token")) {
		t.Fatal("가린 자리가 없다")
	}
}

func TestCaptureBinaryInputBecomesValidText(t *testing.T) {
	generator := rand.New(rand.NewSource(35))
	raw := make([]byte, 1<<20)
	generator.Read(raw)
	captured := CaptureOutput(string(raw))
	if !utf8.ValidString(captured.Text) || len(captured.Text) > FailureOutputLimit {
		t.Fatalf("바이너리 입력은 상한 안의 올바른 UTF-8 이어야 한다: valid=%v len=%d", utf8.ValidString(captured.Text), len(captured.Text))
	}
	if !captured.Truncated || len(markerLine.FindAllString(captured.Text, -1)) != 1 { // 바꾼 글자(U+FFFD 는 3바이트)로 1MB 가 상한을 넘는다
		t.Fatalf("1MB 바이너리는 잘려야 한다: truncated=%v", captured.Truncated)
	}
	for _, character := range captured.Text {
		if (character < 0x20 && character != '\t' && character != '\n') || character == 0x7f {
			t.Fatalf("제어 문자 %U 가 남았다", character)
		}
	}
}

func TestCaptureStreamsLargeOutputWithBoundedMemory(t *testing.T) {
	if testing.Short() {
		t.Skip("큰 출력(96MB)을 흘려 보는 시험 — -short 에서는 건너뛴다")
	}
	chunk := []byte(numberedLines(1 << 20)) // 1MB 조각을 되풀이해 96MB — 통째로 들면 문턱(32MB)을 크게 넘는다
	capture := NewOutputCapture()
	runtime.GC()
	var baseline, sample runtime.MemStats
	runtime.ReadMemStats(&baseline)
	peak := uint64(0)
	for index := range 96 {
		if _, err := capture.Write(chunk); err != nil {
			t.Fatal(err)
		}
		if index%16 == 15 {
			runtime.GC()
			runtime.ReadMemStats(&sample)
			peak = max(peak, sample.HeapAlloc)
		}
	}
	captured := capture.Finish()
	if grown := int64(peak) - int64(baseline.HeapAlloc); grown > 32<<20 {
		t.Fatalf("흘려 받는 동안 붙잡은 메모리가 %dMB 다 — 출력을 통째로 들고 있다", grown>>20)
	}
	if !captured.Truncated || len(captured.Text) > FailureOutputLimit || captured.OmittedBytes < 95<<20 {
		t.Fatalf("96MB 는 상한 안으로 잘려야 한다: len=%d omitted=%d", len(captured.Text), captured.OmittedBytes)
	}
	if !bytes.HasSuffix([]byte(captured.Text), chunk[len(chunk)-1000:]) {
		t.Fatal("끝은 마지막 출력이어야 한다")
	}
}
