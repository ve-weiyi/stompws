package web

import (
	"embed"
	"net/http"
)

//go:embed client.html
var clientHTML embed.FS

func HandleWebClient(w http.ResponseWriter, r *http.Request) {
	http.ServeFileFS(w, r, clientHTML, "client.html")
}
