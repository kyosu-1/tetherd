// sampleapp is the verification target: it reports its hostname, TETHERD_ENV
// and the peer address so you can tell whether a request came through the
// agent (peer = agent IP) or directly.
package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
)

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8081"
	}
	host, _ := os.Hostname()
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { fmt.Fprintln(w, "ok") })
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "sampleapp on %s env=%s from %s\n", host, os.Getenv("TETHERD_ENV"), r.RemoteAddr)
	})
	log.Printf("sampleapp listening on :%s", port)
	log.Fatal(http.ListenAndServe(":"+port, mux))
}
