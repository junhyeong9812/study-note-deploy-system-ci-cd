package agent

// R2 · R3 — 경로 길이의 단위 · 값의 넘침. 기준: requirement-spec.md(traffic-map) "T5 리뷰 루프 1 의 반영" R2 · R3.
//
// 정한 계약:
//   - path 의 길이 1024 는 UTF-16 단위로 잰다(관측 서버 TrafficEntry.kt:313 의 String.length). 넘으면 그 줄은 unparsed(묶음에 세지 않는다).
//     4바이트 글자(U+1F600) 512개 = 1024 단위는 받고 513개는 unparsed · 한글 · ASCII 는 한 글자 = 1 단위.
//   - upstream_time: 조각의 ms 가 int64 를 넘거나 조각의 합이 int64 를 넘으면 그 줄은 unparsed. 넘지 않는 가장 큰 합(9223372036854775807)은 받는다.
//   - request_time 의 ms · bytes_sent 도 int64 를 넘으면 unparsed, 가장 큰 값은 받는다(관측 서버는 시간 · 바이트에 상한이 없다 — 음수가 아니면 받는다,
//     TrafficEntry.kt:284 · :288 · :320-323. MAX_CELL_COUNT 는 세는 수에만 — :329).
//   - 어느 경우에도 그 항목은 관측 서버가 받는다(줄 하나가 항목 전체를 버리게 하지 않는다).

import (
	"strings"
	"testing"
)

func TestPathLengthIsMeasuredInUtf16Units(t *testing.T) {
	defer failOnPanic(t)
	emoji := "\U0001F600"
	cases := []struct {
		name, path string
		accepted   bool
	}{
		{"이모지 512개 = 1024 단위", strings.Repeat(emoji, 512), true},
		{"이모지 513개 = 1026 단위", strings.Repeat(emoji, 513), false},
		{"/ + 이모지 511 + a = 1024", "/" + strings.Repeat(emoji, 511) + "a", true},
		{"/ + 이모지 512 = 1025", "/" + strings.Repeat(emoji, 512), false},
		{"한글 1024자", "/" + strings.Repeat("가", 1023), true},
		{"한글 1025자", "/" + strings.Repeat("가", 1024), false},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			defer failOnPanic(t)
			body, _, _ := cycleWith(t, defaultAccess().with(func(line *accessLine) { line.path = testCase.path }).text())
			source := body.source(t, nginx)
			if testCase.accepted {
				if source.Lines != 1 || len(body.Requests) != 1 || body.Requests[0].Path != testCase.path {
					t.Fatalf("%s: 받아 요청별 줄에 싣는다: %+v", testCase.name, source)
				}
			} else if source.Unparsed != 1 || source.Lines != 0 || len(body.Segments) != 0 {
				t.Fatalf("%s: unparsed — 묶음에 세지 않는다: %+v %d묶음", testCase.name, source, len(body.Segments))
			}
		})
	}
}

func TestOverflowingValuesMakeTheLineUnparsed(t *testing.T) {
	defer failOnPanic(t)
	cases := []struct {
		name     string
		change   func(line *accessLine)
		accepted bool
		check    func(t *testing.T, body trafficBody)
	}{
		{"upstream 조각마다는 들지만 합이 넘침", func(line *accessLine) { line.upstreamTime = "9000000000000000.000, 9000000000000000.000" }, false, nil},
		{"upstream 조각 하나가 넘침", func(line *accessLine) { line.upstreamTime = "9300000000000000.000" }, false, nil},
		{"upstream 가장 큰 합", func(line *accessLine) { line.upstreamTime = "9223372036854775.000, 0.807" }, true, func(t *testing.T, body trafficBody) {
			if upstream := body.Requests[0].UpstreamMs; upstream == nil || *upstream != 9223372036854775807 {
				t.Fatalf("넘지 않는 가장 큰 합은 받는다: %v", upstream)
			}
		}},
		{"request_time 넘침", func(line *accessLine) { line.requestTime = "9300000000000000.000" }, false, nil},
		{"request_time 가장 큰 값", func(line *accessLine) { line.requestTime = "9223372036854775.807" }, true, func(t *testing.T, body trafficBody) {
			if body.Requests[0].DurationMs != 9223372036854775807 || body.Segments[0].Buckets[19] != 1 {
				t.Fatalf("가장 큰 request_time 은 받는다: %+v", body.Requests[0])
			}
		}},
		{"bytes_sent 넘침", func(line *accessLine) { line.bytesSent = "9223372036854775808" }, false, nil},
		{"bytes_sent 가장 큰 값", func(line *accessLine) { line.bytesSent = "9223372036854775807" }, true, func(t *testing.T, body trafficBody) {
			if body.Requests[0].BytesSent != 9223372036854775807 {
				t.Fatalf("가장 큰 bytes_sent 는 받는다: %d", body.Requests[0].BytesSent)
			}
		}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			defer failOnPanic(t)
			line := defaultAccess().with(testCase.change).text()
			body, _, _ := cycleWith(t, line, accessLines(1, lineBaseMillis+1, nil)[0]) // 곁의 맞는 줄 하나 — 항목이 버려지지 않고 그 줄은 실린다
			source := body.source(t, nginx)
			if testCase.accepted {
				if source.Lines != 2 || source.Unparsed != 0 {
					t.Fatalf("%s: 받는다: %+v", testCase.name, source)
				}
				testCase.check(t, body)
			} else {
				if source.Lines != 1 || source.Unparsed != 1 || len(body.Segments) != 1 || body.Segments[0].Count != 1 {
					t.Fatalf("%s: 그 줄은 unparsed · 곁의 줄은 그대로: %+v %+v", testCase.name, source, body.Segments)
				}
				for _, request := range body.Requests {
					if request.UpstreamMs != nil && *request.UpstreamMs < 0 {
						t.Fatalf("%s: 음수 upstream_ms 가 실렸다", testCase.name)
					}
				}
			}
		})
	}
}
