// ci-cd 서버 — MODE=master|agent 단일 바이너리 (.env로 역할 전환, 사용자 결정 2026-08-29)
package main

import (
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
	mux.HandleFunc("GET /health", func(writer http.ResponseWriter, request *http.Request) {
		shared.WriteOK(writer, map[string]string{"mode": mode})
	})

	var defaultPort string
	switch mode {
	case "master":
		master.New(logger).Register(mux)
		defaultPort = "15000"
	case "agent":
		agent.New(logger).Register(mux)
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
	server := &http.Server{                       // 자원 보호 타임아웃 (리뷰 F9)
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
