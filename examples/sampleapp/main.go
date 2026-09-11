// sampleapp is the verification target: it reports its hostname, TETHERD_ENV
// and the peer address so you can tell whether a request came through the
// agent (peer = agent IP) or directly. /db proves the task can reach RDS
// with the secret-injected password; /whoami proves the task role.
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// dsn builds a postgres URL from DB_* env vars; empty when DB_HOST is unset.
func dsn(getenv func(string) string) string {
	host := getenv("DB_HOST")
	if host == "" {
		return ""
	}
	port := getenv("DB_PORT")
	if port == "" {
		port = "5432"
	}
	u := url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(getenv("DB_USER"), getenv("DB_PASSWORD")),
		Host:     host + ":" + port,
		Path:     "/" + getenv("DB_NAME"),
		RawQuery: "sslmode=require",
	}
	return u.String()
}

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
		if err != nil || resp.StatusCode != http.StatusOK {
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
	mux.HandleFunc("/db", func(w http.ResponseWriter, r *http.Request) {
		d := dsn(os.Getenv)
		if d == "" {
			http.Error(w, "DB_HOST is not set", http.StatusServiceUnavailable)
			return
		}
		db, err := sql.Open("pgx", d)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer db.Close()
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		var now time.Time
		if err := db.QueryRowContext(ctx, "SELECT now()").Scan(&now); err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		fmt.Fprintf(w, "db now=%s host=%s\n", now.Format(time.RFC3339), os.Getenv("DB_HOST"))
	})
	mux.HandleFunc("/whoami", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		cfg, err := config.LoadDefaultConfig(ctx)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		out, err := sts.NewFromConfig(cfg).GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		fmt.Fprintf(w, "whoami %s\n", *out.Arn)
	})
	log.Printf("sampleapp listening on :%s", port)
	log.Fatal(http.ListenAndServe(":"+port, mux))
}
