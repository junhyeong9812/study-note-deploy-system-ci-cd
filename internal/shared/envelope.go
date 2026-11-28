// Package shared — 봉투·로그·HMAC 등 master/agent 공통 규약 구현.
package shared

import (
	"encoding/json"
	"net/http"
)

// 응답 봉투 — llm·backend와 동일 규약: success 플래그 하나로 분기한다.

func WriteOK(writer http.ResponseWriter, data any) {
	writeJSON(writer, http.StatusOK, map[string]any{"success": true, "data": data})
}

func WriteAccepted(writer http.ResponseWriter, data any) {
	writeJSON(writer, http.StatusAccepted, map[string]any{"success": true, "data": data})
}

func WriteFail(writer http.ResponseWriter, status int, code string, detail string) {
	writeJSON(writer, status, map[string]any{"success": false, "error": failureBody(code, detail)})
}

// WriteFailWithOutput — WriteFail 에 가리고 줄인 전체 출력(error.output)을 더한다.
func WriteFailWithOutput(writer http.ResponseWriter, status int, code string, detail string, output CapturedOutput) {
	errorBody := failureBody(code, detail)
	errorBody["output"] = output
	writeJSON(writer, status, map[string]any{"success": false, "error": errorBody})
}

func failureBody(code, detail string) map[string]any {
	errorBody := map[string]any{"code": code}
	if detail != "" {
		errorBody["detail"] = detail
	}
	return errorBody
}

func writeJSON(writer http.ResponseWriter, status int, body any) {
	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(body)
}
