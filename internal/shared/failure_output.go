package shared

// 배포 실패의 전체 출력 — 가린 뒤 크기 상한 안으로 줄인다(agent 는 보내기 전에, master 는 저장하기 전에 같은 것을 쓴다).
// 계약: failure_output_test.go 머리의 주석.

import "errors"

const (
	FailureOutputLimit     = 256 << 10 // 가린 뒤 남기는 전체 상한(가운데를 버렸다는 표시 줄 포함)
	FailureOutputHeadLimit = 32 << 10  // 넘을 때 앞에서 남기는 몫
)

// CapturedOutput — 가리고 줄인 출력. agent 의 실패 봉투(error.output)에 이 꼴로 실린다.
type CapturedOutput struct {
	Text         string         `json:"text"`
	Truncated    bool           `json:"truncated"`
	OmittedBytes int64          `json:"omitted_bytes"`
	Redactions   map[string]int `json:"redactions"` // 가린 종류 → 횟수
}

// OutputCapture — 출력을 흘려 받으며 가리고 줄인다(전체를 메모리에 올리지 않는다).
type OutputCapture struct{}

func NewOutputCapture() *OutputCapture { return &OutputCapture{} }

func (capture *OutputCapture) Write(chunk []byte) (int, error) {
	return 0, errors.New("not implemented")
}

func (capture *OutputCapture) Finish() CapturedOutput {
	return CapturedOutput{Text: "not implemented", Redactions: map[string]int{}}
}

// CaptureOutput — 글 하나를 한 번에 가리고 줄인다.
func CaptureOutput(raw string) CapturedOutput {
	return CapturedOutput{Text: "not implemented", Redactions: map[string]int{}}
}
