// ci-cd 서버 — MODE=master|agent 단일 바이너리 (.env로 역할 전환, 사용자 결정 2026-08-29)
package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/junhyeong9812/study-note-deploy-system-ci-cd/internal/agent"
	"github.com/junhyeong9812/study-note-deploy-system-ci-cd/internal/master"
	"github.com/junhyeong9812/study-note-deploy-system-ci-cd/internal/shared"
)

func main() {
	mode := os.Getenv("MODE")
	logger := shared.NewLogger()
	mux := http.NewServeMux()

	var defaultPort string
	switch mode {
	case "master":
		deployMaster, err := master.New(logger)
		if err != nil {
			fmt.Fprintln(os.Stderr, err) // fail-closed: 이력을 남길 수 없으면 뜨지 않는다
			os.Exit(1)
		}
		deployMaster.Register(mux)
		mux.HandleFunc("GET /health", func(writer http.ResponseWriter, request *http.Request) {
			shared.WriteOK(writer, map[string]string{"mode": mode})
		})
		defaultPort = "15000"
	case "agent":
		deployAgent := agent.New(logger)
		deployAgent.Register(mux)
		// 지난 실행이나 서버 재부팅이 두 벌을 남겼으면 실제 활성 자리의 반대쪽을 정리한다(앱이 뜰 때까지 재시도)
		go deployAgent.ReconcileSlotsUntil(0, 30*time.Second) // 실제 자리를 확인할 때까지 — 포기하면 두 벌이 남는다
		// 자원 지표 — 30초마다 Redis 스트림으로(LOG_REDIS_URL 이 비면 시작하지 않는다). 배포 잠금과 무관하게 따로 돈다
		deployAgent.StartResourceMetrics(context.Background())
		// 통신 수치 — 30초마다 nginx 컨테이너의 접속 기록을 Redis 스트림 traffic 으로(설정이 없거나 틀리면 시작하지 않고 헬스가 까닭을 낸다)
		traffic := deployAgent.StartTraffic(context.Background())
		mux.HandleFunc("GET /health", agent.AgentHealthHandler(mode, traffic))
		defaultPort = "15001"
	default:
		fmt.Fprintln(os.Stderr, "MODE must be master or agent") // fail-closed: 모드 없이 뜨지 않는다
		os.Exit(1)
	}
	port := os.Getenv("PORT")
	if port == "" {
		port = defaultPort
	}
	logger.Log("boot", "listening :"+port+" mode="+mode, "info")
	server := &http.Server{ // 자원 보호 타임아웃 (리뷰 F9)
		Addr:              ":" + port,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    8 << 10,
	}
	if err := server.ListenAndServe(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
