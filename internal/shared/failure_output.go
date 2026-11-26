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
	linePieceLimit     = 64 << 10 // 줄바꿈 없이 이보다 길면 나눠 가린다
	linePieceSpaceSpan = 4 << 10  // 나누는 자리를 찾는 조각 끝의 폭(마지막 공백 · 탭 뒤)
	redactBlockLimit   = 64 << 10 // 줄을 이만큼 모아 한 번에 거른다
	pemSwallowLimit    = 64 << 10 // END 없는 PEM 을 삼키는 상한
	oscLengthLimit     = 4 << 10  // 끝 표시 없는 OSC 를 삼키는 상한
	maskPrefix         = "[가림:"
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
	searchedLine int    // line 에서 줄바꿈을 찾아 본 곳까지
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
	if len(capture.line) > 0 {
		capture.takeSegment(capture.line, false)
		capture.line = nil
	}
	capture.flushBlock()

	redactions := make(map[string]int, len(capture.redactions))
	for kind, count := range capture.redactions {
		redactions[kind] = count
	}
	if capture.total <= FailureOutputLimit {
		return CapturedOutput{Text: string(capture.window), Redactions: redactions}
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
	return CapturedOutput{Text: text.String(), Truncated: true, OmittedBytes: omitted, Redactions: redactions}
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

// drainLines — line 에서 온전한 줄을 떼어 낸다. 줄바꿈 없이 linePieceLimit 를 넘으면 조각으로 나눈다.
func (capture *OutputCapture) drainLines() {
	for {
		if newline := bytes.IndexByte(capture.line[capture.searchedLine:], '\n'); newline >= 0 {
			end := capture.searchedLine + newline + 1
			capture.takeSegment(capture.line[:end], true)
			capture.line, capture.searchedLine = capture.line[end:], 0
			continue
		}
		capture.searchedLine = len(capture.line)
		if len(capture.line) <= linePieceLimit {
			break
		}
		cut := pieceCut(capture.line)
		capture.takeSegment(capture.line[:cut], false)
		capture.line = capture.line[cut:]
		capture.searchedLine = len(capture.line)
	}
	if len(capture.line) == 0 {
		capture.line = capture.line[:0:0] // 다 쓴 배열을 놓아준다
	}
}

// pieceCut — 줄바꿈 없는 긴 글을 나눌 자리: 첫 64KB 의 마지막 4KB 안의 마지막 공백 · 탭 뒤, 없으면 64KB 의 글자 경계.
func pieceCut(text []byte) int {
	limit := runeBoundaryBefore(text, linePieceLimit)
	if space := bytes.LastIndexAny(text[limit-linePieceSpaceSpan:limit], " \t"); space >= 0 {
		return limit - linePieceSpaceSpan + space + 1
	}
	return limit
}

var (
	pemInline = regexp.MustCompile(`-----BEGIN [A-Z0-9 ]{1,64}-----.*?-----END [A-Z0-9 ]{1,64}-----`)
	pemBegin  = regexp.MustCompile(`-----BEGIN [A-Z0-9 ]{1,64}-----`)
	pemEnd    = regexp.MustCompile(`-----END [A-Z0-9 ]{1,64}-----`)
)

// takeSegment — 온전한 줄(endsLine) 또는 긴 줄의 조각 하나. PEM 덩어리는 여기서 표시 하나로 바꾸고, 나머지는 묶음에 모은다.
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
		capture.flushBlock() // 조각은 다음 조각과 이어 가리지 않는다
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
// 묶음 전체에 한 번, 그다음 줄마다 한 번 보고, 맞을 수 있는 줄에만 apply 를 돌린다(정규식을 모든 줄에 돌리지 않는다).
// 미리 거르는 글자는 규칙이 맞는 모든 글에 반드시 들어 있는 것이어야 한다(아니면 거르기가 규칙을 가린다).
type redactionRule struct {
	kind     string
	mayMatch func(lowered []byte) bool
	apply    func(line []byte, mask func() []byte) []byte
}

var redactionRules = []redactionRule{
	{"token", containsAny("ghp_", "gho_", "ghu_", "ghs_", "ghr_", "github_pat_", "sk-", "akia", "asia", "eyj"), replaceMatches(tokenPattern)},
	{"authorization", containsAny("authorization", "bearer"), replaceGroups(authorizationPattern, bearerPattern)},
	{"url_credential", containsAny("://"), replaceGroups(urlCredentialPattern)},
	{"secret_field", containsAny(secretWords...), replaceSecretFields},
	{"hex", hasLongHexRun, replaceLongHex},
	{"email", containsAny("@"), replaceEmails},
	{"private_ip", mayHoldInternalAddress, replaceInternalAddresses},
}

var (
	tokenPattern = regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9]{36,}|github_pat_[A-Za-z0-9_]{22,}|sk-[A-Za-z0-9_-]{20,}|` +
		`(?:AKIA|ASIA)[A-Z0-9]{16}\b|eyJ[A-Za-z0-9_-]{10,}\.eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,})`)
	// Authorization 머리 · 칸: 방식(Bearer · Basic …)은 남기고 값만 가린다.
	authorizationPattern = regexp.MustCompile(`(?i)authorization["']?[ \t]*[:=][ \t]*["']?(?:(?:bearer|basic|token|digest|negotiate)[ \t]+)?([^ \t"'\n,;]+)`)
	bearerPattern        = regexp.MustCompile(`(?i)\bbearer[ \t]+([^ \t"'\n,;]+)`)
	urlCredentialPattern = regexp.MustCompile(`://([^/ \t\n"'@:]*:[^/ \t\n"'@]+)@`)
	// 비밀 낱말 바로 뒤에서 맞춰 보는 칸의 나머지: 이름의 남은 글자(점은 오지 않는다 — 비밀 낱말은 이름의 마지막 마디에 있어야 한다:
	// token_test.go:42 같은 파일 이름은 남는다) · 구분 · 값.
	secretFieldRest = regexp.MustCompile(`^[A-Za-z0-9_-]*["']?[ \t]*[:=][ \t]*(?:"([^"\n]*)"|'([^'\n]*)'|([^ \t"'&,;\n]+))`)
	emailDomain     = regexp.MustCompile(`^[A-Za-z0-9-]+(?:\.[A-Za-z0-9-]+)*\.[A-Za-z]{2,}`) // '@' 바로 뒤에서 맞춰 본다
)

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
		mask := func() []byte {
			redactions[rule.kind]++
			return []byte(maskPrefix + rule.kind + "]")
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
				if replaced := rule.apply(line, mask); !bytes.Equal(replaced, line) {
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

func isMasked(value []byte) bool { return bytes.HasPrefix(value, []byte(maskPrefix)) }

// replaceMatches — 맞은 것 전체를 가린다.
func replaceMatches(pattern *regexp.Regexp) func([]byte, func() []byte) []byte {
	return func(line []byte, mask func() []byte) []byte {
		return pattern.ReplaceAllFunc(line, func(matched []byte) []byte { return mask() })
	}
}

// replaceGroups — 맞은 것 가운데 값(잡힌 묶음 가운데 처음으로 잡힌 것)만 가린다. 빈 값 · 이미 가린 값은 그대로 둔다.
func replaceGroups(patterns ...*regexp.Regexp) func([]byte, func() []byte) []byte {
	return func(line []byte, mask func() []byte) []byte {
		for _, pattern := range patterns {
			matches := pattern.FindAllSubmatchIndex(line, -1)
			if matches == nil {
				continue
			}
			var result []byte
			previous := 0
			for _, match := range matches {
				valueStart, valueEnd := -1, -1
				for group := 2; group+1 < len(match); group += 2 {
					if match[group] >= 0 {
						valueStart, valueEnd = match[group], match[group+1]
						break
					}
				}
				if valueStart < 0 || valueStart == valueEnd || isMasked(line[valueStart:valueEnd]) {
					continue
				}
				result = append(result, line[previous:valueStart]...)
				result = append(result, mask()...)
				previous = valueEnd
			}
			if result != nil {
				line = append(result, line[previous:]...)
			}
		}
		return line
	}
}

var secretWords = []string{"password", "passwd", "secret", "token", "api_key", "apikey", "api-key",
	"access_key", "accesskey", "access-key", "private_key", "privatekey", "private-key"}

// replaceSecretFields — 이름에 비밀 낱말이 든 칸의 값을 가린다. 줄 전체에 정규식을 돌리지 않고, 낱말이 나온 자리에서만 맞춰 본다.
func replaceSecretFields(line []byte, mask func() []byte) []byte {
	lowered := asciiLower(line)
	var wordEnds []int
	for _, word := range secretWords {
		for offset := 0; ; {
			found := bytes.Index(lowered[offset:], []byte(word))
			if found < 0 {
				break
			}
			wordEnds = append(wordEnds, offset+found+len(word))
			offset += found + 1
		}
	}
	sort.Ints(wordEnds)
	var result []byte
	previous := 0
	for _, wordEnd := range wordEnds {
		if wordEnd < previous {
			continue // 앞에서 가린 값 안
		}
		match := secretFieldRest.FindSubmatchIndex(line[wordEnd:])
		if match == nil {
			continue
		}
		valueStart, valueEnd := -1, -1
		for group := 2; group+1 < len(match); group += 2 {
			if match[group] >= 0 {
				valueStart, valueEnd = wordEnd+match[group], wordEnd+match[group+1]
				break
			}
		}
		if valueStart < 0 || valueStart == valueEnd || isMasked(line[valueStart:valueEnd]) {
			continue
		}
		result = append(result, line[previous:valueStart]...)
		result = append(result, mask()...)
		previous = valueEnd
	}
	if result == nil {
		return line
	}
	return append(result, line[previous:]...)
}

func isEmailLocalByte(value byte) bool {
	return isWordByte(value) || value == '.' || value == '%' || value == '+' || value == '-'
}

// replaceEmails — 메일 주소를 가린다. '@' 가 나온 자리에서만 앞(이름)과 뒤(도메인)를 맞춰 본다.
func replaceEmails(line []byte, mask func() []byte) []byte {
	var result []byte
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
		result = append(result, line[previous:start]...)
		result = append(result, mask()...)
		previous, offset = end, end
	}
	if result == nil {
		return line
	}
	return append(result, line[previous:]...)
}

func isHexDigit(value byte) bool {
	return (value >= '0' && value <= '9') || (value >= 'a' && value <= 'f') || (value >= 'A' && value <= 'F')
}

func isWordByte(value byte) bool {
	return (value >= '0' && value <= '9') || (value >= 'a' && value <= 'z') || (value >= 'A' && value <= 'Z') || value == '_'
}

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

var digestPrefixes = [][]byte{[]byte("sha1:"), []byte("sha224:"), []byte("sha256:"), []byte("sha384:"), []byte("sha512:")}

// replaceLongHex — 16진 글자만으로 된 낱말(앞뒤가 글자 · 숫자 · _ 가 아닌)이 41자 이상이면 가린다. `sha256:` 꼴 다이제스트는 남긴다.
func replaceLongHex(line []byte, mask func() []byte) []byte {
	var result []byte
	previous := 0
	for start := 0; start < len(line); {
		if !isWordByte(line[start]) {
			start++
			continue
		}
		end, allHex := start, true
		for end < len(line) && isWordByte(line[end]) {
			allHex = allHex && isHexDigit(line[end])
			end++
		}
		if allHex && end-start >= longHexLength && !followsDigestPrefix(line[:start]) {
			result = append(result, line[previous:start]...)
			result = append(result, mask()...)
			previous = end
		}
		start = end
	}
	if result == nil {
		return line
	}
	return append(result, line[previous:]...)
}

func followsDigestPrefix(before []byte) bool {
	lowered := asciiLower(before[max(0, len(before)-8):])
	for _, prefix := range digestPrefixes {
		if bytes.HasSuffix(lowered, prefix) {
			return true
		}
	}
	return false
}

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

// replaceInternalAddresses — 내부 주소를 가린다. 정규식 없이 훑는다: IPv4 는 점으로 이은 수 전체(넷이 아니면 주소가 아니다 —
// 3.10.0.1.4 같은 판 번호의 일부를 가리지 않는다), IPv6 는 첫 묶음이 네 자리인 fc · fd · fe 로 시작하는 낱말. 범위는 netip 로 본다.
func replaceInternalAddresses(line []byte, mask func() []byte) []byte {
	var result []byte
	previous := 0
	replace := func(start, end int) {
		result = append(result, line[previous:start]...)
		result = append(result, mask()...)
		previous = end
	}
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
			if parts == 4 {
				if address, err := netip.ParseAddr(string(line[start:end])); err == nil && isInternal(address) {
					replace(start, end)
				}
			}
			index = end
			continue
		}
		if end, found := internalIPv6At(line, index); found {
			replace(index, end)
			index = end
			continue
		}
		index++
	}
	if result == nil {
		return line
	}
	return append(result, line[previous:]...)
}

// internalIPv6At — line[start:] 이 내부 대역의 IPv6(뒤에 %zone 가능)로 시작하면 그 끝.
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
	for end < len(line) && (isHexDigit(line[end]) || line[end] == ':') {
		end++
	}
	if end+1 < len(line) && line[end] == '%' && (isWordByte(line[end+1]) || line[end+1] == '.' || line[end+1] == '-') {
		for end++; end < len(line) && (isWordByte(line[end]) || line[end] == '.' || line[end] == '-'); end++ {
		}
	}
	for ; end > start; end-- { // 뒤에 붙은 ':' 는 주소가 아닐 수 있다 — 하나씩 떼어 다시 본다
		if address, err := netip.ParseAddr(string(line[start:end])); err == nil {
			return end, address.Is6() && isInternal(address)
		}
		if line[end-1] != ':' {
			break
		}
	}
	return 0, false
}
