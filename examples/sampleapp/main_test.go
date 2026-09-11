package main

import "testing"

func TestDSN(t *testing.T) {
	getenv := func(k string) string {
		return map[string]string{
			"DB_HOST": "db.example", "DB_USER": "tetherd", "DB_NAME": "app", "DB_PASSWORD": "p@ss word",
		}[k]
	}
	got := dsn(getenv)
	want := "postgres://tetherd:p%40ss%20word@db.example:5432/app?sslmode=require"
	if got != want {
		t.Fatalf("dsn = %q, want %q", got, want)
	}
	if dsn(func(string) string { return "" }) != "" {
		t.Fatal("dsn must be empty when DB_HOST is unset")
	}
}
