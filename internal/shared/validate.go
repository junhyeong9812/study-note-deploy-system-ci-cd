package shared

import "regexp"

// 커밋 해시만 허용 — 셸 주입·로그 주입·임의 ref 차단 (master가 접수 전에 선검증, 리뷰 F5)
var validSha = regexp.MustCompile(`^[0-9a-f]{7,64}$`)

func ValidCommitSha(value string) bool { return validSha.MatchString(value) }
