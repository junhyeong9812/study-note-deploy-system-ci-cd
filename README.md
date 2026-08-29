# study-note-deploy-system-ci-cd

Go 배포 서버 — **단일 바이너리, `.env`의 MODE로 master/agent 전환**.

```
리포 Actions(main push — 빌드 없음, 호출만) ──▶ www/api/deploy (front 중계, 비밀 패스스루)
                                     ▼
                          master (.9:15000) — HMAC·서비스 allowlist·라우팅·이력(/status)
                                     ▼ HTTP(LAN)
                          agent (각 호스트 :15001, docker.sock 컨테이너 — sudo 불요)
                                     ▼ 고정 디렉토리 allowlist에서만
                          git fetch + reset --hard <commit_sha>
                          docker compose up -d --build <서비스>     ← 호스트 재빌드 (전달 채널 = git 하나)
```

- 규약: 봉투(success/error) · 로그 `requestId:server-name:message`(stdout+Redis XADD 실패무해)
- 가드: fail-closed 시크릿 · 서비스/디렉토리 고정 열거 · 커밋 sha 정규식(hex만 — 주입·임의 ref 차단) · 에이전트 single-flight
- 구동: `.env` 작성 후 `docker compose up -d --build` · 테스트: `go test ./...`
