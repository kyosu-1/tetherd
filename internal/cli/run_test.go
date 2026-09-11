package cli

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestParseRemoteCIDRs(t *testing.T) {
	got, err := ParseRemoteCIDRs([]string{"10.0.0.0/16", "169.254.170.0/24"})
	if err != nil || len(got) != 2 || got[1].String() != "169.254.170.0/24" {
		t.Fatalf("got %v, err %v", got, err)
	}
	if _, err := ParseRemoteCIDRs([]string{"10.0.0.0"}); err == nil {
		t.Fatal("bare address must fail")
	}
	if _, err := ParseRemoteCIDRs([]string{"fd00::/8"}); err == nil {
		t.Fatal("ipv6 must fail in v1")
	}
}

func TestRunCommandParsesFlagsAndCommand(t *testing.T) {
	var captured RunOptions
	runFn = func(opts RunOptions) (int, error) { captured = opts; return 0, nil }
	t.Cleanup(func() { runFn = defaultRun })

	root := NewRootCommand()
	root.SetArgs([]string{"run", "--transport", "direct", "--agent-addr", "127.0.0.1:9900",
		"--remote-cidr", "172.20.0.0/16", "--remote-cidr", "10.1.0.0/16", "--user", "shota",
		"--", "curl", "-s", "http://172.20.0.11:8081/"})
	var stderr bytes.Buffer
	root.SetErr(&stderr)
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if captured.AgentAddr != "127.0.0.1:9900" || len(captured.RemoteCIDRs) != 2 || captured.User != "shota" {
		t.Fatalf("opts = %+v", captured)
	}
	if len(captured.Command) != 3 || captured.Command[0] != "curl" {
		t.Fatalf("command = %v", captured.Command)
	}
}

func TestExitCodeMapping(t *testing.T) {
	t.Run("child exit code", func(t *testing.T) {
		runFn = func(opts RunOptions) (int, error) { return 7, nil }
		t.Cleanup(func() { runFn = defaultRun })

		root := NewRootCommand()
		root.SetArgs([]string{"run", "--transport", "direct", "--agent-addr", "x:1", "--", "true"})
		root.SetErr(&bytes.Buffer{})
		root.SetOut(&bytes.Buffer{})
		err := root.Execute()
		if err == nil {
			t.Fatal("expected an error")
		}
		if got := ExitCode(err); got != 7 {
			t.Fatalf("ExitCode = %d, want 7", got)
		}
		if !IsChildExit(err) {
			t.Fatal("IsChildExit = false, want true")
		}
	})

	t.Run("run error with explicit usage code", func(t *testing.T) {
		runFn = func(opts RunOptions) (int, error) { return 2, errors.New("bad flag") }
		t.Cleanup(func() { runFn = defaultRun })

		root := NewRootCommand()
		root.SetArgs([]string{"run", "--transport", "direct", "--agent-addr", "x:1", "--", "true"})
		root.SetErr(&bytes.Buffer{})
		root.SetOut(&bytes.Buffer{})
		err := root.Execute()
		if err == nil {
			t.Fatal("expected an error")
		}
		if got := ExitCode(err); got != 2 {
			t.Fatalf("ExitCode = %d, want 2", got)
		}
		if IsChildExit(err) {
			t.Fatal("IsChildExit = true, want false")
		}
		if !strings.Contains(err.Error(), "bad flag") {
			t.Fatalf("err = %v, want it to contain %q", err, "bad flag")
		}
	})

	t.Run("run error with code 1", func(t *testing.T) {
		runFn = func(opts RunOptions) (int, error) { return 1, errors.New("connect failed") }
		t.Cleanup(func() { runFn = defaultRun })

		root := NewRootCommand()
		root.SetArgs([]string{"run", "--transport", "direct", "--agent-addr", "x:1", "--", "true"})
		root.SetErr(&bytes.Buffer{})
		root.SetOut(&bytes.Buffer{})
		err := root.Execute()
		if err == nil {
			t.Fatal("expected an error")
		}
		if got := ExitCode(err); got != 1 {
			t.Fatalf("ExitCode = %d, want 1", got)
		}
		if IsChildExit(err) {
			t.Fatal("IsChildExit = true, want false")
		}
	})

	t.Run("cobra usage error", func(t *testing.T) {
		root := NewRootCommand()
		root.SetArgs([]string{"run", "--transport", "direct", "--agent-addr", "x:1"})
		root.SetErr(&bytes.Buffer{})
		root.SetOut(&bytes.Buffer{})
		err := root.Execute()
		if err == nil {
			t.Fatal("expected an error")
		}
		if got := ExitCode(err); got != 2 {
			t.Fatalf("ExitCode = %d, want 2", got)
		}
		if IsChildExit(err) {
			t.Fatal("IsChildExit = true, want false")
		}
	})
}

func TestRunRequiresCommand(t *testing.T) {
	root := NewRootCommand()
	root.SetArgs([]string{"run", "--transport", "direct", "--agent-addr", "x:1"})
	root.SetErr(&bytes.Buffer{})
	root.SetOut(&bytes.Buffer{})
	if err := root.Execute(); err == nil {
		t.Fatal("run without -- <command> must fail")
	}
}
