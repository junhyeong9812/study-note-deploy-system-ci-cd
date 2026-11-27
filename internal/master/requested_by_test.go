package master

// "누가 일으켰나"(requested_by) — 접수 본문의 선택 칸 · 이력의 칸 · 조회 응답의 칸 · 사람이 일으킨 접수의 알림.
// 계약:
//   - 본문 requested_by(선택, 글자). 없거나 null 이거나 빈 글자면 "github".
//   - 허용: 영문자 · 숫자 · '.' '_' ':' '-' 만, 1~128 바이트(예: github · admin:<계정> · admin:<계정>:incident:<id>).
//     '@' 는 받지 않는다 — 값이 알림 제목에 실려 메신저로 그대로 가므로 @everyone 같은 전체 호출이 될 수 있다.
//     그 밖(줄바꿈 · 제어 문자 · 공백 · ASCII 밖 글자 · 129 바이트 이상)은 422 {"error":{"code":"invalid_requested_by"}} — 접수 전에 거절.
//   - 인증(X-Deploy-Secret)은 그대로 먼저 탄다. 접수 응답(POST /deploy)의 칸은 바뀌지 않는다.
//   - 이력 표의 칸: requested_by TEXT NOT NULL DEFAULT 'github' — 옛 파일은 뜰 때 한 번 더한다(있으면 건너뜀), 더하지 못하면 New 가 오류.
//   - 조회 응답(GET /deploys/{id} · GET /deploys · GET /status 의 history)의 각 줄 맨 끝에 "requested_by" 칸.
//   - 같은 request_id 재접수는 처음 것이 남는다(덮어쓰지 않는다 — 지금의 멱등 규칙: 서비스 · 커밋이 같으면 202 duplicate).
//   - requested_by 가 github 가 아닌 접수가 새로 기록될 때만 알림 서버로 한 건: source "ci-cd" · kind "deploy_requested" ·
//     severity "info" · key "deploy-requested:<request_id>" · title 에 서비스와 requested_by · detail 에 커밋.
//     실패 알림과 같은 길(shared.AlertSender — 따로 돌고 접수 응답이 기다리지 않는다), ALERT_EVENTS_URL 이 없으면 보내지 않는다.

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/junhyeong9812/study-note-deploy-system-ci-cd/internal/shared"
)

func submitBody(t *testing.T, fields map[string]any) string {
	t.Helper()
	body, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func submitRequestedBy(t *testing.T, master *Master, requestID, requestedBy string) *httptest.ResponseRecorder {
	t.Helper()
	return call(master, "POST", "/deploy", submitBody(t, map[string]any{
		"service": "llm", "commit_sha": deploySha, "request_id": requestID, "requested_by": requestedBy,
	}))
}

// recordedRequestedBy — 조회 응답의 requested_by. 칸이 없으면 실패한다(빈 글자와 칸 없음을 가른다).
func recordedRequestedBy(t *testing.T, deploymentJSON json.RawMessage) string {
	t.Helper()
	fields := orderedFields(t, deploymentJSON)
	if len(fields) == 0 || fields[len(fields)-1].name != "requested_by" {
		t.Fatalf("requested_by 가 맨 끝 칸이 아니다: %s", deploymentJSON)
	}
	var requestedBy string
	if err := json.Unmarshal([]byte(fields[len(fields)-1].value), &requestedBy); err != nil {
		t.Fatalf("requested_by 가 글자가 아니다: %s", deploymentJSON)
	}
	return requestedBy
}

func singleRequestedBy(t *testing.T, master *Master, requestID string) string {
	t.Helper()
	return recordedRequestedBy(t, envelopeData(t, call(master, "GET", "/deploys/"+requestID, "")))
}

// alertRecorder — 알림 서버 대역. 받은 사건을 차례대로 쌓는다.
type alertRecorder struct {
	mutex  sync.Mutex
	events []map[string]string
	server *httptest.Server
}

func newAlertRecorder(t *testing.T, status int) *alertRecorder {
	recorder := &alertRecorder{}
	recorder.server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var event map[string]string
		json.NewDecoder(request.Body).Decode(&event)
		recorder.mutex.Lock()
		recorder.events = append(recorder.events, event)
		recorder.mutex.Unlock()
		writer.WriteHeader(status)
	}))
	t.Cleanup(recorder.server.Close)
	t.Setenv("ALERT_EVENTS_URL", recorder.server.URL)
	return recorder
}

func (recorder *alertRecorder) eventsOfKind(kind string) []map[string]string {
	recorder.mutex.Lock()
	defer recorder.mutex.Unlock()
	matched := []map[string]string{}
	for _, event := range recorder.events {
		if event["kind"] == kind {
			matched = append(matched, event)
		}
	}
	return matched
}

// waitEventsOfKind — 기다린 뒤 그 종류의 사건 수를 돌려준다(보내기는 따로 돈다).
func (recorder *alertRecorder) waitEventsOfKind(kind string, atLeast int, wait time.Duration) []map[string]string {
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		if events := recorder.eventsOfKind(kind); len(events) >= atLeast {
			return events
		}
		time.Sleep(5 * time.Millisecond)
	}
	return recorder.eventsOfKind(kind)
}

// waitSettled — 끝난 뒤 서비스의 진행 중 표시가 풀릴 때까지 기다린다(다음 접수가 진행 중 409 에 걸리지 않게).
// 진행 중 표시는 끝난 결과를 기록한 다음에 풀린다 — 결과가 보이는 것만으로는 다음 접수를 받는다는 보장이 없다.
func waitSettled(t *testing.T, master *Master, requestID string) Deployment {
	t.Helper()
	deployment := waitFinished(t, master, requestID)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		master.mutex.Lock()
		running := master.inFlight[deployment.Service]
		master.mutex.Unlock()
		if !running {
			return deployment
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("%s 의 진행 중 표시가 풀리지 않았다", deployment.Service)
	return deployment
}

// ---- 2. requested_by 의 기록 · 허용하는 꼴 ----

func TestSubmissionWithoutRequestedByIsRecordedAsGithub(t *testing.T) {
	newFakeAgent(t)
	master := newTestMaster(t)
	call(master, "POST", "/deploy", workflowBody("llm", "gh-910-1"))
	if requestedBy := singleRequestedBy(t, master, "gh-910-1"); requestedBy != "github" {
		t.Fatalf("requested_by 가 없는 접수는 github 로 남아야 한다: %q", requestedBy)
	}
	waitSettled(t, master, "gh-910-1")
	if requestedBy := singleRequestedBy(t, master, "gh-910-1"); requestedBy != "github" {
		t.Fatalf("끝난 뒤에도 github 여야 한다: %q", requestedBy)
	}
}

func TestEmptyOrNullRequestedByIsRecordedAsGithub(t *testing.T) {
	newFakeAgent(t)
	master := newTestMaster(t)
	for requestID, requestedBy := range map[string]any{"gh-911-1": "", "gh-911-2": nil} {
		recorder := call(master, "POST", "/deploy", submitBody(t, map[string]any{
			"service": "llm", "commit_sha": deploySha, "request_id": requestID, "requested_by": requestedBy,
		}))
		if recorder.Code != http.StatusAccepted {
			t.Fatalf("requested_by=%v 접수가 거절됐다: %d %s", requestedBy, recorder.Code, recorder.Body.String())
		}
		if recorded := singleRequestedBy(t, master, requestID); recorded != "github" {
			t.Fatalf("requested_by=%v 는 github 로 남아야 한다: %q", requestedBy, recorded)
		}
		waitSettled(t, master, requestID) // 다음 접수가 진행 중 409 에 걸리지 않게
	}
}

func TestRequestedByValueIsRecordedAsGiven(t *testing.T) {
	newFakeAgent(t)
	master := newTestMaster(t)
	for index, requestedBy := range []string{"admin:jun", "admin:jun.lee-example_1:incident:inc_42-a", "github", "admin:" + strings.Repeat("a", 122)} {
		requestID := "adm-912-" + string(rune('a'+index))
		recorder := submitRequestedBy(t, master, requestID, requestedBy)
		if recorder.Code != http.StatusAccepted {
			t.Fatalf("허용하는 꼴 %q 가 거절됐다: %d %s", requestedBy, recorder.Code, recorder.Body.String())
		}
		waitSettled(t, master, requestID)
		if recorded := singleRequestedBy(t, master, requestID); recorded != requestedBy {
			t.Fatalf("requested_by 가 그대로 남지 않았다: 받은 것 %q 기대 %q", recorded, requestedBy)
		}
	}
}

func TestSubmissionResponseFieldsStayTheSameWithRequestedBy(t *testing.T) {
	newFakeAgent(t)
	master := newTestMaster(t)
	requireExactResponse(t, submitRequestedBy(t, master, "adm-913-1", "admin:jun"), http.StatusAccepted,
		`{"data":{"request_id":"adm-913-1","service":"llm","state":"accepted"},"success":true}`)
}

func TestMalformedRequestedByIsRejectedBeforeAcceptance(t *testing.T) {
	agent := newFakeAgent(t)
	alerts := newAlertRecorder(t, http.StatusAccepted)
	master := newTestMaster(t)
	malformed := map[string]string{
		"line-feed":       "admin:jun\nforged",
		"carriage-return": "admin:jun\rforged",
		"tab":             "admin:jun\tforged",
		"nul":             "admin:jun\x00",
		"delete":          "admin:jun\x7f",
		"line-separator":  "admin:jun ",
		"space":           "admin:jun lee",
		"non-ascii":       "admin:준",
		"slash":           "admin:../jun",
		"too-long":        "admin:" + strings.Repeat("a", 123), // 129 바이트
		"at-everyone":     "admin:@everyone",
		"at-here":         "@here",
		"at-in-account":   "user@example",
	}
	for name, requestedBy := range malformed {
		t.Run(name, func(t *testing.T) {
			requestID := "bad-914-" + name
			requireExactResponse(t, submitRequestedBy(t, master, requestID, requestedBy), http.StatusUnprocessableEntity,
				`{"error":{"code":"invalid_requested_by"},"success":false}`)
			if recorder := call(master, "GET", "/deploys/"+requestID, ""); recorder.Code != http.StatusNotFound {
				t.Fatalf("거절한 접수가 이력에 남았다: %d %s", recorder.Code, recorder.Body.String())
			}
		})
	}
	time.Sleep(100 * time.Millisecond)
	if agent.called() != "" {
		t.Fatalf("거절한 접수로 agent 를 불렀다: %s", agent.called())
	}
	if events := alerts.eventsOfKind("deploy_requested"); len(events) != 0 {
		t.Fatalf("거절한 접수를 알렸다: %v", events)
	}
	// 거절이 진행 중 표시를 남기지 않는다
	if recorder := submitRequestedBy(t, master, "adm-914-ok", "admin:jun"); recorder.Code != http.StatusAccepted {
		t.Fatalf("거절 뒤의 정상 접수가 통하지 않았다: %d %s", recorder.Code, recorder.Body.String())
	}
}

func TestRequestedByOfWrongTypeIsRejected(t *testing.T) {
	master := newTestMaster(t)
	for name, rawValue := range map[string]string{"number": `5`, "boolean": `true`, "object": `{"name":"admin:jun"}`, "array": `["admin:jun"]`} {
		t.Run(name, func(t *testing.T) {
			requestID := "bad-915-" + name
			recorder := call(master, "POST", "/deploy", `{"service":"llm","commit_sha":"`+deploySha+`","request_id":"`+requestID+`","requested_by":`+rawValue+`}`)
			var envelope struct {
				Success bool `json:"success"`
				Error   struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil || recorder.Code != http.StatusUnprocessableEntity ||
				envelope.Success || envelope.Error.Code != "invalid_request" {
				t.Fatalf("글자가 아닌 requested_by 는 422 invalid_request 여야 한다: %d %s", recorder.Code, recorder.Body.String())
			}
			if lookupRecorder := call(master, "GET", "/deploys/"+requestID, ""); lookupRecorder.Code != http.StatusNotFound {
				t.Fatalf("거절한 접수가 이력에 남았다: %d", lookupRecorder.Code)
			}
		})
	}
}

func TestRequestedByDoesNotChangeAuthentication(t *testing.T) {
	master := newTestMaster(t)
	for _, body := range []string{
		`{"service":"llm","commit_sha":"` + deploySha + `","request_id":"adm-916-1","requested_by":"admin:jun"}`,
		`{"service":"llm","commit_sha":"` + deploySha + `","request_id":"adm-916-2","requested_by":"github"}`,
		`{"service":"llm","commit_sha":"` + deploySha + `","request_id":"adm-916-3","requested_by":"admin:jun\nx"}`,
	} {
		requireExactResponse(t, request(master, false, body), http.StatusUnauthorized, `{"error":{"code":"unauthorized"},"success":false}`)
		wrongSecret := httptest.NewRequest("POST", "/deploy", strings.NewReader(body))
		wrongSecret.Header.Set("X-Deploy-Secret", "other-value")
		recorder := httptest.NewRecorder()
		mux := http.NewServeMux()
		master.Register(mux)
		mux.ServeHTTP(recorder, wrongSecret)
		requireExactResponse(t, recorder, http.StatusUnauthorized, `{"error":{"code":"unauthorized"},"success":false}`)
	}
	for _, requestID := range []string{"adm-916-1", "adm-916-2", "adm-916-3"} {
		if recorder := call(master, "GET", "/deploys/"+requestID, ""); recorder.Code != http.StatusNotFound {
			t.Fatalf("무인증 접수 %s 가 이력에 남았다: %d", requestID, recorder.Code)
		}
	}
}

// ---- 3. 조회 응답에 실린다 ----

func TestEveryLookupCarriesRequestedByAsLastField(t *testing.T) {
	newFakeAgent(t)
	master := newTestMaster(t)
	call(master, "POST", "/deploy", workflowBody("llm", "gh-920-1"))
	waitSettled(t, master, "gh-920-1")
	submitRequestedBy(t, master, "adm-920-2", "admin:jun")
	waitSettled(t, master, "adm-920-2")

	want := map[string]string{"gh-920-1": "github", "adm-920-2": "admin:jun"}
	for requestID, requestedBy := range want {
		if recorded := singleRequestedBy(t, master, requestID); recorded != requestedBy {
			t.Fatalf("GET /deploys/%s 의 requested_by: 받은 것 %q 기대 %q", requestID, recorded, requestedBy)
		}
	}
	for _, listing := range []struct{ path, listName string }{{"/deploys?service=llm", "deployments"}, {"/status", "history"}} {
		deployments := listedDeployments(t, call(master, "GET", listing.path, ""), listing.listName)
		if len(deployments) != 2 {
			t.Fatalf("%s 의 건수가 다르다: %d", listing.path, len(deployments))
		}
		for _, deploymentJSON := range deployments {
			var identity struct {
				RequestID string `json:"request_id"`
			}
			json.Unmarshal(deploymentJSON, &identity)
			if recorded := recordedRequestedBy(t, deploymentJSON); recorded != want[identity.RequestID] {
				t.Fatalf("%s 의 %s requested_by: 받은 것 %q 기대 %q", listing.path, identity.RequestID, recorded, want[identity.RequestID])
			}
		}
	}
}

func TestRequestedBySurvivesRestart(t *testing.T) {
	newFakeAgent(t)
	master := newTestMaster(t)
	submitRequestedBy(t, master, "adm-921-1", "admin:jun:incident:7")
	waitSettled(t, master, "adm-921-1")
	restarted, err := New(master.logger) // 같은 DEPLOY_DB_PATH — 메모리가 아니라 표에 남았는지
	if err != nil {
		t.Fatal(err)
	}
	if recorded := singleRequestedBy(t, restarted, "adm-921-1"); recorded != "admin:jun:incident:7" {
		t.Fatalf("재시작 뒤 requested_by 가 표에 없다: %q", recorded)
	}
}

func TestRequestedByIsKeptWhenResultIsServedFromMemory(t *testing.T) {
	agent := newFakeAgent(t)
	agent.failPath, agent.failCode = "/agent/ci", "ci_test"
	master := newTestMaster(t)
	master.store = &failingStore{deploymentStore: master.store, failOnState: StateFailed}
	submitRequestedBy(t, master, "adm-922-1", "admin:jun")
	waitSettled(t, master, "adm-922-1") // 끝난 결과를 DB 에 쓰지 못해 메모리에서 답한다
	if recorded := singleRequestedBy(t, master, "adm-922-1"); recorded != "admin:jun" {
		t.Fatalf("메모리에서 답한 결과가 requested_by 를 잃었다: %q", recorded)
	}
	deployments := listedDeployments(t, call(master, "GET", "/deploys?service=llm", ""), "deployments")
	if len(deployments) != 1 || recordedRequestedBy(t, deployments[0]) != "admin:jun" {
		t.Fatalf("이력이 메모리의 결과에서 requested_by 를 잃었다: %s", deployments)
	}
}

// ---- 4. 표 변경 ----

func TestLegacyDatabaseGainsRequestedByColumnAndOldRowsReadAsGithub(t *testing.T) {
	path := writeLegacyDatabase(t)
	master, err := startMasterOn(t, path)
	if err != nil {
		t.Fatalf("옛 이력 파일로 뜨지 못했다: %v", err)
	}
	database := master.store.(*sqliteStore).database
	rows, err := database.Query(`SELECT name, type, "notnull", dflt_value FROM pragma_table_info('deployments') WHERE name = 'requested_by'`)
	if err != nil {
		t.Fatal(err)
	}
	columns := 0
	for rows.Next() {
		var name, columnType string
		var notNull int
		var defaultValue *string
		if err := rows.Scan(&name, &columnType, &notNull, &defaultValue); err != nil {
			t.Fatal(err)
		}
		columns++
		if strings.ToUpper(columnType) != "TEXT" || notNull != 1 || defaultValue == nil || *defaultValue != "'github'" {
			t.Fatalf("requested_by 칸의 꼴이 다르다: type=%q notnull=%d default=%v", columnType, notNull, defaultValue)
		}
	}
	rows.Close()
	if columns != 1 {
		t.Fatalf("옛 표에 requested_by 칸이 더해지지 않았다(칸 수 %d)", columns)
	}
	for _, requestID := range []string{"gh-800-1", "gh-800-2"} {
		if recorded := singleRequestedBy(t, master, requestID); recorded != "github" {
			t.Fatalf("옛 줄 %s 는 github 로 읽혀야 한다: %q", requestID, recorded)
		}
	}
	deployments := listedDeployments(t, call(master, "GET", "/deploys", ""), "deployments")
	for index, want := range []string{legacyFailedFields, legacySucceededFields} {
		if got := renderFields(orderedFields(t, deployments[index])); got != want+` requested_by="github"` {
			t.Fatalf("옛 줄의 조회 응답이 다르다:\n  받은 것 %s\n  기대     %s", got, want+` requested_by="github"`)
		}
	}
}

func TestStartingTwiceOnSameDatabaseAddsColumnOnce(t *testing.T) {
	path := writeLegacyDatabase(t)
	first, err := startMasterOn(t, path)
	if err != nil {
		t.Fatalf("첫 기동 실패: %v", err)
	}
	first.store.(*sqliteStore).database.Close()
	second, err := startMasterOn(t, path)
	if err != nil {
		t.Fatalf("칸이 이미 있는 표로 다시 뜨지 못했다: %v", err)
	}
	var columns int
	if err := second.store.(*sqliteStore).database.QueryRow(`SELECT count(*) FROM pragma_table_info('deployments') WHERE name = 'requested_by'`).Scan(&columns); err != nil || columns != 1 {
		t.Fatalf("requested_by 칸이 하나여야 한다: %d %v", columns, err)
	}
	if recorded := singleRequestedBy(t, second, "gh-800-1"); recorded != "github" {
		t.Fatalf("두 번째 기동 뒤 옛 줄이 github 로 읽히지 않는다: %q", recorded)
	}
	// 새 코드가 처음부터 만든 표로 다시 떠도 마찬가지다
	fresh := newTestMaster(t)
	fresh.store.(*sqliteStore).database.Close()
	if _, err := New(fresh.logger); err != nil {
		t.Fatalf("새로 만든 표로 다시 뜨지 못했다: %v", err)
	}
}

func TestNewAcceptanceOnMigratedDatabaseRecordsRequestedBy(t *testing.T) {
	agent := newFakeAgent(t)
	t.Setenv("DEPLOY_SECRET", "test-value")
	t.Setenv("DEPLOY_DB_PATH", writeLegacyDatabase(t))
	t.Setenv("AGENT_URL_LLM", agent.server.URL)
	master, err := New(shared.NewLogger())
	if err != nil {
		t.Fatalf("옛 이력 파일로 뜨지 못했다: %v", err)
	}
	submitRequestedBy(t, master, "adm-930-1", "admin:jun")
	waitSettled(t, master, "adm-930-1")
	if recorded := singleRequestedBy(t, master, "adm-930-1"); recorded != "admin:jun" {
		t.Fatalf("옛 표에 칸을 더한 뒤 새 접수의 requested_by 가 남지 않았다: %q", recorded)
	}
	if recorded := singleRequestedBy(t, master, "gh-800-1"); recorded != "github" {
		t.Fatalf("옛 줄이 github 로 읽히지 않는다: %q", recorded)
	}
}

// 옛 표의 파일에 저장소 여럿이 동시에 열려도(두 마스터가 함께 뜬 경우) 모두 뜬다 — 늦게 칸을 더하려던 쪽은
// 다른 쪽이 방금 더한 칸을 보고 정상으로 뜬다. 칸은 하나, 옛 줄은 github.
// 라운드마다 새 옛 파일을 만들고, 여는 쪽들을 한꺼번에 풀어(start 를 닫아) 칸 확인과 칸 더하기가 겹치게 한다.
func TestConcurrentStartupsOnLegacyDatabaseAllSucceed(t *testing.T) {
	const rounds = 30
	const openers = 4
	for round := 0; round < rounds; round++ {
		path := writeLegacyDatabase(t)
		start := make(chan struct{})
		stores := make([]*sqliteStore, openers)
		errs := make([]error, openers)
		var group sync.WaitGroup
		for opener := 0; opener < openers; opener++ {
			group.Add(1)
			go func(opener int) {
				defer group.Done()
				<-start
				stores[opener], errs[opener] = openStore(path)
			}(opener)
		}
		close(start)
		group.Wait()
		for opener := 0; opener < openers; opener++ {
			if errs[opener] != nil {
				for _, store := range stores {
					if store != nil {
						store.database.Close()
					}
				}
				t.Fatalf("라운드 %d: 동시에 연 저장소 %d 가 뜨지 못했다: %v", round, opener, errs[opener])
			}
		}
		for opener, store := range stores {
			var columns int
			if err := store.database.QueryRow(`SELECT count(*) FROM pragma_table_info('deployments') WHERE name = 'requested_by'`).Scan(&columns); err != nil || columns != 1 {
				t.Fatalf("라운드 %d 저장소 %d: requested_by 칸이 하나여야 한다: %d %v", round, opener, columns, err)
			}
			for _, requestID := range []string{"gh-800-1", "gh-800-2"} {
				deployment, found, err := store.Find(requestID)
				if err != nil || !found || deployment.RequestedBy != "github" {
					t.Fatalf("라운드 %d 저장소 %d: 옛 줄 %s 가 github 로 읽히지 않는다: %+v %v %v", round, opener, requestID, deployment, found, err)
				}
			}
		}
		for _, store := range stores {
			store.database.Close()
		}
	}
}

// 칸을 더하지 못하는 실제 SQLite 상황: STRICT 표에 선언과 다른 꼴의 값이 남아 있으면 ADD COLUMN 이 거절된다
// ("type mismatch on DEFAULT"). 지금 코드는 이 파일로 뜨고 읽는다 — 칸 더하기만 실패한다.
// 칸 더하기가 실패한 뒤 칸이 있는지 다시 보아도 여전히 없으므로 뜨지 않아야 한다(경합의 회복이 실패를 삼키지 않는다).
func TestStartupFailsWhenRequestedByColumnCannotBeAdded(t *testing.T) {
	path := writeLegacyDatabase(t)
	database, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`PRAGMA writable_schema = ON;
		UPDATE sqlite_schema SET sql = replace(sql, 'state        TEXT NOT NULL', 'state        INTEGER NOT NULL') || ' STRICT' WHERE name = 'deployments';
		PRAGMA writable_schema = OFF;`); err != nil {
		t.Fatal(err)
	}
	database.Close()
	master, err := startMasterOn(t, path)
	if err == nil {
		t.Fatalf("requested_by 칸을 더하지 못했는데 떴다(조용히 넘어갔다): %+v", master != nil)
	}
	if !strings.Contains(err.Error(), "requested_by") {
		t.Fatalf("기동 실패의 오류가 무엇 때문인지 말하지 않는다: %v", err)
	}
}

// ---- 5. 멱등 접수와 requested_by ----

func TestResubmissionWithDifferentRequestedByKeepsFirst(t *testing.T) {
	agent := newFakeAgent(t)
	master := newTestMaster(t)
	submitRequestedBy(t, master, "adm-940-1", "admin:alpha")
	waitSettled(t, master, "adm-940-1")
	requireExactResponse(t, call(master, "POST", "/deploy", workflowBodyWithSha("adm-940-1", deploySha)), http.StatusAccepted,
		`{"data":{"duplicate":true,"request_id":"adm-940-1","service":"llm","state":"succeeded"},"success":true}`)
	requireExactResponse(t, submitRequestedBy(t, master, "adm-940-1", "admin:beta"), http.StatusAccepted,
		`{"data":{"duplicate":true,"request_id":"adm-940-1","service":"llm","state":"succeeded"},"success":true}`)
	if recorded := singleRequestedBy(t, master, "adm-940-1"); recorded != "admin:alpha" {
		t.Fatalf("재접수가 requested_by 를 덮어썼다: %q", recorded)
	}

	call(master, "POST", "/deploy", workflowBodyWithSha("gh-940-2", deploySha))
	waitSettled(t, master, "gh-940-2")
	submitRequestedBy(t, master, "gh-940-2", "admin:beta")
	if recorded := singleRequestedBy(t, master, "gh-940-2"); recorded != "github" {
		t.Fatalf("GitHub 접수의 재접수가 requested_by 를 덮어썼다: %q", recorded)
	}
	time.Sleep(50 * time.Millisecond)
	if calls := agent.called(); calls != "/agent/ci,/agent/cd,/agent/ci,/agent/cd" {
		t.Fatalf("재접수가 배포를 한 번 더 돌렸다: %s", calls)
	}
}

func TestResubmissionWhileRunningWithDifferentRequestedByKeepsFirst(t *testing.T) {
	agent := newFakeAgent(t)
	hold := make(chan struct{})
	agent.holds = map[string]chan struct{}{"/agent/ci": hold}
	released := false
	t.Cleanup(func() {
		if !released {
			close(hold)
		}
	})
	master := newTestMaster(t)
	submitRequestedBy(t, master, "adm-941-1", "admin:alpha")
	waitState(t, master, "adm-941-1", StateIntegrating)
	if recorder := submitRequestedBy(t, master, "adm-941-1", "admin:beta"); recorder.Code != http.StatusAccepted ||
		!strings.Contains(recorder.Body.String(), `"duplicate":true`) {
		t.Fatalf("진행 중 재접수는 기존 건이어야 한다: %d %s", recorder.Code, recorder.Body.String())
	}
	close(hold)
	released = true
	waitSettled(t, master, "adm-941-1")
	if recorded := singleRequestedBy(t, master, "adm-941-1"); recorded != "admin:alpha" {
		t.Fatalf("진행 중 재접수가 requested_by 를 덮어썼다: %q", recorded)
	}
}

func workflowBodyWithSha(requestID, commitSha string) string {
	return `{"service":"llm","commit_sha":"` + commitSha + `","request_id":"` + requestID + `"}`
}

// ---- 6. 사람이 일으킨 접수의 알림 ----

func TestHumanSubmissionIsReportedToAlertServerOnce(t *testing.T) {
	alerts := newAlertRecorder(t, http.StatusAccepted)
	newFakeAgent(t)
	master := newTestMaster(t)
	submitRequestedBy(t, master, "adm-950-1", "admin:jun:incident:12")
	events := alerts.waitEventsOfKind("deploy_requested", 1, 2*time.Second)
	if len(events) != 1 {
		t.Fatalf("사람이 일으킨 접수를 한 건 알려야 한다: %v", events)
	}
	event := events[0]
	if event["source"] != "ci-cd" || event["severity"] != "info" || event["key"] != "deploy-requested:adm-950-1" ||
		!strings.Contains(event["title"], "llm") || !strings.Contains(event["title"], "admin:jun:incident:12") ||
		!strings.Contains(event["detail"], deploySha) {
		t.Fatalf("접수 알림의 내용이 다르다: %v", event)
	}
	waitSettled(t, master, "adm-950-1")
	submitRequestedBy(t, master, "adm-950-1", "admin:jun:incident:12") // 새로 고침 · 두 번 누름
	time.Sleep(200 * time.Millisecond)
	if events := alerts.eventsOfKind("deploy_requested"); len(events) != 1 {
		t.Fatalf("멱등 재접수를 다시 알렸다: %v", events)
	}
}

func TestEachHumanSubmissionGetsItsOwnAlertKey(t *testing.T) {
	alerts := newAlertRecorder(t, http.StatusAccepted)
	newFakeAgent(t)
	master := newTestMaster(t)
	submitRequestedBy(t, master, "adm-951-1", "admin:jun")
	waitSettled(t, master, "adm-951-1")
	submitRequestedBy(t, master, "adm-951-2", "admin:jun") // 같은 서비스 · 같은 커밋 · 다른 요청 — 알림 서버의 10분 억제에 묶이지 않게
	events := alerts.waitEventsOfKind("deploy_requested", 2, 2*time.Second)
	if len(events) != 2 || events[0]["key"] == events[1]["key"] {
		t.Fatalf("접수마다 다른 key 로 한 건씩이어야 한다: %v", events)
	}
}

func TestGithubSubmissionSendsNoRequestAlert(t *testing.T) {
	alerts := newAlertRecorder(t, http.StatusAccepted)
	newFakeAgent(t)
	master := newTestMaster(t)
	call(master, "POST", "/deploy", workflowBody("llm", "gh-952-1"))
	waitSettled(t, master, "gh-952-1")
	submitRequestedBy(t, master, "gh-952-2", "github")
	waitSettled(t, master, "gh-952-2")
	time.Sleep(200 * time.Millisecond)
	alerts.mutex.Lock()
	defer alerts.mutex.Unlock()
	if len(alerts.events) != 0 {
		t.Fatalf("GitHub 접수(성공)를 알렸다: %v", alerts.events)
	}
}

func TestRefusedHumanSubmissionSendsNoRequestAlert(t *testing.T) {
	alerts := newAlertRecorder(t, http.StatusAccepted)
	agent := newFakeAgent(t)
	hold := make(chan struct{})
	agent.holds = map[string]chan struct{}{"/agent/ci": hold}
	released := false
	t.Cleanup(func() {
		if !released {
			close(hold)
		}
	})
	master := newTestMaster(t)
	call(master, "POST", "/deploy", workflowBody("llm", "gh-953-1"))
	waitState(t, master, "gh-953-1", StateIntegrating)
	if recorder := submitRequestedBy(t, master, "adm-953-2", "admin:jun"); recorder.Code != http.StatusConflict {
		t.Fatalf("진행 중이면 409 여야 한다: %d %s", recorder.Code, recorder.Body.String())
	}
	if recorder := call(master, "POST", "/deploy", submitBody(t, map[string]any{
		"service": "llm", "commit_sha": "fff9999", "request_id": "gh-953-1", "requested_by": "admin:jun",
	})); recorder.Code != http.StatusConflict {
		t.Fatalf("같은 번호 · 다른 커밋은 409 여야 한다: %d %s", recorder.Code, recorder.Body.String())
	}
	close(hold)
	released = true
	waitSettled(t, master, "gh-953-1")

	master.store = &failingStore{deploymentStore: master.store, failInsert: true}
	if recorder := submitRequestedBy(t, master, "adm-953-3", "admin:jun"); recorder.Code != http.StatusInternalServerError {
		t.Fatalf("기록하지 못하면 500 이어야 한다: %d %s", recorder.Code, recorder.Body.String())
	}
	time.Sleep(200 * time.Millisecond)
	if events := alerts.eventsOfKind("deploy_requested"); len(events) != 0 {
		t.Fatalf("접수되지 않은 요청을 알렸다: %v", events)
	}
}

func TestFailedHumanDeploymentReportsRequestAndFailure(t *testing.T) {
	alerts := newAlertRecorder(t, http.StatusAccepted)
	agent := newFakeAgent(t)
	agent.failPath, agent.failCode = "/agent/ci", "ci_test"
	master := newTestMaster(t)
	submitRequestedBy(t, master, "adm-954-1", "admin:jun")
	waitSettled(t, master, "adm-954-1")
	requested := alerts.waitEventsOfKind("deploy_requested", 1, 2*time.Second)
	failed := alerts.waitEventsOfKind("deploy_failed", 1, 2*time.Second)
	if len(requested) != 1 || len(failed) != 1 || !strings.Contains(failed[0]["title"], "ci_test") {
		t.Fatalf("접수 알림 한 건과 실패 알림 한 건이어야 한다: requested=%v failed=%v", requested, failed)
	}
}

func TestSlowAlertServerDoesNotDelaySubmission(t *testing.T) {
	release := make(chan struct{})
	reached := make(chan struct{}, 4)
	stalled := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		reached <- struct{}{}
		<-release
		writer.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(stalled.Close)
	t.Cleanup(func() { close(release) }) // 서버를 닫기 전에 붙잡힌 요청을 푼다
	t.Setenv("ALERT_EVENTS_URL", stalled.URL)
	newFakeAgent(t)
	master := newTestMaster(t)
	started := time.Now()
	recorder := submitRequestedBy(t, master, "adm-955-1", "admin:jun")
	elapsed := time.Since(started)
	if recorder.Code != http.StatusAccepted || elapsed > 500*time.Millisecond {
		t.Fatalf("느린 알림 서버가 접수를 늦췄다: %d %v %s", recorder.Code, elapsed, recorder.Body.String())
	}
	select {
	case <-reached:
	case <-time.After(2 * time.Second):
		t.Fatal("접수 알림을 보내지 않았다")
	}
	if requestedBy := singleRequestedBy(t, master, "adm-955-1"); requestedBy != "admin:jun" {
		t.Fatalf("접수가 기록되지 않았다: %q", requestedBy)
	}
}

func TestFailingAlertServerDoesNotBlockSubmission(t *testing.T) {
	for name, configure := range map[string]func(t *testing.T){
		"server-error": func(t *testing.T) { newAlertRecorder(t, http.StatusInternalServerError) },
		"unreachable":  func(t *testing.T) { t.Setenv("ALERT_EVENTS_URL", "http://127.0.0.1:1/events") },
		"not-configured": func(t *testing.T) {
			t.Setenv("ALERT_EVENTS_URL", "")
		},
	} {
		t.Run(name, func(t *testing.T) {
			configure(t)
			newFakeAgent(t)
			master := newTestMaster(t)
			requireExactResponse(t, submitRequestedBy(t, master, "adm-956-1", "admin:jun"), http.StatusAccepted,
				`{"data":{"request_id":"adm-956-1","service":"llm","state":"accepted"},"success":true}`)
			if deployment := waitSettled(t, master, "adm-956-1"); deployment.State != StateSucceeded {
				t.Fatalf("알림을 못 보냈다고 배포가 바뀌었다: %+v", deployment)
			}
			if requestedBy := singleRequestedBy(t, master, "adm-956-1"); requestedBy != "admin:jun" {
				t.Fatalf("알림을 못 보냈다고 기록이 바뀌었다: %q", requestedBy)
			}
		})
	}
}
