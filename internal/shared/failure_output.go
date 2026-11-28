package shared

// 배포 실패의 전체 출력 — 가린 뒤 크기 상한 안으로 줄인다(agent 는 보내기 전에, master 는 저장하기 전에 같은 것을 쓴다).
// 계약: failure_output_test.go 머리의 주석.
//
// 차례: 글자 정리 → 줄로 모으기(PEM 덩어리는 여기서 한 줄로) → 줄마다 가림 → 앞 · 끝만 붙잡아 두기.
// 흘려 받는 동안 붙잡는 것은 앞 32KB · 끝 창(상한의 두 배까지) · 끝나지 않은 줄(64KB까지) · 가릴 차례를 기다리는 줄 묶음(64KB 남짓)뿐이다.

import (
	"bytes"
	"fmt"
	"net/netip"
	"regexp"
	"sort"
	"unicode/utf8"
)

const (
	FailureOutputLimit     = 256 << 10 // 가린 뒤 남기는 전체 상한(가운데를 버렸다는 표시 줄 포함)
	FailureOutputHeadLimit = 32 << 10  // 넘을 때 앞에서 남기는 몫

	// master 가 agent 의 단계 요청에 싣는 머리 — 값이 정확히 FailureOutputVersion 일 때만 agent 가 실패 봉투에 output 을 싣는다.
	FailureOutputHeader  = "X-Deploy-Failure-Output"
	FailureOutputVersion = "v1"
)

const (
	longLineLimit    = 1 << 20  // 한 줄(줄바꿈을 빼고)을 통째로 가리는 상한 — 넘는 줄은 내용 없이 표시 줄로 바뀐다
	redactBlockLimit = 64 << 10 // 줄을 이만큼 모아 한 번에 거른다
	pemSwallowLimit  = 64 << 10 // END 없는 PEM 을 삼키는 상한
	oscLengthLimit   = 4 << 10  // 끝 표시 없는 OSC 를 삼키는 상한
	maskPrefix       = "[가림:"
)

// CapturedOutput — 가리고 줄인 출력. agent 의 실패 봉투(error.output)에 이 꼴로 실린다.
type CapturedOutput struct {
	Text         string         `json:"text"`
	Truncated    bool           `json:"truncated"`
	OmittedBytes int64          `json:"omitted_bytes"`
	Redactions   map[string]int `json:"redactions"` // 가린 종류 → 횟수
}

// 글자 정리의 제어열 상태
const (
	escapeNone = iota
	escapeStarted
	escapeControlSequence  // CSI — ESC [ … 끝 글자(0x40–0x7E)
	escapeOperatingCommand // OSC — ESC ] … BEL 또는 ESC \
	escapeOperatingCommandEscape
)

// OutputCapture — 출력을 흘려 받으며 가리고 줄인다(전체를 메모리에 올리지 않는다).
type OutputCapture struct {
	// 글자 정리
	incompleteRune      []byte // 쓰기 사이에서 나뉜 UTF-8 의 앞 조각
	escapeState         int
	oscLength           int
	afterCarriageReturn bool // 바로 앞이 \r — 이어 오는 \n 은 같은 줄바꿈이다
	afterInvalidByte    bool // 깨진 바이트가 이어지면 U+FFFD 하나로 바꾼다

	// 줄로 모으기 · 가림
	line         []byte // 아직 줄바꿈을 만나지 않은 글
	searchedLine int    // line 에서 줄바꿈을 찾아 본 곳까지(1바이트씩 써도 처음부터 다시 찾지 않는다)
	longLine     int64  // 0 보다 크면 지금 줄이 longLineLimit 를 넘었다 — 모으지 않고 바이트만 센다
	longOmitted  int64  // 긴 줄로 버린 바이트의 합
	block        []byte // 가릴 차례를 기다리는 온전한 줄들
	insidePem    bool
	pemSwallowed int
	redactions   map[string]int

	// 앞 · 끝
	total  int64
	head   []byte // 가린 글의 처음 FailureOutputHeadLimit 바이트
	window []byte // 가린 글의 끝(FailureOutputLimit 이상 · 두 배 이하)
}

func NewOutputCapture() *OutputCapture { return &OutputCapture{redactions: map[string]int{}} }

// Write — 늘 받은 만큼 받는다(오류가 없다).
func (capture *OutputCapture) Write(chunk []byte) (int, error) {
	capture.clean(chunk, false)
	capture.drainLines()
	return len(chunk), nil
}

func (capture *OutputCapture) Finish() CapturedOutput {
	capture.clean(nil, true)
	capture.drainLines()
	if capture.longLine > 0 {
		capture.takeLongLine(capture.longLine+int64(len(capture.line)), false)
	} else if len(capture.line) > 0 {
		capture.takeSegment(capture.line, false)
	}
	capture.line = nil
	capture.flushBlock()

	redactions := make(map[string]int, len(capture.redactions))
	for kind, count := range capture.redactions {
		redactions[kind] = count
	}
	if capture.total <= FailureOutputLimit {
		return CapturedOutput{Text: string(capture.window), Truncated: capture.longOmitted > 0, OmittedBytes: capture.longOmitted, Redactions: redactions}
	}

	head := capture.head[:cutHead(capture.head)]
	separator := ""
	if len(head) > 0 && head[len(head)-1] != '\n' {
		separator = "\n" // 표시 줄은 언제나 한 줄을 차지한다
	}
	longestMarker := len(omittedMarker(capture.total)) // N 은 전체보다 클 수 없다 — 자리를 넉넉히 잡는다
	tailBudget := FailureOutputLimit - len(head) - len(separator) - longestMarker
	tail := capture.window[cutTail(capture.window, len(capture.window)-tailBudget):]
	omitted := capture.total - int64(len(head)) - int64(len(tail))

	var text bytes.Buffer
	text.Grow(len(head) + len(separator) + longestMarker + len(tail))
	text.Write(head)
	text.WriteString(separator)
	text.WriteString(omittedMarker(omitted))
	text.Write(tail)
	return CapturedOutput{Text: text.String(), Truncated: true, OmittedBytes: omitted + capture.longOmitted, Redactions: redactions}
}

// CaptureOutput — 글 하나를 한 번에 가리고 줄인다.
func CaptureOutput(raw string) CapturedOutput {
	capture := NewOutputCapture()
	capture.Write([]byte(raw))
	return capture.Finish()
}

func omittedMarker(omitted int64) string {
	return fmt.Sprintf("[가운데 %d바이트를 버렸다]\n", omitted)
}

// cutHead — 앞에서 남길 길이: 마지막 줄바꿈까지, 없으면 글자 경계까지.
func cutHead(head []byte) int {
	if newline := bytes.LastIndexByte(head, '\n'); newline >= 0 {
		return newline + 1
	}
	return runeBoundaryBefore(head, len(head))
}

// cutTail — 끝을 어디서 시작할지: start 가 줄의 처음이면 그대로, 아니면 다음 줄의 처음, 줄바꿈이 없으면 글자 경계.
func cutTail(window []byte, start int) int {
	if start <= 0 || window[start-1] == '\n' {
		return max(start, 0)
	}
	if newline := bytes.IndexByte(window[start:], '\n'); newline >= 0 {
		return start + newline + 1
	}
	for start < len(window) && !utf8.RuneStart(window[start]) {
		start++
	}
	return start
}

// runeBoundaryBefore — text[:limit] 이 글자의 중간에서 끝나지 않는 가장 긴 limit 이하의 자리(text 는 올바른 UTF-8).
func runeBoundaryBefore(text []byte, limit int) int {
	start := limit - 1
	for start > 0 && start > limit-utf8.UTFMax && !utf8.RuneStart(text[start]) {
		start--
	}
	if start >= 0 && !utf8.FullRune(text[start:limit]) {
		return start
	}
	return limit
}

// ---- 글자 정리 ----

// clean — 받은 바이트를 정리해 line 에 붙인다. finishing 이면 끝나지 않은 글자 · 제어열을 마저 처리한다.
func (capture *OutputCapture) clean(chunk []byte, finishing bool) {
	data := chunk
	if len(capture.incompleteRune) > 0 {
		data = append(capture.incompleteRune, chunk...)
		capture.incompleteRune = nil
	}
	for index := 0; index < len(data); {
		value := data[index]
		switch capture.escapeState {
		case escapeStarted:
			switch {
			case value == '[':
				capture.escapeState = escapeControlSequence
				index++
			case value == ']':
				capture.escapeState, capture.oscLength = escapeOperatingCommand, 0
				index++
			case value >= 0x20 && value <= 0x7e: // 그 밖의 ESC 글자
				capture.escapeState = escapeNone
				index++
			default: // 제어열이 아니다 — ESC 만 버리고 이 바이트는 보통으로
				capture.escapeState = escapeNone
			}
			continue
		case escapeControlSequence:
			switch {
			case value >= 0x40 && value <= 0x7e:
				capture.escapeState = escapeNone
				index++
			case value >= 0x20 && value <= 0x3f:
				index++
			default: // 끝 글자 없이 끊겼다 — 이 바이트는 보통으로
				capture.escapeState = escapeNone
			}
			continue
		case escapeOperatingCommand:
			switch {
			case value == 0x07:
				capture.escapeState = escapeNone
				index++
			case value == 0x1b:
				capture.escapeState = escapeOperatingCommandEscape
				index++
			case value == '\n' || capture.oscLength >= oscLengthLimit: // 단순화: 끝 표시 없는 OSC 는 줄바꿈이나 4KB 에서 그친다(그때까지의 글은 버린다)
				capture.escapeState = escapeNone
			default:
				capture.oscLength++
				index++
			}
			continue
		case escapeOperatingCommandEscape:
			if value == '\\' {
				capture.escapeState = escapeNone
				index++
			} else {
				capture.escapeState = escapeStarted
			}
			continue
		}

		if value >= 0x20 && value < 0x7f { // 흔한 경우 — 보이는 ASCII 를 한꺼번에
			end := index + 1
			for end < len(data) && data[end] >= 0x20 && data[end] < 0x7f {
				end++
			}
			capture.line = append(capture.line, data[index:end]...)
			capture.afterCarriageReturn, capture.afterInvalidByte = false, false
			index = end
			continue
		}
		if value < 0x80 {
			switch value {
			case 0x1b:
				capture.escapeState = escapeStarted
			case '\n':
				if !capture.afterCarriageReturn {
					capture.line = append(capture.line, '\n')
				}
			case '\r':
				capture.line = append(capture.line, '\n')
			case '\t':
				capture.line = append(capture.line, '\t')
			default:
				capture.line = utf8.AppendRune(capture.line, utf8.RuneError)
			}
			capture.afterCarriageReturn = value == '\r'
			capture.afterInvalidByte = false
			index++
			continue
		}
		if !utf8.FullRune(data[index:]) && !finishing { // 다음 쓰기에 이어진다
			capture.incompleteRune = append([]byte(nil), data[index:]...)
			return
		}
		character, size := utf8.DecodeRune(data[index:])
		capture.afterCarriageReturn = false
		if character == utf8.RuneError && size <= 1 {
			if !capture.afterInvalidByte {
				capture.line = utf8.AppendRune(capture.line, utf8.RuneError)
			}
			capture.afterInvalidByte = true
			index++
			continue
		}
		capture.line = append(capture.line, data[index:index+size]...)
		capture.afterInvalidByte = false
		index += size
	}
	if finishing {
		capture.escapeState = escapeNone // 끝나지 않은 제어열은 버린다
	}
}

// ---- 줄로 모으기 ----

// drainLines — line 에서 온전한 줄을 떼어 낸다. 줄은 longLineLimit 까지 통째로 모은다 — 그보다 길어지면 더 모으지 않고
// 줄이 끝날 때까지 바이트만 세다가 표시 줄 하나로 바꾼다(줄바꿈 없는 큰 출력도 메모리가 늘지 않는다).
func (capture *OutputCapture) drainLines() {
	for {
		if capture.longLine > 0 {
			newline := bytes.IndexByte(capture.line, '\n')
			if newline < 0 {
				capture.longLine += int64(len(capture.line))
				capture.line = capture.line[:0]
				break
			}
			capture.takeLongLine(capture.longLine+int64(newline), true)
			capture.line = capture.line[newline+1:]
			continue
		}
		if newline := bytes.IndexByte(capture.line[capture.searchedLine:], '\n'); newline >= 0 {
			end := capture.searchedLine + newline + 1
			if end-1 > longLineLimit {
				capture.takeLongLine(int64(end-1), true)
			} else {
				capture.takeSegment(capture.line[:end], true)
			}
			capture.line, capture.searchedLine = capture.line[end:], 0
			continue
		}
		capture.searchedLine = len(capture.line)
		if len(capture.line) > longLineLimit {
			capture.longLine, capture.searchedLine = int64(len(capture.line)), 0
			capture.line = capture.line[:0]
		}
		break
	}
	if len(capture.line) == 0 {
		capture.line = capture.line[:0:0] // 다 쓴 배열을 놓아준다
	}
}

// takeLongLine — longLineLimit 를 넘은 줄(length 바이트 — 줄바꿈을 빼고)을 내용 없이 표시 줄로 바꾼다. 줄바꿈은 입력대로.
func (capture *OutputCapture) takeLongLine(length int64, endsLine bool) {
	capture.longLine = 0
	capture.longOmitted += length
	marker := fmt.Sprintf("[긴 한 줄 %d바이트를 버렸다]", length)
	if endsLine {
		marker += "\n"
	}
	capture.takeSegment([]byte(marker), endsLine)
}

var (
	pemInline = regexp.MustCompile(`-----BEGIN [A-Z0-9 ]{1,64}-----.*?-----END [A-Z0-9 ]{1,64}-----`)
	pemBegin  = regexp.MustCompile(`-----BEGIN [A-Z0-9 ]{1,64}-----`)
	pemEnd    = regexp.MustCompile(`-----END [A-Z0-9 ]{1,64}-----`)
)

// takeSegment — 온전한 줄(endsLine) 또는 줄바꿈 없는 마지막 줄. PEM 덩어리는 여기서 표시 하나로 바꾸고, 나머지는 묶음에 모은다.
func (capture *OutputCapture) takeSegment(segment []byte, endsLine bool) {
	if capture.insidePem {
		if location := pemEnd.FindIndex(segment); location != nil {
			capture.insidePem = false
			if rest := segment[location[1]:]; len(rest) > 0 {
				capture.takeSegment(rest, endsLine)
			}
			return
		}
		capture.pemSwallowed += len(segment)
		if capture.pemSwallowed > pemSwallowLimit { // 단순화: END 가 64KB 안에 없으면 덩어리가 끝난 것으로 본다(열쇠는 그보다 짧다)
			capture.insidePem = false
		}
		return
	}
	if bytes.Contains(segment, []byte("-----BEGIN ")) {
		segment = pemInline.ReplaceAllFunc(segment, func([]byte) []byte {
			capture.redactions["pem"]++
			return []byte(maskPrefix + "pem]")
		})
		if location := pemBegin.FindIndex(segment); location != nil {
			capture.redactions["pem"]++
			capture.block = append(capture.block, segment[:location[0]]...)
			capture.block = append(capture.block, maskPrefix+"pem]"...)
			capture.insidePem, capture.pemSwallowed = true, 0
			if !endsLine {
				capture.flushBlock()
			}
			return
		}
	}
	capture.block = append(capture.block, segment...)
	if !endsLine || len(capture.block) >= redactBlockLimit {
		capture.flushBlock()
	}
}

func (capture *OutputCapture) flushBlock() {
	if len(capture.block) == 0 {
		return
	}
	capture.keep(redactBlock(capture.block, capture.redactions))
	capture.block = capture.block[:0]
}

// keep — 가린 글을 앞 · 끝 창에 붙인다.
func (capture *OutputCapture) keep(text []byte) {
	capture.total += int64(len(text))
	if room := FailureOutputHeadLimit - len(capture.head); room > 0 {
		capture.head = append(capture.head, text[:min(room, len(text))]...)
	}
	capture.window = append(capture.window, text...)
	if len(capture.window) > 2*FailureOutputLimit {
		kept := copy(capture.window, capture.window[len(capture.window)-FailureOutputLimit:])
		capture.window = capture.window[:kept]
	}
}

// ---- 가림 ----

// redactionRule — 가림 규칙 하나. mayMatch 는 ASCII 소문자로 바꾼 글에서 규칙이 맞을 수 있는지를 싸게 본다 —
// 묶음 전체에 한 번, 그다음 줄마다 한 번 보고, 맞을 수 있는 줄에만 find 를 돌린다.
// 미리 거르는 글자는 규칙이 맞는 모든 글에 반드시 들어 있는 것이어야 한다(아니면 거르기가 규칙을 가린다).
// find 는 줄에서 맞음을 앞에서부터 겹치지 않게 돌려준다. 시간은 줄의 길이에 비례해야 한다 — 맞춰 보는 자리마다 줄 끝까지 훑지 않는다
// (실패한 자리의 훑기가 서로 겹치지 않거나, 길이에 상한이 있거나, 맞은 자리 안의 자리를 건너뛴다).
type redactionRule struct {
	kind     string
	mayMatch func(lowered []byte) bool
	find     func(line, lowered []byte) []redactionSpan
}

// redactionSpan — 맞음 하나에서 표시로 바꿀 값 [start, end). 맞음끼리 겹칠 수 있다(합쳐서 가린다).
type redactionSpan struct{ start, end int }

var redactionRules = []redactionRule{
	{"token", containsAny(tokenNeedles...), findTokens},
	{"authorization", containsAny("authorization", "bearer"), findPatternValues(authorizationPattern, bearerPattern)},
	{"url_credential", containsAny("://"), findPatternValues(urlCredentialPattern)},
	{"secret_field", containsAny(secretFieldNeedles...), findSecretFields},
	{"hex", hasLongHexRun, findLongHex},
	{"email", containsAny("@"), findEmails},
	{"private_ip", mayHoldInternalAddress, findInternalAddresses},
}

// maskKinds — 표시에 쓰는 종류 전부(pem 은 줄로 모을 때 가린다).
var maskKinds = map[string]bool{"pem": true, "token": true, "authorization": true, "url_credential": true, "secret_field": true,
	"hex": true, "email": true, "private_ip": true}

var (
	// Authorization 머리 · 칸: 방식(Bearer · Basic …)은 남기고 값만 가린다.
	authorizationPattern = regexp.MustCompile(`(?i)authorization["']?[ \t]*[:=][ \t]*["']?(?:(?:bearer|basic|token|digest|negotiate)[ \t]+)?([^ \t"'\n,;]+)`)
	bearerPattern        = regexp.MustCompile(`(?i)\bbearer[ \t]+([^ \t"'\n,;]+)`)
	urlCredentialPattern = regexp.MustCompile(`://([^/ \t\n"'@:]*:[^/ \t\n"'@]+)@`)
	// 비밀 낱말 바로 뒤에서 맞춰 보는 칸의 나머지: 구분 · 값. 낱말 바로 뒤에 이름의 글자가 오면 맞지 않는다 — 이름이 비밀 낱말로 끝날 때만.
	// 따옴표 안의 값은 역빗금 이스케이프를 따라 닫는 따옴표까지 — 닫는 따옴표가 없으면 줄 끝까지.
	secretFieldRest = regexp.MustCompile(`^["']?[ \t]*[:=][ \t]*` + quotedOrBareValue + `)`)
	// 긴 옵션의 값(`--password <값>`) — `-` 로 시작하는 다음 옵션은 값이 아니다.
	longOptionRest = regexp.MustCompile(`^[ \t]+([^- \t"'\n][^ \t"'\n]*)`)
	dockerLogin    = regexp.MustCompile(`\bdocker[ \t]+login\b[^\n]*?[ \t]-p[ \t]+` + quotedOrBareValue + `)`)
	emailDomain    = regexp.MustCompile(`^[A-Za-z0-9-]+(?:\.[A-Za-z0-9-]+)*\.[A-Za-z]{2,}`) // '@' 바로 뒤에서 맞춰 본다
)

// quotedOrBareValue — 값: 따옴표 안(역빗금 이스케이프를 따라 닫는 따옴표까지, 없으면 줄 끝까지) 또는 구분 글자까지. 여는 묶음 `(?:` 은 닫지 않은 채 — 쓰는 쪽이 `)` 로 닫는다.
const quotedOrBareValue = `(?:"((?:[^"\\\n]|\\.)*)"?|'((?:[^'\\\n]|\\.)*)'?|([^ \t"'&,;\n]+)`

var internalPrefixes = []netip.Prefix{
	netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("172.16.0.0/12"), netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("100.64.0.0/10"), netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("fc00::/7"), netip.MustParsePrefix("fe80::/10"),
}

// redactBlock — 온전한 줄들(마지막은 줄바꿈 없는 조각일 수 있다)을 규칙의 차례대로 가린다. 규칙은 줄을 넘어 맞지 않는다.
func redactBlock(block []byte, redactions map[string]int) []byte {
	var lowered []byte
	for _, rule := range redactionRules {
		if lowered == nil {
			lowered = asciiLower(block)
		}
		if !rule.mayMatch(lowered) {
			continue
		}
		var result []byte
		changed := false
		for start := 0; start < len(block); {
			end := len(block)
			if newline := bytes.IndexByte(block[start:], '\n'); newline >= 0 {
				end = start + newline + 1
			}
			line := block[start:end]
			if rule.mayMatch(lowered[start:end]) {
				if replaced, count := replaceSpans(line, rule.find(line, lowered[start:end]), rule.kind); count > 0 {
					redactions[rule.kind] += count
					if !changed {
						result = append(make([]byte, 0, len(block)), block[:start]...)
						changed = true
					}
					line = replaced
				}
			}
			if changed {
				result = append(result, line...)
			}
			start = end
		}
		if changed {
			block, lowered = result, nil
		}
	}
	return block
}

// replaceSpans — 맞음의 값을 표시로 바꾼다. 겹치는 값은 합쳐 표시 하나로. 빈 값 · 값 전체가 정확히 표시 하나인 것은 그대로 둔다.
// 다시 거치면 가림이 늘 수는 있어도(표시가 새 낱말 경계를 만든다) 줄지 않고, 가린 값이 되살아나지 않는다.
func replaceSpans(line []byte, spans []redactionSpan, kind string) ([]byte, int) {
	sort.Slice(spans, func(left, right int) bool { return spans[left].start < spans[right].start })
	var result []byte
	previous, count := 0, 0
	for index := 0; index < len(spans); {
		start, end := spans[index].start, spans[index].end
		for index++; index < len(spans) && spans[index].start < end; index++ {
			end = max(end, spans[index].end)
		}
		if start == end || isExactMask(line[start:end]) {
			continue
		}
		result = append(result, line[previous:start]...)
		result = append(result, maskPrefix+kind+"]"...)
		previous = end
		count++
	}
	if count == 0 {
		return line, 0
	}
	return append(result, line[previous:]...), count
}

// isExactMask — 값이 정확히 `[가림:<아는 종류>]` 하나인가. 표시로 시작해도 뒤에 글이 붙었으면 아니다.
func isExactMask(value []byte) bool {
	kind, found := bytes.CutPrefix(value, []byte(maskPrefix))
	if !found || len(kind) == 0 || kind[len(kind)-1] != ']' {
		return false
	}
	return maskKinds[string(kind[:len(kind)-1])]
}

func asciiLower(text []byte) []byte {
	lowered := make([]byte, len(text))
	for index, value := range text {
		if value >= 'A' && value <= 'Z' {
			value += 'a' - 'A'
		}
		lowered[index] = value
	}
	return lowered
}

func containsAny(needles ...string) func([]byte) bool {
	return func(lowered []byte) bool {
		for _, needle := range needles {
			if bytes.Contains(lowered, []byte(needle)) {
				return true
			}
		}
		return false
	}
}

// occurrences — lowered 에서 needles 가 나온 자리(처음 · 끝), 처음의 차례로(같으면 긴 것 먼저).
func occurrences(lowered []byte, needles []string) [][2]int {
	var found [][2]int
	for _, needle := range needles {
		for offset := 0; ; {
			index := bytes.Index(lowered[offset:], []byte(needle))
			if index < 0 {
				break
			}
			found = append(found, [2]int{offset + index, offset + index + len(needle)})
			offset += index + 1
		}
	}
	sort.Slice(found, func(left, right int) bool {
		if found[left][0] != found[right][0] {
			return found[left][0] < found[right][0]
		}
		return found[left][1] > found[right][1]
	})
	return found
}

// findPatternValues — 정규식의 맞음마다 처음으로 잡힌 묶음(값). RE2 라 한 번 훑기다.
func findPatternValues(patterns ...*regexp.Regexp) func([]byte, []byte) []redactionSpan {
	return func(line, _ []byte) []redactionSpan {
		var spans []redactionSpan
		for _, pattern := range patterns {
			for _, match := range pattern.FindAllSubmatchIndex(line, -1) {
				if start, end := firstGroup(match); start >= 0 {
					spans = append(spans, redactionSpan{start, end})
				}
			}
		}
		return spans
	}
}

// firstGroup — 맞음에서 처음으로 잡힌 묶음의 자리(없으면 -1).
func firstGroup(match []int) (int, int) {
	for group := 2; group+1 < len(match); group += 2 {
		if match[group] >= 0 {
			return match[group], match[group+1]
		}
	}
	return -1, -1
}

// ---- 토큰 ----

// tokenShape — 접두어(대소문자 그대로) + 허용 글자의 연속. 연속이 minimum 이상이면 접두어부터 연속의 끝까지가 토큰이다
// (허용 글자에 _ 가 있어 이어 붙은 토큰 `ghp_…ghp_…` 도 하나로 가린다).
type tokenShape struct {
	prefix       string
	allowed      func(byte) bool
	minimum      int
	wordEndAfter bool // 연속 바로 뒤에 낱말 글자가 오면 아니다
}

func isAlphanumeric(value byte) bool {
	return (value >= '0' && value <= '9') || (value >= 'a' && value <= 'z') || (value >= 'A' && value <= 'Z')
}
func isUpperOrDigit(value byte) bool {
	return (value >= '0' && value <= '9') || (value >= 'A' && value <= 'Z')
}
func isTokenByte(value byte) bool   { return isAlphanumeric(value) || value == '_' || value == '-' }
func isUnderscored(value byte) bool { return isAlphanumeric(value) || value == '_' }
func isSlackPathByte(value byte) bool {
	return isAlphanumeric(value) || value == '/' || value == '_' || value == '-'
}

var tokenShapes = []tokenShape{
	{"ghp_", isUnderscored, 36, false}, {"gho_", isUnderscored, 36, false}, {"ghu_", isUnderscored, 36, false},
	{"ghs_", isUnderscored, 36, false}, {"ghr_", isUnderscored, 36, false},
	{"github_pat_", isUnderscored, 22, false},
	{"sk-", isTokenByte, 20, false},
	{"AKIA", isUpperOrDigit, 16, true}, {"ASIA", isUpperOrDigit, 16, true},
	{"xoxa-", isTokenByte, 10, false}, {"xoxb-", isTokenByte, 10, false}, {"xoxp-", isTokenByte, 10, false},
	{"xoxr-", isTokenByte, 10, false}, {"xoxs-", isTokenByte, 10, false},
	{"AIza", isTokenByte, 35, false},
	{"npm_", isUnderscored, 36, true},
}

const slackWebhookPrefix = "hooks.slack.com/services/"

var tokenNeedles = []string{"ghp_", "gho_", "ghu_", "ghs_", "ghr_", "github_pat_", "sk-", "akia", "asia", "eyj", "xox", "aiza", "npm_", slackWebhookPrefix}

// findTokens — 토큰 꼴. 낱말의 처음에서만(앞이 낱말 글자가 아닐 때). 맞은 토큰 안의 자리는 건너뛰고,
// JWT 가 첫 마디에서 실패하면 그 마디 안의 다른 자리도 같은 까닭으로 실패하므로 건너뛴다 — 한 번 훑기.
func findTokens(line, lowered []byte) []redactionSpan {
	var spans []redactionSpan
	previous, jwtSkipUntil := 0, 0
	for _, occurrence := range occurrences(lowered, tokenNeedles) {
		start := occurrence[0]
		if start < previous {
			continue
		}
		if bytes.HasPrefix(lowered[start:], []byte(slackWebhookPrefix)) { // 웹훅은 주소를 남기고 경로를 가린다
			valueStart := start + len(slackWebhookPrefix)
			end := runEnd(line, valueStart, isSlackPathByte)
			if end > valueStart {
				spans = append(spans, redactionSpan{valueStart, end})
				previous = end
			}
			continue
		}
		if start > 0 && isWordByte(line[start-1]) {
			continue
		}
		if bytes.HasPrefix(line[start:], []byte("eyJ")) {
			if start < jwtSkipUntil {
				continue
			}
			end, firstSegmentEnd := jwtEnd(line, start)
			if end < 0 {
				jwtSkipUntil = firstSegmentEnd
				continue
			}
			spans = append(spans, redactionSpan{start, end})
			previous = end
			continue
		}
		for _, shape := range tokenShapes {
			if !bytes.HasPrefix(line[start:], []byte(shape.prefix)) {
				continue
			}
			bodyStart := start + len(shape.prefix)
			end := runEnd(line, bodyStart, shape.allowed)
			if end-bodyStart < shape.minimum || (shape.wordEndAfter && end < len(line) && isWordByte(line[end])) {
				break
			}
			spans = append(spans, redactionSpan{start, end})
			previous = end
			break
		}
	}
	return spans
}

func runEnd(line []byte, start int, allowed func(byte) bool) int {
	end := start
	for end < len(line) && allowed(line[end]) {
		end++
	}
	return end
}

// jwtEnd — `eyJ<10+>.eyJ<10+>.<10+>` 의 끝. 맞지 않으면 -1 과 첫 마디의 끝.
func jwtEnd(line []byte, start int) (end int, firstSegmentEnd int) {
	firstSegmentEnd = runEnd(line, start+3, isTokenByte)
	if firstSegmentEnd-(start+3) < 10 || firstSegmentEnd >= len(line) || line[firstSegmentEnd] != '.' ||
		!bytes.HasPrefix(line[firstSegmentEnd+1:], []byte("eyJ")) {
		return -1, firstSegmentEnd
	}
	secondStart := firstSegmentEnd + 4
	secondEnd := runEnd(line, secondStart, isTokenByte)
	if secondEnd-secondStart < 10 || secondEnd >= len(line) || line[secondEnd] != '.' {
		return -1, firstSegmentEnd
	}
	thirdEnd := runEnd(line, secondEnd+1, isTokenByte)
	if thirdEnd-(secondEnd+1) < 10 {
		return -1, firstSegmentEnd
	}
	return thirdEnd, firstSegmentEnd
}

// ---- 비밀 칸 ----

// secretWords — 이름(소문자로)이 이 낱말로 끝나면 비밀 칸이다. 한글 이름은 그대로.
var secretWords = []string{"password", "passwd", "secret", "token", "api_key", "apikey", "api-key",
	"access_key", "accesskey", "access-key", "private_key", "privatekey", "private-key",
	"secret_key", "secretkey", "secret-key", "secret_key_base", "secretkeybase", "secret-key-base", "비밀번호", "패스워드", "암호"}

// shortSecretWords — 이름의 끝 마디가 정확히 이것일 때만(pass 는 앞에 다른 마디가 있을 때만) · 값이 숫자만이면 남긴다(시험 수 등).
// 혼자 선 pwd 는 값이 / 로 시작하면 남긴다(env 덤프의 PWD=/app).
var shortSecretWords = []string{"pass", "pwd"}

var secretFieldNeedles = append(append([]string{"docker"}, secretWords...), shortSecretWords...)

const (
	longOptionNameLimit = 128 // 긴 옵션의 이름을 거슬러 찾는 상한
	replacementRune     = "\xef\xbf\xbd"
)

func isNameByte(value byte) bool { return isTokenByte(value) || value == '.' }

// findSecretFields — 이름이 비밀 낱말로 끝나는 칸의 값. 낱말이 나온 자리에서만 맞춰 보고, 맞은 칸 안의 자리는 건너뛴다.
// docker login 의 -p 값은 따로 찾는다(칸 · 긴 옵션과 겹쳐도 버리지 않는다 — replaceSpans 가 합친다).
func findSecretFields(line, lowered []byte) []redactionSpan {
	var spans []redactionSpan
	previous := 0
	for _, occurrence := range occurrences(lowered, append(append([]string{}, secretWords...), shortSecretWords...)) {
		wordStart, wordEnd := occurrence[0], occurrence[1]
		if wordStart < previous {
			continue
		}
		word := string(lowered[wordStart:wordEnd])
		short := word == "pass" || word == "pwd"
		before := byte(' ')
		if wordStart > 0 {
			before = line[wordStart-1]
		}
		separated := before == '_' || before == '-' || before == '.'
		if short {
			if word == "pass" && !(separated && wordStart >= 2 && isAlphanumeric(line[wordStart-2])) {
				continue // pass 는 앞에 다른 마디가 있을 때만(PASS: · pass=12 는 시험 결과다)
			}
			if word == "pwd" && !separated && isAlphanumeric(before) {
				continue // cwd 같은 낱말의 끝
			}
		}
		if wordEnd < len(line) && line[wordEnd] == '>' && !short { // XML `<이름>값</이름>`
			if valueStart, valueEnd, found := xmlElementValue(line, wordStart, wordEnd); found {
				spans = append(spans, redactionSpan{valueStart, valueEnd})
				previous = valueEnd
			}
			continue
		}
		restStart := wordEnd
		for bytes.HasPrefix(line[restStart:], []byte(replacementRune)) { // 이름 안에 낀 제어 문자(정리 뒤의 U+FFFD)
			restStart += len(replacementRune)
		}
		match := secretFieldRest.FindSubmatchIndex(line[restStart:])
		if match == nil {
			optionStart := wordStart
			for optionStart > 0 && wordStart-optionStart < longOptionNameLimit && isTokenByte(line[optionStart-1]) {
				optionStart--
			}
			isLongOption := bytes.HasPrefix(line[optionStart:], []byte("--")) && optionStart+2 < wordStart+1 &&
				(optionStart == 0 || line[optionStart-1] == ' ' || line[optionStart-1] == '\t')
			if !isLongOption || short {
				continue
			}
			if match = longOptionRest.FindSubmatchIndex(line[restStart:]); match == nil {
				continue
			}
		}
		valueStart, valueEnd := firstGroup(match)
		if valueStart < 0 {
			continue
		}
		valueStart, valueEnd = restStart+valueStart, restStart+valueEnd
		value := line[valueStart:valueEnd]
		if short && (allDigits(value) || (word == "pwd" && !separated && bytes.HasPrefix(value, []byte("/")))) {
			continue
		}
		spans = append(spans, redactionSpan{valueStart, valueEnd})
		previous = restStart + match[1]
	}
	if bytes.Contains(lowered, []byte("docker")) {
		for _, match := range dockerLogin.FindAllSubmatchIndex(line, -1) {
			if start, end := firstGroup(match); start >= 0 {
				spans = append(spans, redactionSpan{start, end})
			}
		}
	}
	return spans
}

// xmlElementValue — `<이름>값</이름>`(같은 줄 · 여는 이름과 닫는 이름이 같다)의 값. 이름은 낱말로 끝난다(wordEnd 바로 뒤가 '>').
// 값은 다음 '<' 까지 — 훑기는 다음 '<' 에서 멈추므로 자리마다 겹치지 않는다. 빈 값은 아니다.
func xmlElementValue(line []byte, wordStart, wordEnd int) (int, int, bool) {
	nameStart := wordStart
	for nameStart > 0 && wordStart-nameStart < longOptionNameLimit && isNameByte(line[nameStart-1]) {
		nameStart--
	}
	if nameStart == 0 || line[nameStart-1] != '<' {
		return 0, 0, false
	}
	valueStart := wordEnd + 1
	valueEnd := bytes.IndexByte(line[valueStart:], '<')
	if valueEnd <= 0 {
		return 0, 0, false
	}
	valueEnd += valueStart
	closing := "</" + string(line[nameStart:wordEnd]) + ">"
	if !bytes.HasPrefix(line[valueEnd:], []byte(closing)) {
		return 0, 0, false
	}
	return valueStart, valueEnd, true
}

func allDigits(value []byte) bool {
	for _, character := range value {
		if character < '0' || character > '9' {
			return false
		}
	}
	return len(value) > 0
}

// ---- 이메일 ----

func isEmailLocalByte(value byte) bool {
	return isWordByte(value) || value == '.' || value == '%' || value == '+' || value == '-'
}

// findEmails — '@' 가 나온 자리에서만 앞(이름)과 뒤(도메인)를 맞춰 본다. 앞뒤 훑기는 '@' 에서 멈추므로 서로 겹치지 않는다.
func findEmails(line, _ []byte) []redactionSpan {
	var spans []redactionSpan
	previous := 0
	for offset := 0; offset < len(line); {
		at := bytes.IndexByte(line[offset:], '@')
		if at < 0 {
			break
		}
		at += offset
		start := at
		for start > previous && isEmailLocalByte(line[start-1]) {
			start--
		}
		domain := emailDomain.FindIndex(line[at+1:])
		if start == at || domain == nil {
			offset = at + 1
			continue
		}
		end := at + 1 + domain[1]
		spans = append(spans, redactionSpan{start, end})
		previous, offset = end, end
	}
	return spans
}

// ---- 16진 ----

func isHexDigit(value byte) bool {
	return (value >= '0' && value <= '9') || (value >= 'a' && value <= 'f') || (value >= 'A' && value <= 'F')
}

func isWordByte(value byte) bool { return isAlphanumeric(value) || value == '_' }

const longHexLength = 41 // 40자 커밋 SHA 는 남긴다

func hasLongHexRun(lowered []byte) bool {
	run := 0
	for _, value := range lowered {
		if isHexDigit(value) {
			if run++; run >= longHexLength {
				return true
			}
		} else {
			run = 0
		}
	}
	return false
}

// digestLengths — 이 이름 뒤에 정확히 이 길이일 때만 다이제스트로 보고 남긴다.
var digestLengths = map[string]int{"sha256:": 64, "sha384:": 96, "sha512:": 128}

// findLongHex — 16진 글자만으로 된 낱말(앞에 0x 가 붙어도)이 41자 이상이면 가린다. 다이제스트 표기(길이가 맞을 때)는 남긴다.
func findLongHex(line, _ []byte) []redactionSpan {
	var spans []redactionSpan
	for start := 0; start < len(line); {
		if !isWordByte(line[start]) {
			start++
			continue
		}
		end := runEnd(line, start, isWordByte)
		digits := line[start:end]
		if len(digits) > 2 && digits[0] == '0' && (digits[1] == 'x' || digits[1] == 'X') {
			digits = digits[2:]
		}
		if len(digits) >= longHexLength && allHex(digits) && digestLength(line[:start]) != len(digits) {
			spans = append(spans, redactionSpan{start, end})
		}
		start = end
	}
	return spans
}

func allHex(value []byte) bool {
	for _, character := range value {
		if !isHexDigit(character) {
			return false
		}
	}
	return true
}

// digestLength — 바로 앞이 다이제스트의 이름(`sha256:` 등 — 그 앞은 낱말 글자가 아니다)이면 그 이름이 요구하는 길이, 아니면 0.
func digestLength(before []byte) int {
	lowered := asciiLower(before[max(0, len(before)-8):])
	for prefix, length := range digestLengths {
		if bytes.HasSuffix(lowered, []byte(prefix)) {
			if nameStart := len(before) - len(prefix); nameStart == 0 || !isWordByte(before[nameStart-1]) {
				return length
			}
		}
	}
	return 0
}

// ---- 내부 주소 ----

// mayHoldInternalAddress — 내부 주소가 들어 있을 수 있는가. IPv4 는 대역마다 앞 글자를, IPv6 는 첫 묶음의 앞 글자와 ':' 를 본다.
func mayHoldInternalAddress(lowered []byte) bool {
	if containsAny("10.", "172.", "192.168.", "100.", "169.254.")(lowered) {
		return true
	}
	return bytes.IndexByte(lowered, ':') >= 0 && containsAny("fc", "fd", "fe8", "fe9", "fea", "feb")(lowered)
}

func isInternal(address netip.Addr) bool {
	address = address.WithZone("")
	for _, prefix := range internalPrefixes {
		if prefix.Contains(address) {
			return true
		}
	}
	return false
}

func isDigit(value byte) bool { return value >= '0' && value <= '9' }

// findInternalAddresses — 정규식 없이 훑는다: IPv4 는 점으로 이은 수 전체(넷이 아니면 주소가 아니다 —
// 3.10.0.1.4 같은 판 번호의 일부를 가리지 않는다), IPv6 는 첫 묶음이 네 자리인 fc · fd · fe 로 시작하는 낱말. 범위는 netip 로 본다.
func findInternalAddresses(line, _ []byte) []redactionSpan {
	var spans []redactionSpan
	for index := 0; index < len(line); {
		if isDigit(line[index]) {
			start, end, parts := index, index, 1
			for end < len(line) && isDigit(line[end]) {
				end++
			}
			for end+1 < len(line) && line[end] == '.' && isDigit(line[end+1]) {
				for end++; end < len(line) && isDigit(line[end]); end++ {
				}
				parts++
			}
			if parts == 4 && end-start <= 15 {
				if address, err := netip.ParseAddr(string(line[start:end])); err == nil && isInternal(address) {
					spans = append(spans, redactionSpan{start, end})
				}
			}
			index = end
			continue
		}
		if end, found := internalIPv6At(line, index); found {
			spans = append(spans, redactionSpan{index, end})
			index = end
			continue
		}
		index++
	}
	return spans
}

const (
	ipv6TextLimit = 45 // 가장 긴 IPv6 글(IPv4 를 품은 꼴)
	zoneTextLimit = 32
)

// internalIPv6At — line[start:] 이 내부 대역의 IPv6(뒤에 %zone 가능)로 시작하면 그 끝. 훑는 길이에 상한이 있다.
func internalIPv6At(line []byte, start int) (int, bool) {
	if start+5 > len(line) || line[start]|0x20 != 'f' || line[start+4] != ':' ||
		(line[start+1]|0x20 != 'c' && line[start+1]|0x20 != 'd' && line[start+1]|0x20 != 'e') ||
		!isHexDigit(line[start+2]) || !isHexDigit(line[start+3]) {
		return 0, false
	}
	if start > 0 && (isWordByte(line[start-1]) || line[start-1] == ':' || line[start-1] == '.') {
		return 0, false // 더 긴 낱말 · 주소의 가운데
	}
	end := start + 5
	for end < len(line) && end-start < ipv6TextLimit && (isHexDigit(line[end]) || line[end] == ':' || line[end] == '.') {
		end++
	}
	if end+1 < len(line) && line[end] == '%' && (isWordByte(line[end+1]) || line[end+1] == '.' || line[end+1] == '-') {
		zoneEnd := end + 1
		for zoneEnd < len(line) && zoneEnd-end <= zoneTextLimit && (isWordByte(line[zoneEnd]) || line[zoneEnd] == '.' || line[zoneEnd] == '-') {
			zoneEnd++
		}
		end = zoneEnd
	}
	for ; end > start; end-- { // 뒤에 붙은 ':' · '.' 는 주소가 아닐 수 있다 — 하나씩 떼어 다시 본다
		if address, err := netip.ParseAddr(string(line[start:end])); err == nil {
			return end, address.Is6() && isInternal(address)
		}
		if line[end-1] != ':' && line[end-1] != '.' {
			break
		}
	}
	return 0, false
}
