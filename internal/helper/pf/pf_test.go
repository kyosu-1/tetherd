package pf

import (
	"net/netip"
	"strings"
	"testing"
)

func TestRules(t *testing.T) {
	spec := Spec{
		RemoteCIDRs:  []netip.Prefix{netip.MustParsePrefix("10.0.0.0/16"), netip.MustParsePrefix("169.254.170.0/24")},
		RedirectPort: 15300,
	}
	got, err := Rules(spec)
	if err != nil {
		t.Fatal(err)
	}
	want := `table <tetherd_remote> { 10.0.0.0/16, 169.254.170.0/24 }
rdr pass on lo0 inet proto tcp from any to <tetherd_remote> -> 127.0.0.1 port 15300
pass out route-to lo0 inet proto tcp from any to <tetherd_remote> group tetherd keep state
`
	if got != want {
		t.Fatalf("rules =\n%s\nwant\n%s", got, want)
	}
}

func TestRulesRejectsEmptyAndIPv6(t *testing.T) {
	if _, err := Rules(Spec{RedirectPort: 1}); err == nil {
		t.Fatal("empty cidrs must fail")
	}
	if _, err := Rules(Spec{RemoteCIDRs: []netip.Prefix{netip.MustParsePrefix("fd00::/8")}, RedirectPort: 1}); err == nil {
		t.Fatal("ipv6 must fail in v1")
	}
}

type call struct {
	args  []string
	stdin string
}

func recorder(out string) (Runner, *[]call) {
	var calls []call
	return func(args []string, stdin string) (string, error) {
		calls = append(calls, call{args, stdin})
		return out, nil
	}, &calls
}

func TestEnableParsesToken(t *testing.T) {
	run, calls := recorder("No ALTQ support in kernel\nALTQ related functions disabled\npf enabled\nToken : 1234567890\n")
	tok, err := (Pfctl{Run: run}).Enable()
	if err != nil || tok != "1234567890" {
		t.Fatalf("tok = %q, err = %v", tok, err)
	}
	if got := (*calls)[0].args; strings.Join(got, " ") != "-E" {
		t.Fatalf("args = %v", got)
	}
}

func TestEnableWithoutToken(t *testing.T) {
	run, _ := recorder("pfctl: pf already enabled\n")
	if _, err := (Pfctl{Run: run}).Enable(); err == nil {
		t.Fatal("missing token must be an error")
	}
}

func TestLoadAndFlushAnchor(t *testing.T) {
	run, calls := recorder("")
	p := (Pfctl{Run: run})
	if err := p.LoadAnchor(Anchor, "pass all\n"); err != nil {
		t.Fatal(err)
	}
	if err := p.FlushAnchor(Anchor); err != nil {
		t.Fatal(err)
	}
	if err := p.Disable("42"); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"-a com.apple/900.tetherd -f -",
		"-a com.apple/900.tetherd -F rules",
		"-a com.apple/900.tetherd -F nat",
		"-a com.apple/900.tetherd -F Tables",
		"-X 42",
	}
	for i, c := range *calls {
		if strings.Join(c.args, " ") != want[i] {
			t.Fatalf("call %d = %v, want %s", i, c.args, want[i])
		}
	}
	if (*calls)[0].stdin != "pass all\n" {
		t.Fatalf("rules not passed on stdin: %q", (*calls)[0].stdin)
	}
}
