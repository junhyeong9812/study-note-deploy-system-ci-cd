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
| `GET /deploys/git/commits?service=&from=&to=` | 그 서비스의 agent 저장소에서 읽은 커밋 목록(from 제외 · to 포함) — 아래 "git 조회" |
| `GET /deploys/git/commit?service=&sha=` | 커밋 하나와 파일별 diff(첫째 부모와의 차이) — 아래 "git 조회" |

## git 조회 — 읽기만 (배포 상세의 커밋·diff)

master 에는 저장소가 없다. master 가 입력을 검사한 뒤 그 서비스의 agent 를 불러(`GET`, 같은 비밀값, 기한 4초, 응답 4MB까지) 상태·봉투를 그대로 돌려준다.

| agent API | 설명 |
|---|---|
| `GET /agent/git/commits?service=&from=&to=` | `{commits:[{sha, author_name, authored_at, subject, body, body_truncated}], truncated, range_complete, relation}` — `relation` 이 `ancestor` 면 from..to(to 쪽이 먼저, `--topo-order`), `not_ancestor`(되돌린 배포·갈라진 이력 — to 의 이력이 온전하거나 to 가 from 의 조상)면 빈 목록, `unknown`(얕은 이력의 경계에서 끊겨 이 서버의 git 으로는 알 수 없음)이면 빈 목록 + `range_complete:false`. 그 밖의 `range_complete:false` 는 목록에 얕은 이력의 경계 커밋이 들어 있다는 뜻(그 너머는 로컬에 없다) |
| `GET /agent/git/commit?service=&sha=` | 커밋 요약 + `files:[{path, previous_path, status, additions, deletions, binary, patch, patch_truncated}]`, `files_truncated` — 병합 커밋은 첫째 부모와의 차이, 바이너리는 `patch:null` |

- **읽기만**: `rev-parse` · `merge-base` · `rev-list` · `log` · `show` 만 쓴다. `fetch` · `reset` · `checkout` · `gc` 를 부르지 않고 네트워크에 나가지 않는다. 배포 잠금을 잡지 않아 배포 중에도 답한다(대신 CPU·디스크는 나눠 쓴다).
- **입력**: `service` 는 허용 목록(`DEPLOY_DIR_<SVC>` 가 있는 서비스)만, SHA 는 소문자 16진수 7~64자만, 칸은 하나씩. SHA 는 `rev-parse --verify` 로 커밋인지 확인해 전체 SHA 로 확정한 값만 쓰고, git 에는 인자 배열로 `--end-of-options` 뒤에 넘긴다.
- **실행 환경**: 부모의 `GIT_*` 를 물려주지 않고 `LC_ALL=C` · `GIT_OPTIONAL_LOCKS=0` · `GIT_NO_LAZY_FETCH=1` · `GIT_NO_REPLACE_OBJECTS=1` · `GIT_TERMINAL_PROMPT=0` · `--no-pager` · `--no-ext-diff --no-textconv` · `-c core.quotePath=true` · `--src-prefix=a/ --dst-prefix=b/` 로 돈다. 판정은 종료 코드로만 한다.
- **상한**(넘으면 자르고 알린다 — 조용히 자르지 않는다): 커밋 100개(`truncated`) · 파일 100개(`files_truncated`) · 파일 하나의 patch 500줄과 한 줄 2000바이트(`patch_truncated`) · 제목 500바이트 · 본문 8KB(`body_truncated`) · git 명령 하나의 출력 2MB(온전히 읽은 커밋·파일까지만) · 응답 JSON 2MB(커밋 목록은 오래된 커밋부터 빼고 `truncated`, 상세는 뒤 파일부터 patch 를 비우고 그래도 넘으면 뒤 파일부터 빼고 `files_truncated`) · 조회 하나 3초 · 동시 조회 2개(넘으면 기다리지 않고 429).
- **파일 목록과 patch 는 따로**: 파일 목록(경로·상태·줄 수)은 `--raw -z` · `--numstat -z` 에서 읽는다. 목록을 온전히 읽었으면 patch 출력이 상한에 걸려도 목록은 싣고, patch 를 온전히 읽지 못했거나 patch 조각과 파일의 짝(머리줄)을 확정하지 못한 파일만 `patch:""` + `patch_truncated:true`. `files_truncated` 는 목록 자체가 잘렸을 때(100개 초과 · 목록 출력 상한 · 응답 크기)만. 형 바뀜(파일↔심볼릭 링크)은 `modified` 한 파일로, patch 는 두 조각을 이어 붙인다.
- 작성자 이메일은 내지 않는다. 오류에 git 의 오류 글자·디렉터리 경로를 넣지 않는다.

| 상황 | agent | master |
|---|---|---|
| 비밀값 틀림 | 401 `unauthorized` | 401 (자기 검사) |
| master 와 agent 의 비밀값이 다름(설정 실수) | 401 `unauthorized` | 502 `agent_unauthorized` (그대로 전달하지 않는 유일한 예외) |
| 입력 틀림 · 허용 목록 밖 | 422 `invalid_request` | 422 (자기 검사 — agent 를 부르지 않는다. 그 서비스의 `AGENT_URL_` 이 없어도 422) |
| SHA 가 로컬에 없음(얕은 이력 밖 포함) · 커밋 아님 · 약식 SHA 가 모호 | 404 `commit_not_found` | 그대로 |
| 두 커밋의 관계를 얕은 이력으로 알 수 없음 | 200 `relation:"unknown"` · 빈 목록 | 그대로 |
| 얕은 이력의 경계 커밋의 diff(부모가 로컬에 없다) | 404 `parent_unavailable` | 그대로 |
| 동시 조회 초과 | 429 `too_many_requests` | 그대로 |
| git 기한 초과 · git 실패 | 504 `git_timeout` · 500 `git_failed` | 그대로 |
| agent 에 닿지 못함 · 4초 안에 답 없음 · 봉투가 아님(4MB 초과 포함) | — | 502 `agent_unreachable` · 504 `agent_timeout` · 502 `unexpected_response` |

## 대상 repo가 지켜야 할 것

- Dockerfile 에 `test` 단계 — agent 는 `docker build --target test <경로>` 만 실행한다. 무엇을 돌릴지는 repo 가 정한다.
- compose 대상 서비스에 `image: <이름>:${IMAGE_TAG:-local}` — CI 가 sha 태그로 빌드하고 CD 가 그 태그로 교체한다.
- 되돌리기: 이전 커밋으로 접수를 다시 보낸다. 서비스별 최근 5개 sha 이미지를 남긴다(이미지 이름은 실행 중인 컨테이너에서 읽는다).
- 서버에서 손으로 올릴 때: `IMAGE_TAG=<운영 중인 sha> docker compose up -d --no-build --no-deps <대상>`. `IMAGE_TAG` 없이 `up` 하면 `local` 태그로 그 자리에서 빌드해 컨테이너를 다시 만든다.

- 규약: 봉투(success/error) · 로그 `requestId:server-name:message`(stdout+Redis XADD 실패무해)
- 가드: fail-closed 시크릿(상수시간) · 서비스/디렉토리 고정 열거 · 커밋 sha hex 검증(**master가 접수 전에**) · master 서비스별 + agent single-flight · CI 실패 시 CD 미호출 · 배포 후 헬스 확인(DEPLOY_HEALTH_*) · 이력을 기록하지 못하면 접수 거절 · http 타임아웃
- 수용 리스크(명문화): git-pull 방식은 저장소 커밋이 compose를 바꿀 수 있다 — main의 PR 게이트·자기 소유 repo가 신뢰 경계. HMAC+타임스탬프(재전송 방지)는 후속 이슈
- 구동: `.env` 작성 후 `docker compose up -d --build` · 테스트: `go test ./...` 또는 `docker build --target test .`(git 조회 테스트는 실제 git 이 필요하다)


## 알림 서버 연동

`ALERT_EVENTS_URL`·`ALERT_SECRET` 이 설정돼 있으면 사람이 알아야 할 일을 알림 서버에 보낸다. 보내는 일은 배포와 분리돼 있어 실패해도 배포 결과를 바꾸지 않는다.

- master: 배포가 `failed` 로 끝났을 때(서비스·단계·출력 끝부분)
- agent: 두 자리가 남았을 수 있을 때 — 자리 정지 보류, 되돌림 미확인, 활성 파일 복원 실패

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
- 정리 알림(`DEPLOY_DRAIN_*`)을 받은 앱은 새 일을 받지 않는다(llm 래퍼는 질문을 돌려보낸다). 그 자리를 손으로 다시 활성 자리로 돌릴 때는 컨테이너를 재기동한다: `docker compose restart <기준>-<자리>`.
- 구 자리는 전환 뒤 reload 가 자리 잡기를 2초 기다린 다음, 남은 연결이 끝나면(최대 3분) 내려간다. 로그에 `자리 정지 보류` 가 남았으면 두 자리가 떠 있는 것이다 — 아래처럼 응답 자리를 확인하고 반대쪽을 내린다.
- 배포가 `cd_switch` + "되돌림 미확인"/"활성 파일 복원 실패"로 끝났으면 두 자리가 모두 떠 있을 수 있다. 위 명령으로 응답 자리를 확인하고 반대쪽을 내린다. 다음 배포도 실제 응답 자리를 기준으로 정리한다.
