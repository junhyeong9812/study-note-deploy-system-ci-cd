package shared

// 긴 한 줄 — 기준: requirement-spec.md(2026-11-25 admin-incident-assist) §18 의 1.
//
// 정한 계약:
//   - 한 줄(줄바꿈을 빼고)이 1MB(1<<20 바이트) 이하면 조각으로 나누지 않고 통째로 가린다 — 줄 안의 어느 자리의 비밀값도 가려진다.
//   - 1MB 를 넘는 한 줄은 내용 없이 `[긴 한 줄 N바이트를 버렸다]` 로 바뀐다. N = 그 줄의 바이트 수(줄바꿈을 빼고 — 시험은 ASCII 만 쓴다).
//     줄바꿈은 입력대로 남는다(마지막 줄에 줄바꿈이 없으면 표시 뒤에도 없다). 앞뒤의 줄은 그대로.
//     Truncated 는 참, OmittedBytes 에는 N 이 더해진다(256KB 상한에서 가운데를 버린 바이트와 합쳐서).
//   - 흘려 받는 동안 줄을 모으다 1MB 를 넘으면 더 모으지 않고 바이트만 센다(줄바꿈 없는 96MB 도 힙 32MB 미만).
//   - 쓰기가 어디서 나뉘든(1바이트씩도) 결과는 한 번에 쓴 것과 같다.

import (
	"fmt"
	"math/rand"
	"runtime"
	"strings"
	"testing"
)

const oneMegabyte = 1 << 20

func longLineMarker(length int) string {
	return fmt.Sprintf("[긴 한 줄 %d바이트를 버렸다]", length)
}

type lineSecret struct{ name, secret, masked, kind string }

func longLineSecrets() []lineSecret {
	privateKeyBody := fakeCharacters(70, 64, alphanumeric+"+/") + `\n` + fakeCharacters(71, 64, alphanumeric+"+/") + `\n` + "Zq81" + fakeCharacters(72, 40, alphanumeric)
	var quoted strings.Builder
	for seed := int64(73); quoted.Len() < 12<<10; seed++ {
		quoted.WriteString("Zq81" + fakeCharacters(seed, 20, alphanumeric) + " ")
	}
	return []lineSecret{
		{"GitHub 토큰", "gh" + "p_" + "Zq81" + strings.Repeat("R", 32), mask("token"), "token"},
		{"sk- 키", "s" + "k-" + "Zq81" + strings.Repeat("A", 40), mask("token"), "token"},
		{"JSON 비밀 칸", `{"password":"Zq81LeakBoundary"}`, `{"password":"` + mask("secret_field") + `"}`, "secret_field"},
		{"한 줄 PEM", `"key":"-----BEGIN ` + `RSA PRIVATE KEY-----\n` + privateKeyBody + `\n-----END ` + `RSA PRIVATE KEY-----"`, `"key":"` + mask("pem") + `"`, "pem"},
		{"12KB 따옴표 값", `password="` + quoted.String() + `"`, `password="` + mask("secret_field") + `"`, "secret_field"},
		{"80KB 값", "token=Zq81" + fakeCharacters(74, 80<<10, alphanumeric), "token=" + mask("secret_field"), "secret_field"},
	}
}

func TestSecretAnywhereInALongLineIsRedacted(t *testing.T) {
	for _, size := range []int{64 << 10, 200_000, oneMegabyte} {
		for _, secret := range longLineSecrets() {
			if len(secret.secret)+2 > size {
				continue
			}
			positions := map[string]int{"처음": 0, "가운데": size/2 - len(secret.secret)/2, "끝": size - len(secret.secret)}
			if size < oneMegabyte { // 1MB 는 시간 때문에 세 자리만
				positions["64KB 앞에 걸침"] = formerPieceBoundary - len(secret.secret)/2
				positions["64KB 바로 뒤"] = formerPieceBoundary + 7
			}
			for where, position := range positions {
				position = max(0, min(position, size-len(secret.secret)))
				before := strings.Repeat(",", position)
				after := strings.Repeat(",", size-position-len(secret.secret))
				line := before + secret.secret + after
				captured := CaptureOutput(line)
				label := fmt.Sprintf("%d바이트 줄 · %s · %s(%d)", size, secret.name, where, position)
				if strings.Contains(captured.Text, "Zq81") || len(longestRun(captured.Text)) >= 12 || captured.Redactions[secret.kind] < 1 {
					t.Fatalf("%s: 비밀값이 남았다(남은 덩어리 %q · 가린 %v)", label, longestRun(captured.Text), captured.Redactions)
				}
				if strings.Contains(captured.Text, "[긴 한 줄") {
					t.Fatalf("%s: 1MB 이하의 줄을 버렸다", label)
				}
				if want := before + secret.masked + after; len(want) <= FailureOutputLimit && captured.Text != want {
					t.Fatalf("%s: 가린 결과가 다르다(길이 %d, 기대 %d)", label, len(captured.Text), len(want))
				}
			}
		}
	}
}

func TestLineOfExactlyOneMegabyteIsKeptNotReplaced(t *testing.T) {
	line := strings.Repeat("abcXYZ019,", oneMegabyte/10+1)[:oneMegabyte]
	captured := CaptureOutput("first\n" + line)
	head, omitted, tail := splitAtMarker(t, captured.Text, "first\n"+line)
	if head != "first\n" || !strings.HasSuffix(line, tail) || len(tail) < 200_000 || strings.Contains(captured.Text, "[긴 한 줄") {
		t.Fatalf("정확히 1MB 인 줄은 버리지 않는다 — 앞 줄과 그 줄의 끝이 남는다: 앞 %q · 끝 %d바이트", head, len(tail))
	}
	if int64(omitted) != captured.OmittedBytes || !captured.Truncated {
		t.Fatalf("가운데를 버린 수가 다르다: %d / %d", omitted, captured.OmittedBytes)
	}
}

func TestLineOverOneMegabyteBecomesMarkerLine(t *testing.T) {
	cases := []struct {
		name, input, want string
		omitted           int64
	}{
		{"1MB + 1", "before line\n" + strings.Repeat("a", oneMegabyte+1) + "\nafter line\n",
			"before line\n" + longLineMarker(oneMegabyte+1) + "\nafter line\n", oneMegabyte + 1},
		{"비밀값이 든 2MB", "before\n" + strings.Repeat(",", oneMegabyte) + "password=Zq81LeakLong" + strings.Repeat(",", oneMegabyte) + "\r\nafter\n",
			"before\n" + longLineMarker(2*oneMegabyte+21) + "\nafter\n", 2*oneMegabyte + 21},
		{"줄바꿈 없는 마지막 줄", "x\n" + strings.Repeat("y", oneMegabyte+5), "x\n" + longLineMarker(oneMegabyte+5), oneMegabyte + 5},
		{"긴 줄 둘", strings.Repeat("p", oneMegabyte+1) + "\n" + strings.Repeat("q", oneMegabyte+2) + "\n",
			longLineMarker(oneMegabyte+1) + "\n" + longLineMarker(oneMegabyte+2) + "\n", 2*oneMegabyte + 3},
	}
	for _, testCase := range cases {
		captured := CaptureOutput(testCase.input)
		if captured.Text != testCase.want || !captured.Truncated || captured.OmittedBytes != testCase.omitted {
			t.Fatalf("%s: 1MB 를 넘는 줄은 표시 한 줄로:\n got %q (truncated=%v omitted=%d)\nwant %q (omitted=%d)",
				testCase.name, captured.Text[:min(200, len(captured.Text))], captured.Truncated, captured.OmittedBytes, testCase.want, testCase.omitted)
		}
	}
}

func TestOmittedBytesAddLongLineAndMiddle(t *testing.T) {
	front := numberedLines(600 << 10)
	input := front + strings.Repeat("z", 2*oneMegabyte) + "\ntail line\n"
	captured := CaptureOutput(input)
	location := markerLine.FindStringSubmatch(captured.Text)
	if location == nil || !strings.HasSuffix(captured.Text, longLineMarker(2*oneMegabyte)+"\ntail line\n") {
		t.Fatalf("긴 줄의 표시와 가운데의 표시가 함께 있어야 한다: 끝 %q", captured.Text[max(0, len(captured.Text)-120):])
	}
	var middle int64
	fmt.Sscan(location[1], &middle)
	if captured.OmittedBytes != middle+2*oneMegabyte {
		t.Fatalf("버린 바이트는 가운데(%d) + 긴 줄(%d) 이어야 한다: %d", middle, 2*oneMegabyte, captured.OmittedBytes)
	}
}

func TestLongLineWithoutNewlineIsCountedNotCollected(t *testing.T) {
	chunk := []byte(strings.Repeat("n", oneMegabyte))
	capture := NewOutputCapture()
	runtime.GC()
	var baseline, sample runtime.MemStats
	runtime.ReadMemStats(&baseline)
	peak := uint64(0)
	for index := range 96 {
		capture.Write(chunk)
		if index%16 == 15 {
			runtime.GC()
			runtime.ReadMemStats(&sample)
			peak = max(peak, sample.HeapAlloc)
		}
	}
	captured := capture.Finish()
	if grown := int64(peak) - int64(baseline.HeapAlloc); grown > 32<<20 {
		t.Fatalf("줄바꿈 없는 96MB 를 받는 동안 붙잡은 메모리가 %dMB 다 — 1MB 를 넘은 줄을 계속 모은다", grown>>20)
	}
	if captured.Text != longLineMarker(96*oneMegabyte) || captured.OmittedBytes != 96*oneMegabyte || !captured.Truncated {
		t.Fatalf("줄바꿈 없는 96MB 는 표시 하나: %q omitted=%d", captured.Text[:min(100, len(captured.Text))], captured.OmittedBytes)
	}
}

func writeInPieces(input string, sizes func() int) CapturedOutput {
	capture := NewOutputCapture()
	for offset := 0; offset < len(input); {
		size := min(sizes(), len(input)-offset)
		capture.Write([]byte(input[offset : offset+size]))
		offset += size
	}
	return capture.Finish()
}

func TestSplitWritesGiveTheSameResult(t *testing.T) {
	secrets := longLineSecrets()
	longLine := strings.Repeat(",", 30_000) + secrets[2].secret + strings.Repeat(",", 30_000) + secrets[0].secret + strings.Repeat(",", 10_000)
	small := "head line\n" + longLine + "\nmid " + secrets[3].secret + "\n한글 🙂 줄\r\nend"
	big := small + "\n" + strings.Repeat("o", oneMegabyte+10) + "\n" + longLine + "\n"
	for name, input := range map[string]string{"짧은 묶음": small, "긴 줄이 든 묶음": big} {
		want := CaptureOutput(input)
		generator := rand.New(rand.NewSource(80))
		for round := range 5 {
			got := writeInPieces(input, func() int { return 1 + generator.Intn(9000) })
			if got.Text != want.Text || got.OmittedBytes != want.OmittedBytes {
				t.Fatalf("%s: 무작위로 나눈 쓰기(%d회째)의 결과가 한 번에 쓴 것과 다르다", name, round)
			}
		}
		for _, split := range []int{1, 10, len(input) / 2, len(input) - 3} {
			capture := NewOutputCapture()
			capture.Write([]byte(input[:split]))
			capture.Write([]byte(input[split:]))
			if got := capture.Finish(); got.Text != want.Text {
				t.Fatalf("%s: %d 에서 나눈 쓰기의 결과가 다르다", name, split)
			}
		}
	}
	if got := writeInPieces(small, func() int { return 1 }); got.Text != CaptureOutput(small).Text {
		t.Fatal("1바이트씩 쓴 결과가 한 번에 쓴 것과 다르다")
	}
}
