package shared

// 실패 글의 끝부분 — 이력 · 알림 · 로그 · agent 의 실패 봉투에 싣는 끝부분은 가린 글에서 뗀다(§22).
// 계약: failure_detail_test.go 머리의 주석.

// DetailRedactionFailed — 가림이 실패(패닉)했을 때 끝부분 대신 싣는 글.
const DetailRedactionFailed = "[가림 실패 — 끝부분을 싣지 않았다]"

// CutTail — 가린 글의 끝 limit 바이트 이하를 뗀다. 가림 표시나 글자의 가운데에서 자르지 않는다.
func CutTail(masked string, limit int) string {
	return "not implemented"
}
