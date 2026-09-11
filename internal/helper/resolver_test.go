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

func TestResolverRefusesForeignFile(t *testing.T) {
	dir := t.TempDir()
	r := Resolver{Dir: dir}
	foreign := "nameserver 10.1.1.1\n"
	if err := os.WriteFile(filepath.Join(dir, "corp.example"), []byte(foreign), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := r.Set([]string{"corp.example"}, 53530); err == nil {
		t.Fatal("Set must refuse to overwrite a file it does not manage")
	}
	got, err := os.ReadFile(filepath.Join(dir, "corp.example"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != foreign {
		t.Fatalf("foreign file must be left untouched: got %q, want %q", got, foreign)
	}

	// A managed file, by contrast, is overwritten on a later Set for the
	// same domain with a different port.
	if err := r.Set([]string{"myapp.internal"}, 53530); err != nil {
		t.Fatal(err)
	}
	if err := r.Set([]string{"myapp.internal"}, 53531); err != nil {
		t.Fatalf("Set must overwrite its own managed file: %v", err)
	}
	got, err = os.ReadFile(filepath.Join(dir, "myapp.internal"))
	if err != nil {
		t.Fatal(err)
	}
	want := "# managed by tetherd\nnameserver 127.0.0.1\nport 53531\n"
	if string(got) != want {
		t.Fatalf("managed file =\n%s\nwant\n%s", got, want)
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
