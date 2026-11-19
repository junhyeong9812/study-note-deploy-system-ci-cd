# study-note-deploy-system-ci-cd

Go 배포 서버 — **단일 바이너리, `.env`의 MODE로 master/agent 전환**.

```
리포 Actions(main push) ──접수──▶ www/api/deploy (front 중계, 비밀 패스스루)
        │                                 ▼
        │                  master (.9:15000) — 서비스 allowlist · 멱등 접수 · 진행 · 이력(SQLite)
        │                         │ ① POST /agent/ci           ② (CI 성공 시에만) POST /agent/cd
        │                         ▼ HTTP(LAN)
        │                  agent (각 호스트 :15001, docker.sock 컨테이너)
        │                    CI: git fetch → reset --hard <sha> → docker build --target test → compose build
        │                        (실패하면 소스를 직전 커밋으로 되돌림 — 운영 컨테이너는 그대로)
        │                    CD: IMAGE_TAG=<sha> compose up -d --no-build --no-deps <대상> → 헬스 확인
        └──조회── www/api/deploy/<request_id> ──▶ master GET /deploys/<request_id>
                 (10초 간격, succeeded 일 때만 초록불)
```

## 상태와 API

- 상태: `accepted → ci_running → cd_running → succeeded | failed`. `failed` 에는 `failed_stage` 와 출력 끝부분이 붙는다.
- 실패 단계: `ci_fetch` · `ci_test` · `ci_build` · `cd_up` · `cd_health` · `agent_unreachable` · `agent_busy` · `store_failed` · `interrupted`(master 재시작)

| API | 설명 |
|---|---|
| `POST /deploy` | 접수(202). 같은 `request_id` 를 다시 보내면 새 배포 없이 기존 건의 상태를 돌려준다. 같은 서비스가 진행 중이면 409 |
| `GET /deploys/{request_id}` | 한 건의 상태 |
| `GET /deploys?service=&limit=` | 이력(최신순, 기본 50·최대 200) |
| `GET /status` | 서비스 목록 + 최근 50건 |

## 대상 repo가 지켜야 할 것

- Dockerfile 에 `test` 단계 — agent 는 `docker build --target test <경로>` 만 실행한다. 무엇을 돌릴지는 repo 가 정한다.
- compose 대상 서비스에 `image: <이름>:${IMAGE_TAG:-current}` — CI 가 sha 태그로 빌드하고 CD 가 그 태그로 교체한다.
- 되돌리기: 이전 커밋으로 접수를 다시 보낸다. 서비스별 최근 5개 sha 이미지를 남긴다.

- 규약: 봉투(success/error) · 로그 `requestId:server-name:message`(stdout+Redis XADD 실패무해)
- 가드: fail-closed 시크릿(상수시간) · 서비스/디렉토리 고정 열거 · 커밋 sha hex 검증(**master가 접수 전에**) · master 서비스별 + agent single-flight · CI 실패 시 CD 미호출 · 배포 후 헬스 확인(DEPLOY_HEALTH_*) · 이력을 기록하지 못하면 접수 거절 · http 타임아웃
- 수용 리스크(명문화): git-pull 방식은 저장소 커밋이 compose를 바꿀 수 있다 — main의 PR 게이트·자기 소유 repo가 신뢰 경계. HMAC+타임스탬프(재전송 방지)는 후속 이슈
- 구동: `.env` 작성 후 `docker compose up -d --build` · 테스트: `go test ./...`
