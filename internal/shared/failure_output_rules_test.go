package shared

// 가림 규칙의 보강(둘째) — 기준: requirement-spec.md(2026-11-25 admin-incident-assist) §18 의 3–9.
//
// 정한 계약:
//   - secret_key · secretKey · secret-key · SECRET_KEY_BASE 꼴의 이름도 비밀 칸이다(secret_key_ref 는 남긴다).
//   - 겹친 맞음은 모두 가린다(합쳐도 된다). 다시 거치면 가림이 늘 수는 있어도 줄지 않고 가린 값이 되살아나지 않는다(requireNeverUnmasks).
//   - 닫는 따옴표가 없는 값은 줄 끝까지 가린다. `docker login -p "…"` 의 따옴표 값은 통째로.
//   - 다이제스트의 예외는 `sha256:`(· sha384 · sha512) 앞이 낱말 글자(영문자 · 숫자 · _)가 아닐 때만.
//   - 이어 붙은 토큰은 허용 글자가 이어지는 끝까지 한 토큰이다.
//   - XML `<이름>값</이름>`(같은 줄) — 이름이 비밀 낱말로 끝나면 값을 가린다.
//   - 혼자 선 `pwd`(대소문자 무관)는 값이 `/` 로 시작하면 남긴다. 앞 마디가 있는 `…_PWD` 는 `/` 로 시작해도 가린다.

import (
	"math/rand"
	"strings"
	"testing"
)

func TestSecretKeyNamesAreRedacted(t *testing.T) {
	value := "Zq81" + fakeCharacters(90, 20, alphanumeric)
	field := mask("secret_field")
	for _, testCase := range []struct{ input, want string }{
		{"secretKey=" + value, "secretKey=" + field},
		{`{"secretKey":"` + value + `","region":"x"}`, `{"secretKey":"` + field + `","region":"x"}`},
		{"SECRET_KEY=" + value, "SECRET_KEY=" + field},
		{"JWT_SECRET_KEY=" + value, "JWT_SECRET_KEY=" + field},
		{"SECRET_KEY_BASE=" + value, "SECRET_KEY_BASE=" + field},
		{"secret-key: " + value, "secret-key: " + field},
		{"secret_key_ref: db-creds", "secret_key_ref: db-creds"},
	} {
		requireCaptured(t, testCase.input, testCase.want)
	}
}

func TestOverlappingMatchesAreAllRedacted(t *testing.T) {
	cases := []struct{ name, input string }{
		{"긴 옵션과 -p", "docker login --password=Zq81alpha -p Zq81beta registry.example.com"},
		{"-p 뒤의 비밀 칸", "docker login -p \t password\t:Zq81x"},
		{"칸 안의 docker login", "token:docker login docker login -p Zq81o"},
		{"주소에 붙은 토큰", "10.0.0.1" + "gh" + "p_" + "Zq81" + fakeCharacters(91, 32, alphanumeric)},
	}
	for _, testCase := range cases {
		first := CaptureOutput(testCase.input)
		second := CaptureOutput(first.Text)
		requireNeverUnmasks(t, first.Text, second.Text)
		if strings.Contains(second.Text, "Zq81") || strings.Contains(second.Text, "10.0.0.1") {
			t.Fatalf("%s: 겹친 맞음의 값이 남았다(master 가 한 번 더 거친 뒤에도):\n first %q\nsecond %q", testCase.name, first.Text, second.Text)
		}
		if testCase.name != "주소에 붙은 토큰" && strings.Contains(first.Text, "Zq81") { // 한 번에 다 가려져야 하는 꼴
			t.Fatalf("%s: 한 번 거친 글에 값이 남았다: %q", testCase.name, first.Text)
		}
	}
	requireCaptured(t, "docker login --password=Zq81alpha -p Zq81beta registry.example.com",
		"docker login --password="+mask("secret_field")+" -p "+mask("secret_field")+" registry.example.com")
}

// 무작위 입력 — 다시 거쳐도 가린 값이 되살아나지 않는다.
func TestRandomInputsNeverUnmaskOnSecondPass(t *testing.T) {
	pieces := []string{
		"password", "token", "secret_key", "pwd", "PASS", "=", ":", " ", "\t", `"`, "'", `\`, "-p ", "docker login ", "--password=",
		"Authorization: Bearer ", "gh" + "p_Zq81" + strings.Repeat("a", 32), "s" + "k-Zq81" + strings.Repeat("b", 30), "10.0.0.1", "fd00::1",
		"ops@example.com", "[가림:token]", "[가림:x]", "[가림:secret_field]", "sha256:" + strings.Repeat("c", 64), "\n", "<password>", "</password>",
		"Zq81Leak", "x", "https://u:Zq81p@h.example.com/", ",", "{", "}", "0x" + strings.Repeat("d", 64), "AI" + "za", "xo" + "xb-",
	}
	generator := rand.New(rand.NewSource(92))
	for round := range 400 {
		var builder strings.Builder
		for range 1 + generator.Intn(60) {
			builder.WriteString(pieces[generator.Intn(len(pieces))])
		}
		first := CaptureOutput(builder.String())
		second := CaptureOutput(first.Text)
		if t.Failed() {
			return
		}
		requireNeverUnmasks(t, first.Text, second.Text)
		_ = round
	}
}

func TestUnterminatedAndQuotedValuesAreRedactedWhole(t *testing.T) {
	field := mask("secret_field")
	for _, testCase := range []struct{ input, want string }{
		{`password="abc Zq81 unterminated tail`, `password="` + field},
		{`secret='abc Zq81 def`, `secret='` + field},
		{`{"token":"Zq81 open` + "\n" + `next line`, `{"token":"` + field + "\n" + `next line`},
		{`docker login -p "alpha Zq81 beta" registry.example.com`, `docker login -p "` + field + `" registry.example.com`},
		{`docker login -p 'alpha Zq81 beta' registry.example.com`, `docker login -p '` + field + `' registry.example.com`},
	} {
		requireCaptured(t, testCase.input, testCase.want)
	}
}

func TestDigestExceptionNeedsWordBoundaryBeforeName(t *testing.T) {
	digest := fakeCharacters(93, 64, lowerHex)
	for _, testCase := range []struct{ input, want string }{
		{"notsha256:" + digest, "notsha256:" + mask("hex")},
		{"my_sha256:" + digest, "my_sha256:" + mask("hex")},
		{"x9sha512:" + fakeCharacters(94, 128, lowerHex), "x9sha512:" + mask("hex")},
		{"image@sha256:" + digest, "image@sha256:" + digest},
		{" sha256:" + digest, " sha256:" + digest},
		{"(sha256:" + digest + ")", "(sha256:" + digest + ")"},
		{"sha256:" + digest, "sha256:" + digest},
	} {
		requireCaptured(t, testCase.input, testCase.want)
	}
}

func TestJoinedTokensAreRedactedAsOne(t *testing.T) {
	github := "gh" + "p_" + "Zq81" + fakeCharacters(95, 32, alphanumeric)
	key := "s" + "k-" + "Zq81" + fakeCharacters(96, 30, alphanumeric)
	requireCaptured(t, "a "+github+github+" b", "a "+mask("token")+" b")
	requireCaptured(t, "a "+key+key+" b", "a "+mask("token")+" b")
}

func TestXmlSecretElementsAreRedacted(t *testing.T) {
	field := mask("secret_field")
	for _, testCase := range []struct{ input, want string }{
		{"<password>Zq81LeakXml</password>", "<password>" + field + "</password>"},
		{"  <apiToken>Zq81abc</apiToken>", "  <apiToken>" + field + "</apiToken>"},
		{"<server><id>nexus</id><password>Zq81pw</password></server>", "<server><id>nexus</id><password>" + field + "</password></server>"},
		{"<passwordPolicy>strict</passwordPolicy>", "<passwordPolicy>strict</passwordPolicy>"},
		{"<tokens>12</tokens>", "<tokens>12</tokens>"},
		{"<password></password>", "<password></password>"},
	} {
		requireCaptured(t, testCase.input, testCase.want)
	}
}

func TestPwdKeepsPathsOnlyWhenStandingAlone(t *testing.T) {
	field := mask("secret_field")
	for _, testCase := range []struct{ input, want string }{
		{"PWD=/app", "PWD=/app"},
		{"pwd: /home/ci/build", "pwd: /home/ci/build"},
		{"pwd=Zq81secret", "pwd=" + field},
		{"DB_PWD=/notapath", "DB_PWD=" + field},
		{"db.pwd: /Zq81", "db.pwd: " + field},
	} {
		requireCaptured(t, testCase.input, testCase.want)
	}
}
