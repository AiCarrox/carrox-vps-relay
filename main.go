// vps-relay entry point.
package main

import (
	"log"
	"net/http"
)

const (
	version    = "M13"
	listenAddr = "127.0.0.1:8787"
)

func main() {
	loadKeyTable()
	mux := http.NewServeMux()
	registerAPIRoutes(mux)
	registerMCPRoutes(mux)
	registerUIRoutes(mux)
	go autoRenewLoop()
	// Upgrade path: a panel left on HTTP whose admin already has a CF token.
	go ensurePanelHTTPS()
	log.Printf("vps-relay listening on %s (%s)", listenAddr, version)
	log.Fatal(http.ListenAndServe(listenAddr, mux))
}
