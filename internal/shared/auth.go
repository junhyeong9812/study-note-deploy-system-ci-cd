package shared

import (
	"crypto/subtle"
	"net/http"
)

const SecretHeader = "X-Deploy-Secret"

// SecretMatches — 상수시간 비교. 빈 설정은 항상 거절(fail-closed).
func SecretMatches(request *http.Request, configured string) bool {
	if configured == "" {
		return false
	}
	provided := request.Header.Get(SecretHeader)
	return subtle.ConstantTimeCompare([]byte(provided), []byte(configured)) == 1
}
