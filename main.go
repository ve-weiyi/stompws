package main

import (
	"fmt"
	"log"
	"net/http"

	"github.com/ve-weiyi/stompws/logws"
	"github.com/ve-weiyi/stompws/server/client"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)
	server := client.NewStompHubServer(
		client.WithEventHooks(client.NewDefaultEventHook()),
		client.WithAuthenticator(client.NewNoAuthenticator()),
		client.WithLogger(logws.NewDefaultLogger()),
	)

	http.HandleFunc("/admin-api/v1/websocket", server.HandleWebSocket)
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, "web/client.html")
	})

	fmt.Println("STOMP chat server starting on :9091")
	if err := http.ListenAndServe(":9091", nil); err != nil {
		fmt.Printf("server failed: %v\n", err)
	}
}
