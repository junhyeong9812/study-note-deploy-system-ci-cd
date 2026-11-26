package shared

// 가림 규칙의 보강 — 기준: requirement-spec.md(2026-11-25 admin-incident-assist) §17 의 2 · 5 · 6 · 7 · 8.
//
// 정한 계약:
//   - 표시 뒤의 나머지: 값 전체가 정확히 표시 하나(`[가림:<종류>]`)일 때만 건너뛴다. 표시로 시작하지만 뒤에 글이 붙은 값은 값 전체를 가린다.
//   - 따옴표 안의 값은 역빗금 이스케이프(`\"` · `\'` · `\\`)를 따라 닫는 따옴표까지.
//   - 다이제스트의 예외: `sha256:` 뒤 정확히 64자 · `sha384:` 96 · `sha512:` 128 일 때만. 그 밖의 길이 · 이름은 16진 규칙(41자 이상 가림).
//   - 비밀 칸의 이름: 소문자로 바꾼 이름이 비밀 낱말(password · passwd · secret · token · api_key · apikey · api-key · access_key ·
//     private_key 와 그 이음 꼴)로 끝날 때만. 그리고 이름의 끝 마디가 pass(앞에 다른 마디가 있을 때) · pwd 이고 값이 숫자만이 아닐 때.
//     한글 이름 비밀번호 · 패스워드 · 암호. 이름과 구분자 사이의 U+FFFD(정리된 제어 문자)는 건너뛰고 맞춘다.
//   - 긴 옵션 `--<비밀 낱말로 끝나는 이름> <값>` · `--<이름>=<값>` · `docker login … -p <값>` 의 값을 가린다(값이 없는 `--password-stdin` 은 그대로).
//   - 토큰 꼴을 더한다: `xox[abprs]-…` · `AIza` + 35자 · `npm_` + 36자 · `hooks.slack.com/services/…` 의 경로 · `0x` + 64자 16진.

import (
	"strings"
	"testing"
)

func requireCaptured(t *testing.T, input, want string) CapturedOutput {
	t.Helper()
	captured := CaptureOutput(input)
	if captured.Text != want {
		t.Fatalf("가린 결과가 다르다:\n  input %q\n    got %q\n   want %q", input, captured.Text, want)
	}
	return captured
}

// ---- 2. 표시 뒤의 나머지 ----

func TestMaskFollowedByLeftoverIsRedactedWhole(t *testing.T) {
	cases := []struct{ input, want string }{
		{"DB_PASSWORD=" + "gh" + "p_" + fakeCharacters(50, 36, alphanumeric) + "!Zq81LeakC", "DB_PASSWORD=" + mask("secret_field")},
		{"token=" + "s" + "k-" + fakeCharacters(51, 24, alphanumeric) + ".Zq81LeakD", "token=" + mask("secret_field")},
		{"password=[가림:x]Zq81LeakA", "password=" + mask("secret_field")},
		{"Authorization: Bearer [가림:x]Zq81LeakB", "Authorization: Bearer " + mask("authorization")},
		{"password=[가림:secret_field]Zq81LeakI", "password=" + mask("secret_field")},
		{`{"token":"[가림:token]Zq81LeakJ"}`, `{"token":"` + mask("secret_field") + `"}`},
	}
	for _, testCase := range cases {
		captured := requireCaptured(t, testCase.input, testCase.want)
		if again := CaptureOutput(captured.Text); again.Text != captured.Text || len(nonZero(again.Redactions)) != 0 {
			t.Fatalf("한 번 더 거치면 바뀌지 않고 세지 않는다: %q → %q %v", captured.Text, again.Text, again.Redactions)
		}
	}
}

func TestValueThatIsExactlyOneMaskIsLeftAlone(t *testing.T) {
	for _, input := range []string{
		"password=" + mask("secret_field"),
		"Authorization: Bearer " + mask("authorization"),
		`{"token":"` + mask("token") + `","user":"ci"}`,
		"https://" + mask("url_credential") + "@git.example.com/x.git",
	} {
		if captured := requireCaptured(t, input, input); len(nonZero(captured.Redactions)) != 0 {
			t.Fatalf("이미 가린 값을 다시 셌다: %q %v", input, captured.Redactions)
		}
	}
}

func nonZero(counts map[string]int) map[string]int {
	kept := map[string]int{}
	for kind, count := range counts {
		if count != 0 {
			kept[kind] = count
		}
	}
	return kept
}

// ---- 5. 이스케이프된 따옴표 ----

func TestQuotedValueFollowsBackslashEscapes(t *testing.T) {
	field := mask("secret_field")
	cases := []struct{ input, want string }{
		{`{"password":"abc\"def","user":"ci"}`, `{"password":"` + field + `","user":"ci"}`},
		{`password="a\"b c" next=1`, `password="` + field + `" next=1`},
		{`'secret': 'it\'s here', 'scope': 'read'`, `'secret': '` + field + `', 'scope': 'read'`},
		{`{"token":"ab\\","user":"ci"}`, `{"token":"` + field + `","user":"ci"}`},         // 이스케이프된 역빗금 뒤의 따옴표가 닫는다
		{`{"password":"x\\\"y","user":"ci"}`, `{"password":"` + field + `","user":"ci"}`}, // \\ 다음 \" — 값은 이어진다
	}
	for _, testCase := range cases {
		captured := requireCaptured(t, testCase.input, testCase.want)
		if strings.Contains(captured.Text, "def") || strings.Contains(captured.Text, "here") {
			t.Fatalf("이스케이프 뒤의 값이 남았다: %q", captured.Text)
		}
	}
}

// ---- 6. 다이제스트 ----

func TestDigestExceptionRequiresExactLength(t *testing.T) {
	hex := func(length int) string { return fakeCharacters(int64(60+length), length, lowerHex) }
	cases := []struct{ input, want string }{
		{"sha256:" + hex(64), "sha256:" + hex(64)},
		{"sha256:" + hex(65), "sha256:" + mask("hex")},
		{"sha256:" + hex(63), "sha256:" + mask("hex")},
		{"sha384:" + hex(96), "sha384:" + hex(96)},
		{"sha384:" + hex(95), "sha384:" + mask("hex")},
		{"sha512:" + hex(128), "sha512:" + hex(128)},
		{"sha512:" + hex(127), "sha512:" + mask("hex")},
		{"sha512:" + hex(129), "sha512:" + mask("hex")},
		{"sha1:" + hex(41), "sha1:" + mask("hex")},
		{"sha1:" + hex(64), "sha1:" + mask("hex")},
		{"sha224:" + hex(56), "sha224:" + mask("hex")},
		{"SHA256:" + strings.ToUpper(hex(64)), "SHA256:" + strings.ToUpper(hex(64))},
		{"md5:" + hex(64), "md5:" + mask("hex")},
	}
	for _, testCase := range cases {
		requireCaptured(t, testCase.input, testCase.want)
	}
}

// ---- 7. 이름이 비밀 낱말로 끝날 때만 ----

func TestSecretFieldNameMustEndWithSecretWord(t *testing.T) {
	for _, kept := range []string{
		"max_tokens=1024", "token_count: 42", "tokenizer: bert", "TOKEN_TYPE=bearer", "secret_key_ref: db-creds",
		"password_policy=strict", `{"max_tokens":512,"tokens_used":40}`, "secrets_manager: enabled", "tokenCount=7",
	} {
		requireCaptured(t, kept, kept)
	}
	value := fakeCharacters(52, 20, alphanumeric)
	field := mask("secret_field")
	for _, name := range []string{"api_token", "DB_PASSWORD", "client_secret", "apiKey", "X-Api-Key", "AWS_SECRET_ACCESS_KEY", "token", "Secret",
		"github.token", "private_key", "db-passwd"} {
		requireCaptured(t, name+"="+value, name+"="+field)
		requireCaptured(t, name+": "+value, name+": "+field)
	}
}

// ---- 8. 더 잡는 꼴 ----

func TestMoreSecretShapesAreRedacted(t *testing.T) {
	value := fakeCharacters(53, 18, alphanumeric)
	field := mask("secret_field")
	cases := []struct{ input, want string }{
		{"run --password " + value + " --verbose", "run --password " + field + " --verbose"},
		{"run --password=" + value + " --verbose", "run --password=" + field + " --verbose"},
		{"cli --api-token " + value, "cli --api-token " + field},
		{"cli --client-secret=" + value, "cli --client-secret=" + field},
		{"docker login -u ci -p " + value + " registry.example.com", "docker login -u ci -p " + field + " registry.example.com"},
		{"DB_PASS=" + value, "DB_PASS=" + field},
		{"smtp_pass: " + value, "smtp_pass: " + field},
		{"pwd=" + value, "pwd=" + field},
		{"DB_PWD: " + value, "DB_PWD: " + field},
		{"비밀번호: " + value, "비밀번호: " + field},
		{"패스워드=" + value, "패스워드=" + field},
		{"암호 = " + value, "암호 = " + field},
		{"password\x00=" + value, "password�=" + field}, // 이름 안의 제어 문자는 정리된 뒤에도 건너뛰어 맞춘다
		{"pass\x1b[0mword=" + value, "password=" + field},
	}
	for _, testCase := range cases {
		requireCaptured(t, testCase.input, testCase.want)
	}
}

func TestLookalikesOfNewShapesAreKept(t *testing.T) {
	for _, kept := range []string{
		"docker login --password-stdin -u ci registry.example.com", // 값이 없다
		"pass=12 fail=0 skip=1",
		"DB_PASS=12345", // 끝 마디가 pass 여도 값이 숫자만이면 남긴다(시험 수)
		"PASS: TestFoo",
		"--- PASS: TestQueue (0.01s)",
		"PASS",
		"/home/ci/work/app",
		"npm_config_cache=/x/.npm",
		"to 0x" + fakeCharacters(54, 40, lowerHex) + " sent", // 40자 주소
		"key AIzaShort here",
		"xoxb-short",
		"run --passes 3 --token-file /run/secrets/token",
		"docker run -p 8080:80 app",
	} {
		requireCaptured(t, kept, kept)
	}
}

func TestMoreTokenShapesAreRedacted(t *testing.T) {
	slackPath := "T" + fakeCharacters(55, 8, "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789") + "/B" + fakeCharacters(56, 8, "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789") + "/" + fakeCharacters(57, 24, alphanumeric)
	cases := []struct {
		name, input, secret string
		kept                []string
	}{
		{"Slack 봇 토큰", "using " + "xo" + "xb-" + "123456789012-1234567890123-" + fakeCharacters(58, 24, alphanumeric) + " now",
			"xo" + "xb-" + "123456789012-1234567890123-" + fakeCharacters(58, 24, alphanumeric), []string{"using ", " now"}},
		{"Slack 사용자 토큰", "t=" + "xo" + "xp-" + fakeCharacters(59, 40, alphanumeric+"-"), "xo" + "xp-" + fakeCharacters(59, 40, alphanumeric+"-"), []string{"t="}},
		{"Google API 키", "key " + "AI" + "za" + fakeCharacters(61, 35, alphanumeric+"-_") + " end", "AI" + "za" + fakeCharacters(61, 35, alphanumeric+"-_"), []string{"key ", " end"}},
		{"npm 토큰", "auth " + "np" + "m_" + fakeCharacters(62, 36, alphanumeric) + " end", "np" + "m_" + fakeCharacters(62, 36, alphanumeric), []string{"auth ", " end"}},
		{"Slack 웹훅", "post https://hooks.slack.com/services/" + slackPath + " done", slackPath, []string{"post https://hooks.slack.com/services/", " done"}},
		{"0x 64자", "key 0x" + fakeCharacters(63, 64, lowerHex) + " end", fakeCharacters(63, 64, lowerHex), []string{"key ", " end"}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			captured := CaptureOutput(testCase.input)
			requireNoFragment(t, captured.Text, testCase.secret)
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
