package master

// 배포 실패의 전체 출력 — agent 단계가 실패한 배포 한 건에 파일 하나(`<요청 ID>.json`)를 이력 옆 폴더에 남기고, 요청 ID 로 조회한다.
// 계약: failure_output_test.go 머리의 주석.

import (
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/junhyeong9812/study-note-deploy-system-ci-cd/internal/shared"
)

const (
	failureOutputRetention     = 14 * 24 * time.Hour // 남긴 때부터 이만큼 지나면 지운 것
	failureOutputKeptCount     = 50                  // 출력이 든 파일은 최근 이만큼만
	expiredMarkerRetention     = 90 * 24 * time.Hour // 지운 자리의 표지는 지운 때부터 이만큼
	failureOutputFileLimit     = 4 << 20             // 저장한 파일 하나를 읽는 상한(가린 뒤 256KB 가 JSON 으로 늘어난 것보다 넉넉히)
	failureOutputDirectoryName = "failure-outputs"   // DEPLOY_OUTPUT_DIR 이 비면 이력 DB 의 폴더 아래 이 이름
	staleTemporaryAge          = time.Hour           // 쓰다 죽어 남은 임시 파일을 지우는 나이
)

// temporaryOutputName — writeFileAtomically 가 만드는 임시 파일의 이름 `.<요청 ID>.json.<무엇이든>.tmp`.
var temporaryOutputName = regexp.MustCompile(`^\.[A-Za-z0-9_-]{1,64}\.json\..+\.tmp$`)

// failureOutput — 저장하는 파일의 꼴이자 조회 응답의 data.
type failureOutput struct {
	RequestID    string         `json:"request_id"`
	Service      string         `json:"service"`
	CommitSha    string         `json:"commit_sha"`
	FailedStage  string         `json:"failed_stage"`
	RecordedAt   string         `json:"recorded_at"`
	Output       string         `json:"output"`
	Truncated    bool           `json:"truncated"`
	OmittedBytes int64          `json:"omitted_bytes"`
	Bytes        int            `json:"bytes"`
	Redactions   map[string]int `json:"redactions"`
}

// failureOutputFile — 저장하는 파일: 조회 응답의 칸 + 저장 번호(남긴 차례 — 조회 응답에는 싣지 않는다).
type failureOutputFile struct {
	failureOutput
	Sequence int64 `json:"sequence"`
}

// expiredFailureOutput — 지운 자리의 표지(출력 없음). 410 "지워짐" 과 404 "남기지 못함" 을 가른다.
type expiredFailureOutput struct {
	RequestID   string `json:"request_id"`
	FailedStage string `json:"failed_stage"`
	RecordedAt  string `json:"recorded_at"`
	ExpiredAt   string `json:"expired_at"`
	Sequence    int64  `json:"sequence"`
}

// storedFailureOutput — 읽은 파일: 출력이 있으면 Output 이, 표지면 ExpiredAt 이 찬다. 번호가 없는 파일(이 칸 이전의 꼴)은 0.
type storedFailureOutput struct {
	failureOutput
	Output    *string `json:"output"`
	ExpiredAt string  `json:"expired_at"`
	Sequence  int64   `json:"sequence"`
}

func failureOutputDirectory(databasePath string) string {
	if directory := os.Getenv("DEPLOY_OUTPUT_DIR"); directory != "" {
		return directory
	}
	return filepath.Join(filepath.Dir(databasePath), failureOutputDirectoryName)
}

// prepareFailureOutputs — 뜰 때 폴더를 만들고 보관 기간을 넘은 것을 정리한다. 못 해도 master 는 뜬다(로그 한 줄 — 남길 때 다시 실패를 드러낸다).
func (master *Master) prepareFailureOutputs() {
	err := os.MkdirAll(master.outputDirectory, 0o700)
	if err == nil {
		err = os.Chmod(master.outputDirectory, 0o700) // 이미 있던 폴더도
	}
	if err != nil {
		master.logger.Log("boot", "failure output directory not ready: "+err.Error(), "warning")
		return
	}
	master.outputMutex.Lock()
	defer master.outputMutex.Unlock()
	master.cleanFailureOutputs("boot") // 폴더의 가장 큰 저장 번호도 여기서 읽는다
}

// keepFailureOutput — agent 단계의 실패 출력을 가린 뒤 남긴다(agent 가 가렸어도 한 번 더 — agent 를 믿지 않는다).
// 실패가 조회에 보이기 전에(recordFinished 앞에서) 부른다. 남기지 못해도 배포의 결과 · 이력 · 알림은 그대로다 — 로그 한 줄(내용 없음).
func (master *Master) keepFailureOutput(deployment Deployment, stage, detail string, agentOutput *shared.CapturedOutput) {
	if !agentStages[stage] {
		return
	}
	if agentOutput == nil && detail == shared.DetailRedactionFailed { // agent 의 가림이 실패했다 — 그 고정 글은 출력이 아니다
		return
	}
	source := detail // 옛 agent — 끝 2000바이트뿐이다
	if agentOutput != nil && agentOutput.Text != "" {
		source = agentOutput.Text
	}
	captured, redacted, panicReason := shared.RedactSafely(master.redact, source)
	if !redacted { // 가리지 못한 글은 남기지 않는다
		master.logger.Log(deployment.RequestID, "redaction panicked: "+panicReason, "error")
		master.logger.Log(deployment.RequestID, "failure output not saved: redaction failed", "warning")
		return
	}
	if agentOutput != nil && agentOutput.Text != "" { // agent 가 이미 줄이고 가린 몫을 더한다
		captured.Truncated = captured.Truncated || agentOutput.Truncated
		captured.OmittedBytes += max(agentOutput.OmittedBytes, 0)
		for kind, count := range agentOutput.Redactions {
			if count > 0 {
				captured.Redactions[kind] += count
			}
		}
	}
	record := failureOutput{RequestID: deployment.RequestID, Service: deployment.Service, CommitSha: deployment.CommitSha,
		FailedStage: stage, RecordedAt: master.clock().UTC().Format(time.RFC3339Nano), Output: captured.Text,
		Truncated: captured.Truncated, OmittedBytes: captured.OmittedBytes, Bytes: len(captured.Text), Redactions: captured.Redactions}

	master.outputMutex.Lock()
	defer master.outputMutex.Unlock()
	sequence := max(master.nextSequence, 1)
	master.nextSequence = sequence + 1
	if err := writeFileAtomically(master.outputDirectory, deployment.RequestID+".json", failureOutputFile{record, sequence}); err != nil {
		master.logger.Log(deployment.RequestID, "failure output not saved: "+err.Error(), "warning")
	}
	master.cleanFailureOutputs(deployment.RequestID)
}

// writeFileAtomically — 같은 폴더의 임시 파일(0600)에 쓴 뒤 이름을 바꾼다. 읽는 쪽은 반쯤 쓴 파일을 보지 않는다.
func writeFileAtomically(directory, name string, value any) error {
	content, err := json.Marshal(value)
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp(directory, "."+name+".*.tmp") // 점으로 시작 — 요청 ID 꼴이 아니라 정리 · 조회가 보지 않는다
	if err != nil {
		return err
	}
	_, err = temporary.Write(content)
	if err == nil {
		err = temporary.Sync()
	}
	if closeErr := temporary.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(temporary.Name(), filepath.Join(directory, name))
	}
	if err != nil {
		os.Remove(temporary.Name())
	}
	return err
}

// readFailureOutput — `<요청 ID>.json` 을 읽는다. 일반 파일만(심볼릭 링크를 따라가지 않는다 — 연 뒤 같은 파일인지 다시 본다).
func readFailureOutput(directory, requestID string) (storedFailureOutput, error) {
	var stored storedFailureOutput
	path := filepath.Join(directory, requestID+".json") // requestID 는 검증한 꼴 — 경로 구분자가 없다
	info, err := os.Lstat(path)
	if err != nil {
		return stored, err
	}
	if !info.Mode().IsRegular() {
		return stored, errors.New("not a regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return stored, err
	}
	defer file.Close()
	if opened, err := file.Stat(); err != nil || !os.SameFile(info, opened) {
		return stored, errors.New("file changed while opening")
	}
	content, err := io.ReadAll(io.LimitReader(file, failureOutputFileLimit+1))
	if err != nil {
		return stored, err
	}
	if len(content) > failureOutputFileLimit {
		return stored, errors.New("file too large")
	}
	return stored, json.Unmarshal(content, &stored)
}

// cleanFailureOutputs — 보관(14일 또는 최근 50건)을 넘은 출력을 표지로 바꾸고, 90일이 지난 표지와 1시간 넘은 임시 파일을 지운다.
// `<요청 ID 꼴>.json` 인 일반 파일 가운데 읽을 수 있는 것만 다룬다. master.outputMutex 를 쥐고 부른다.
// "최근 50건"은 저장 번호로 가른다(벽시계가 아니다). 기한(14일 · 표지 90일)은 적힌 시각으로 본다 — 미래의 시각은 지난 시간이 0 아래라
// 기한으로 지우지 않는다. 읽은 파일의 가장 큰 번호(표지 포함)로 다음 번호를 잇는다. requestID 는 로그 줄의 요청 ID 다.
func (master *Master) cleanFailureOutputs(requestID string) {
	entries, err := os.ReadDir(master.outputDirectory)
	if err != nil {
		master.logger.Log(requestID, "failure output cleanup failed: "+err.Error(), "warning")
		return
	}
	now := master.clock()
	type keptOutput struct {
		requestID  string
		recordedAt time.Time
		stored     storedFailureOutput
	}
	var outputs []keptOutput
	for _, entry := range entries {
		if temporaryOutputName.MatchString(entry.Name()) && entry.Type().IsRegular() {
			if info, err := entry.Info(); err == nil && now.Sub(info.ModTime()) > staleTemporaryAge {
				if err := os.Remove(filepath.Join(master.outputDirectory, entry.Name())); err != nil {
					master.logger.Log(requestID, "failure output cleanup failed: "+err.Error(), "warning")
				}
			}
			continue
		}
		name, isJSON := strings.CutSuffix(entry.Name(), ".json")
		if !isJSON || !shared.ValidRequestID(name) || !entry.Type().IsRegular() {
			continue
		}
		stored, err := readFailureOutput(master.outputDirectory, name)
		if err != nil {
			continue
		}
		master.nextSequence = max(master.nextSequence, stored.Sequence+1)
		if stored.ExpiredAt != "" {
			if expiredAt, err := time.Parse(time.RFC3339Nano, stored.ExpiredAt); err == nil && now.Sub(expiredAt) > expiredMarkerRetention {
				if err := os.Remove(filepath.Join(master.outputDirectory, entry.Name())); err != nil {
					master.logger.Log(requestID, "failure output cleanup failed: "+err.Error(), "warning")
				}
			}
			continue
		}
		recordedAt, err := time.Parse(time.RFC3339Nano, stored.RecordedAt)
		if stored.Output == nil || err != nil {
			continue
		}
		outputs = append(outputs, keptOutput{name, recordedAt, stored})
	}
	sort.Slice(outputs, func(left, right int) bool { // 저장 번호가 큰 것(나중에 남긴 것)부터
		first, second := outputs[left], outputs[right]
		if first.stored.Sequence != second.stored.Sequence {
			return first.stored.Sequence > second.stored.Sequence
		}
		return first.requestID > second.requestID
	})
	for index, output := range outputs {
		if index < failureOutputKeptCount && now.Sub(output.recordedAt) <= failureOutputRetention {
			continue
		}
		marker := expiredFailureOutput{RequestID: output.requestID, FailedStage: output.stored.FailedStage,
			RecordedAt: output.stored.RecordedAt, ExpiredAt: now.UTC().Format(time.RFC3339Nano), Sequence: output.stored.Sequence}
		if err := writeFileAtomically(master.outputDirectory, output.requestID+".json", marker); err != nil {
			master.logger.Log(requestID, "failure output cleanup failed: "+err.Error(), "warning")
		}
	}
}

// handleFailureOutput — GET /deploys/{requestID}/output. 상태 · 코드의 표는 failure_output_test.go 머리.
func (master *Master) handleFailureOutput(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("X-Content-Type-Options", "nosniff")
	if !shared.SecretMatches(request, master.secret) {
		shared.WriteFail(writer, http.StatusUnauthorized, "unauthorized", "")
		return
	}
	requestID := request.PathValue("requestID")
	if !shared.ValidRequestID(requestID) { // 경로를 잇기 전에 — 꼴 밖의 값은 파일 이름이 되지 않는다
		shared.WriteFail(writer, http.StatusUnprocessableEntity, "invalid_request_id", "")
		return
	}
	deployment, found, err := master.store.Find(requestID)
	if err != nil {
		shared.WriteFail(writer, http.StatusInternalServerError, StageStore, "")
		return
	}
	if !found {
		shared.WriteFail(writer, http.StatusNotFound, "not_found", "")
		return
	}
	master.mutex.Lock()
	deployment = master.actual(deployment)
	master.mutex.Unlock()
	if !deployment.finished() {
		shared.WriteFail(writer, http.StatusConflict, "not_finished", "")
		return
	}
	if deployment.State != StateFailed {
		shared.WriteFail(writer, http.StatusConflict, "not_failed", "")
		return
	}

	stored, err := readFailureOutput(master.outputDirectory, requestID)
	if errors.Is(err, fs.ErrNotExist) { // master 단계의 실패 · 남기기 실패 · 이 기능 전의 실패
		shared.WriteFail(writer, http.StatusNotFound, "output_not_recorded", "")
		return
	}
	if err != nil {
		shared.WriteFail(writer, http.StatusInternalServerError, "output_read_failed", "")
		return
	}
	now := master.clock()
	if stored.ExpiredAt != "" {
		expiredAt, err := time.Parse(time.RFC3339Nano, stored.ExpiredAt)
		switch {
		case err != nil:
			shared.WriteFail(writer, http.StatusInternalServerError, "output_read_failed", "")
		case now.Sub(expiredAt) > expiredMarkerRetention: // 정리 전이라도 표지의 기한이 지났다
			shared.WriteFail(writer, http.StatusNotFound, "output_not_recorded", "")
		default:
			shared.WriteFail(writer, http.StatusGone, "output_expired", "")
		}
		return
	}
	recordedAt, err := time.Parse(time.RFC3339Nano, stored.RecordedAt)
	if stored.Output == nil || err != nil {
		shared.WriteFail(writer, http.StatusInternalServerError, "output_read_failed", "")
		return
	}
	if now.Sub(recordedAt) > failureOutputRetention { // 지우기 전이라도 지운 것으로 답한다
		shared.WriteFail(writer, http.StatusGone, "output_expired", "")
		return
	}
	record := stored.failureOutput
	record.Output = *stored.Output
	shared.WriteOK(writer, record)
}
