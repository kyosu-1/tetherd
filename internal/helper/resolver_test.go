package helper

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolverSetAndClear(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "resolver")
	r := Resolver{Dir: dir}
	// A file that tetherd does not own must survive Clear.
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "corp.example"), []byte("nameserver 10.1.1.1\n"), 0o644)

	if err := r.Set([]string{"myapp.internal", "svc.local"}, 53530); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "myapp.internal"))
	if err != nil {
		t.Fatal(err)
	}
	want := "# managed by tetherd\nnameserver 127.0.0.1\nport 53530\n"
	if string(got) != want {
		t.Fatalf("file =\n%s\nwant\n%s", got, want)
	}
	if err := r.Clear(); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"myapp.internal", "svc.local"} {
		if _, err := os.Stat(filepath.Join(dir, d)); !os.IsNotExist(err) {
			t.Fatalf("%s should be removed", d)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "corp.example")); err != nil {
		t.Fatal("foreign resolver file must be kept")
	}
}

func TestResolverRejectsBadDomain(t *testing.T) {
	r := Resolver{Dir: t.TempDir()}
	for _, bad := range []string{"", "../etc", "a b", "UPPER.CASE", "-x.internal", "x/y"} {
		if err := r.Set([]string{bad}, 53530); err == nil {
			t.Fatalf("domain %q must be rejected", bad)
		}
	}
}
