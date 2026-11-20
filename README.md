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
- 실패 단계: `ci_fetch` · `ci_test` · `ci_build` · `cd_up` · `cd_health` · `agent_unreachable` · `agent_rejected`(agent 가 단계 전에 거절 — 설정 누락·계속 작업 중. 사유는 `detail`) · `store_failed`(이력 기록 실패) · `interrupted`(master 재시작)
- 시간 예산: agent 작업 중 대기 3분 + CI 11.5분 + CD 5분 — 워크플로 조회 상한 20분 안

| API | 설명 |
|---|---|
| `POST /deploy` | 접수(202). 같은 `request_id` 를 다시 보내면 새 배포 없이 기존 건의 상태를 돌려준다. 같은 서비스가 진행 중이면 409 |
| `GET /deploys/{request_id}` | 한 건의 상태 |
| `GET /deploys?service=&limit=` | 이력(최신순, 기본 50·최대 200) |
| `GET /status` | 서비스 목록 + 최근 50건(최신순, 원소는 `/deploys` 와 같은 형태) |

## 대상 repo가 지켜야 할 것

- Dockerfile 에 `test` 단계 — agent 는 `docker build --target test <경로>` 만 실행한다. 무엇을 돌릴지는 repo 가 정한다.
- compose 대상 서비스에 `image: <이름>:${IMAGE_TAG:-local}` — CI 가 sha 태그로 빌드하고 CD 가 그 태그로 교체한다.
- 되돌리기: 이전 커밋으로 접수를 다시 보낸다. 서비스별 최근 5개 sha 이미지를 남긴다(이미지 이름은 실행 중인 컨테이너에서 읽는다).
- 서버에서 손으로 올릴 때: `IMAGE_TAG=<운영 중인 sha> docker compose up -d --no-build --no-deps <대상>`. `IMAGE_TAG` 없이 `up` 하면 `local` 태그로 그 자리에서 빌드해 컨테이너를 다시 만든다.

- 규약: 봉투(success/error) · 로그 `requestId:server-name:message`(stdout+Redis XADD 실패무해)
- 가드: fail-closed 시크릿(상수시간) · 서비스/디렉토리 고정 열거 · 커밋 sha hex 검증(**master가 접수 전에**) · master 서비스별 + agent single-flight · CI 실패 시 CD 미호출 · 배포 후 헬스 확인(DEPLOY_HEALTH_*) · 이력을 기록하지 못하면 접수 거절 · http 타임아웃
- 수용 리스크(명문화): git-pull 방식은 저장소 커밋이 compose를 바꿀 수 있다 — main의 PR 게이트·자기 소유 repo가 신뢰 경계. HMAC+타임스탬프(재전송 방지)는 후속 이슈
- 구동: `.env` 작성 후 `docker compose up -d --build` · 테스트: `go test ./...`


## 무중단 배포(blue/green) — 서비스별로 켠다

`DEPLOY_MODE_<SVC>=bluegreen` 인 서비스는 앱을 두 자리(`<기준>-blue`·`<기준>-green`)로 두고 nginx 가 그중 하나로 보낸다. 켜지 않은 서비스는 위의 단일 컨테이너 방식 그대로다.

- agent 설정: `DEPLOY_MODE_<SVC>=bluegreen` · `DEPLOY_HEALTH_<SVC>`(nginx 경유 외부 포트) · `DEPLOY_HEALTH_<SVC>_BLUE`·`_GREEN`(자리별 127.0.0.1 포트) · `DEPLOY_DRAIN_<SVC>_BLUE`·`_GREEN`(선택 — 구 자리에 정리 시작을 알리는 URL)
- 서버의 `deploy/active/`(git 에 없다)가 nginx 가 읽는 전부다: `nginx.conf`(repo `deploy/nginx/nginx.conf` 의 운영 사본) + `upstream.conf`(활성 자리, 첫 줄 `# slot: blue|green`). agent 가 전환할 때 둘을 갱신하고, 실패하면 이전 내용으로 되돌린다.
- 판정 기준은 파일이 아니라 nginx 응답의 `X-Deploy-Slot` 헤더다.

### 최초 전환(서버에서 한 번, 수 초 중단)
```
cd <배포 디렉토리>
mkdir -p deploy/active
cp deploy/nginx/nginx.conf deploy/active/nginx.conf
cp deploy/nginx/upstream-blue.conf deploy/active/upstream.conf
docker stop <기존 앱 컨테이너>            # 외부 포트를 nginx 에 넘긴다
IMAGE_TAG=<운영 중인 sha> docker compose up -d --no-build <기준>-blue nginx
curl -sD- -o /dev/null <외부 헬스 URL> | grep -i x-deploy-slot   # blue
```

### 손으로 확인·조작
```
curl -sD- -o /dev/null <외부 헬스 URL> | grep -i x-deploy-slot     # 지금 응답하는 자리
head -1 deploy/active/upstream.conf                               # 파일이 가리키는 자리
docker compose --profile blue --profile green ps                  # 떠 있는 자리(평소 하나)
# 자리를 손으로 바꿀 때: 대상 자리를 띄워 헬스를 확인한 뒤
cp deploy/nginx/upstream-green.conf deploy/active/upstream.conf && docker compose exec -T nginx nginx -t && docker compose exec -T nginx nginx -s reload
docker compose stop <기준>-blue                                    # 남은 연결이 끝난 뒤(최대 3분)
```
- 이름 없이 `docker compose up -d` 를 해도 두 자리는 뜨지 않는다(자리에는 profiles 가 붙어 있다).
- 배포가 `cd_switch` + "되돌림 미확인"/"활성 파일 복원 실패"로 끝났으면 두 자리가 모두 떠 있을 수 있다. 위 명령으로 응답 자리를 확인하고 반대쪽을 내린다. 다음 배포도 실제 응답 자리를 기준으로 정리한다.
