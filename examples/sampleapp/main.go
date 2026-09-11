// sampleapp is the verification target: it reports its hostname, TETHERD_ENV
// and the peer address so you can tell whether a request came through the
// agent (peer = agent IP) or directly.
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"
)

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8081"
	}
	check := flag.Bool("check", false, "check /healthz on 127.0.0.1:PORT and exit (no server started)")
	flag.Parse()
	if *check {
		c := http.Client{Timeout: 2 * time.Second}
		resp, err := c.Get("http://127.0.0.1:" + port + "/healthz")
		if err != nil {
			os.Exit(1)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			os.Exit(1)
		}
		os.Exit(0)
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
