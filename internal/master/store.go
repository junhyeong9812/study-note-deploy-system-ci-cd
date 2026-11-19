package master

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite" // 순수 Go SQLite — CGO 없는 빌드 이미지에서 동작
)

// 배포 상태 — accepted → ci_running → cd_running → succeeded | failed
const (
	StateAccepted    = "accepted"
	StateIntegrating = "ci_running"
	StateDelivering  = "cd_running"
	StateSucceeded   = "succeeded"
	StateFailed      = "failed"
)

// Deployment — 배포 한 건. 이력 DB의 한 행이자 조회 API의 응답이다.
type Deployment struct {
	RequestID     string `json:"request_id"`
	Service       string `json:"service"`
	CommitSha     string `json:"commit_sha"`
	State         string `json:"state"`
	FailedStage   string `json:"failed_stage,omitempty"`
	Detail        string `json:"detail,omitempty"`
	AcceptedAt    string `json:"accepted_at"`
	FinishedAt    string `json:"finished_at,omitempty"`
	IntegrationMs int64  `json:"ci_ms"`
	DeliveryMs    int64  `json:"cd_ms"`
}

func (deployment Deployment) finished() bool {
	return deployment.State == StateSucceeded || deployment.State == StateFailed
}

// deploymentStore — 이력 저장 경계. 테스트는 쓰기 실패를 끼워 넣는다.
type deploymentStore interface {
	Insert(deployment Deployment) error
	Update(deployment Deployment) error
	Find(requestID string) (Deployment, bool, error)
	List(service string, limit int) ([]Deployment, error)
	FailUnfinished(failedStage, detail, finishedAt string) (int64, error)
}

type sqliteStore struct{ database *sql.DB }

const deploymentColumns = `request_id, service, commit_sha, state, failed_stage, detail, accepted_at, finished_at, ci_ms, cd_ms`

func openStore(path string) (*sqliteStore, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	database, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, err
	}
	database.SetMaxOpenConns(1) // 쓰는 주체는 master 하나 — 직렬화로 잠금 충돌을 없앤다
	_, err = database.Exec(`CREATE TABLE IF NOT EXISTS deployments (
		request_id   TEXT PRIMARY KEY,
		service      TEXT NOT NULL,
		commit_sha   TEXT NOT NULL,
		state        TEXT NOT NULL,
		failed_stage TEXT NOT NULL DEFAULT '',
		detail       TEXT NOT NULL DEFAULT '',
		accepted_at  TEXT NOT NULL,
		finished_at  TEXT NOT NULL DEFAULT '',
		ci_ms        INTEGER NOT NULL DEFAULT 0,
		cd_ms        INTEGER NOT NULL DEFAULT 0
	);
	CREATE INDEX IF NOT EXISTS deployments_service_accepted ON deployments(service, accepted_at);`)
	if err != nil {
		database.Close()
		return nil, err
	}
	return &sqliteStore{database: database}, nil
}

func (store *sqliteStore) Insert(deployment Deployment) error {
	_, err := store.database.Exec(`INSERT INTO deployments (`+deploymentColumns+`) VALUES (?,?,?,?,?,?,?,?,?,?)`,
		deployment.RequestID, deployment.Service, deployment.CommitSha, deployment.State, deployment.FailedStage,
		deployment.Detail, deployment.AcceptedAt, deployment.FinishedAt, deployment.IntegrationMs, deployment.DeliveryMs)
	return err
}

func (store *sqliteStore) Update(deployment Deployment) error {
	result, err := store.database.Exec(`UPDATE deployments SET state=?, failed_stage=?, detail=?, finished_at=?, ci_ms=?, cd_ms=? WHERE request_id=?`,
		deployment.State, deployment.FailedStage, deployment.Detail, deployment.FinishedAt,
		deployment.IntegrationMs, deployment.DeliveryMs, deployment.RequestID)
	if err != nil {
		return err
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		return errors.New("deployment row not found: " + deployment.RequestID)
	}
	return nil
}

type rowScanner interface{ Scan(dest ...any) error }

func scanDeployment(row rowScanner) (Deployment, error) {
	var deployment Deployment
	err := row.Scan(&deployment.RequestID, &deployment.Service, &deployment.CommitSha, &deployment.State,
		&deployment.FailedStage, &deployment.Detail, &deployment.AcceptedAt, &deployment.FinishedAt,
		&deployment.IntegrationMs, &deployment.DeliveryMs)
	return deployment, err
}

func (store *sqliteStore) Find(requestID string) (Deployment, bool, error) {
	deployment, err := scanDeployment(store.database.QueryRow(`SELECT `+deploymentColumns+` FROM deployments WHERE request_id=?`, requestID))
	if errors.Is(err, sql.ErrNoRows) {
		return Deployment{}, false, nil
	}
	return deployment, err == nil, err
}

func (store *sqliteStore) List(service string, limit int) ([]Deployment, error) {
	rows, err := store.database.Query(`SELECT `+deploymentColumns+` FROM deployments
		WHERE (?='' OR service=?) ORDER BY accepted_at DESC, rowid DESC LIMIT ?`, service, service, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	deployments := []Deployment{}
	for rows.Next() {
		deployment, err := scanDeployment(rows)
		if err != nil {
			return nil, err
		}
		deployments = append(deployments, deployment)
	}
	return deployments, rows.Err()
}

// FailUnfinished — 끝나지 않은 건을 실패로 닫는다. master가 죽었다 뜨면 그 건을 진행하던 goroutine은 없다.
func (store *sqliteStore) FailUnfinished(failedStage, detail, finishedAt string) (int64, error) {
	result, err := store.database.Exec(`UPDATE deployments SET state=?, failed_stage=?, detail=?, finished_at=?
		WHERE state NOT IN (?, ?)`, StateFailed, failedStage, detail, finishedAt, StateSucceeded, StateFailed)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}
