package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ve-weiyi/stompws/logws"
	"github.com/ve-weiyi/stompws/server"
	"github.com/ve-weiyi/stompws/web"
)

// shutdownTimeout 是关停时等待连接与中继收尾的上限。
const shutdownTimeout = 10 * time.Second

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)

	// 变量名不能用 server —— 那会遮蔽包名。
	hub := server.NewStompHubServer(
		server.WithEventHooks(server.NewDefaultEventHook()),
		server.WithAuthenticator(server.NewNoAuthenticator()),
		server.WithLogger(logws.NewDefaultLogger()),
	)

	mux := http.NewServeMux()
	mux.HandleFunc("/admin-api/v1/websocket", hub.HandleWebSocket)
	// 只服务根路径："/" 当 catch-all 会把任意未匹配路径都返回这个 HTML 页面。
	mux.HandleFunc("/{$}", web.HandleWebClient)

	httpSrv := &http.Server{Addr: ":9091", Handler: mux}

	// 收到 SIGINT/SIGTERM 时优雅关停：先停 HTTP 接新连接，再让 hub 通知各连接收尾
	// （未确认的队列帧只有走到这一步才会被退回队首）。
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		<-ctx.Done()
		log.Println("shutting down ...")

		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := httpSrv.Shutdown(shutdownCtx); err != nil {
			log.Printf("http shutdown: %v", err)
		}
		if err := hub.Shutdown(shutdownTimeout); err != nil {
			log.Printf("stomp hub shutdown: %v", err)
		}
	}()

	fmt.Println("STOMP chat server starting on :9091")
	if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		// 启动失败不能以 0 退出：端口被占这类情况编排器必须能看见。
		fmt.Printf("server failed: %v\n", err)
		os.Exit(1)
	}
}
