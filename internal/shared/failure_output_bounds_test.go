package shared

// 가림의 시간 상한 · 긴 한 줄 — 기준: requirement-spec.md(2026-11-25 admin-incident-assist) §17 의 1 · §18 의 1.
//
// 정한 계약:
//   - 시간: 가림은 입력에 비례하는 시간에 끝난다. 최악 입력(비밀 낱말만 되풀이한 · 구분자 없는 한 줄)도
//     16KB 1초 · 64KB 2초 · 256KB 3초 · 1MB 8초 안(넉넉한 상한 — 느린 기계를 감안했다. -race 에서는 4배. 선형이면 1MB 가 1초 안팎이다).
//     1MB 와 256KB 의 시간 비율은 8 이하(선형이면 4 언저리, 제곱이면 16). -short 에서도 돈다.
//   - 긴 한 줄(1MB 까지)은 조각으로 나누지 않고 통째로 가린다 — 자세한 것은 failure_output_long_line_test.go.

import (
	"strings"
	"testing"
	"time"
)

// captureWithin — 기한 안에 끝나면 걸린 시간, 아니면 실패(이어서 더 큰 입력을 돌리지 않게 false).
func captureWithin(t *testing.T, label string, input string, budget time.Duration) (time.Duration, bool) {
	t.Helper()
	started := time.Now()
	done := make(chan struct{})
	go func() {
		CaptureOutput(input)
		close(done)
	}()
	select {
	case <-done:
		return time.Since(started), true
	case <-time.After(budget * timeBudgetFactor):
		t.Errorf("%s: %d바이트를 가리는 데 %v 를 넘겼다 — 입력의 제곱으로 느려지는 길이 있다", label, len(input), budget*timeBudgetFactor)
		return budget, false
	}
}

func TestRedactionTimeIsLinearForWorstInputs(t *testing.T) {
	steps := []struct {
		size   int
		budget time.Duration
	}{{16 << 10, time.Second}, {64 << 10, 2 * time.Second}, {256 << 10, 3 * time.Second}, {1 << 20, 8 * time.Second}}
	for _, unit := range []string{"token-", "password", "secret_", "password=", "authorization", "@a."} {
		t.Run(strings.TrimSpace(unit), func(t *testing.T) {
			elapsed := map[int]time.Duration{}
			for _, step := range steps { // 작은 것부터 — 기한을 넘기면 더 큰 것은 돌리지 않는다
				took, finished := captureWithin(t, unit, strings.Repeat(unit, step.size/len(unit)+1)[:step.size], step.budget)
				if !finished {
					return
				}
				elapsed[step.size] = took
			}
			if small, large := elapsed[256<<10], elapsed[1<<20]; small >= 10*time.Millisecond && large > 8*small {
				t.Errorf("%q: 1MB(%v) 가 256KB(%v) 의 8배를 넘는다 — 선형이 아니다", unit, large, small)
			}
		})
	}
}

func TestRedactionTimeStaysLowForOrdinaryOutput(t *testing.T) {
	ordinary := strings.Repeat("max_tokens=1024 token_count: 42 tokenizer: bert --- PASS: TestTokenRefresh (0.00s) at 192.0.2.10 sha "+
		strings.Repeat("ab12", 10)+"\n", (1<<20)/150)
	captureWithin(t, "평범한 출력", ordinary, 5*time.Second) // 비밀 낱말이 줄마다 나오지만 칸이 아닌 1MB
	captureWithin(t, "번호 줄", numberedLines(1<<20), 5*time.Second)
}

// formerPieceBoundary — 옛 설계(§17)가 줄을 나누던 자리. 지금은 나누지 않는다 — 그 둘레의 비밀값도 가려지는지 본다.
const formerPieceBoundary = 64 << 10

func TestLongLineWithoutSecretsIsKeptByteForByte(t *testing.T) {
	for name, line := range map[string]string{
		"점":     strings.Repeat(".", 200_000),
		"글자":    strings.Repeat("abcXYZ019", 22_000),
		"한글 섞임": strings.Repeat("가a나b", 25_000),
		"따옴표":   strings.Repeat(`{"k":"v"},`, 20_000),
	} {
		if captured := CaptureOutput(line); captured.Text != line || captured.Truncated {
			t.Fatalf("%s: 가릴 것이 없는 긴 한 줄(%d바이트)이 바뀌었다(길이 %d)", name, len(line), len(captured.Text))
		}
	}
}

func TestSecretsAtEvery64KBOfALongLineAreRedacted(t *testing.T) {
	// 줄바꿈 없는 200KB — 옛 조각 경계(64KB 마다)에 걸치도록 비밀 칸을 둔다.
	var builder strings.Builder
	for builder.Len() < 200_000 {
		for builder.Len()%formerPieceBoundary < formerPieceBoundary-12 && builder.Len() < 200_000 {
			builder.WriteByte('x')
		}
		builder.WriteString(`"token":"Zq81LeakPiece"`)
	}
	captured := CaptureOutput(builder.String())
	if strings.Contains(captured.Text, "Zq81") || captured.Redactions["secret_field"] != strings.Count(builder.String(), "Zq81") {
		t.Fatalf("64KB 마다 둔 비밀 칸이 남았다: 남은 %d · 가린 %d · 넣은 %d",
			strings.Count(captured.Text, "Zq81"), captured.Redactions["secret_field"], strings.Count(builder.String(), "Zq81"))
	}
}
