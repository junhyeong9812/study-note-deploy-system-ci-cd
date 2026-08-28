package shared

import (
	"net/http/httptest"
	"testing"
)

func TestSecretMatches(t *testing.T) {
	request := httptest.NewRequest("POST", "/deploy", nil)
	request.Header.Set(SecretHeader, "right")
	if !SecretMatches(request, "right") {
		t.Fatal("일치하는 시크릿이 거절됨")
	}
	if SecretMatches(request, "different") {
		t.Fatal("불일치 시크릿이 통과됨")
	}
	if SecretMatches(request, "") {
		t.Fatal("빈 설정은 fail-closed여야 한다") // 설정 누락 = 전부 거절
	}
}

func TestAcceptOrIssue(t *testing.T) {
	if AcceptOrIssue("gh-12345") != "gh-12345" {
		t.Fatal("정상 id가 재발행됨")
	}
	if issued := AcceptOrIssue("has:colon"); issued == "has:colon" {
		t.Fatal("콜론 포함 id는 로그 포맷을 깨므로 재발행돼야 한다")
	}
	if AcceptOrIssue("") == "" {
		t.Fatal("빈 id는 발행돼야 한다")
	}
}
