package awsid

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

func TestFetchContainerCredentialsThroughDialer(t *testing.T) {
	var gotPath, gotHost string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotHost = r.URL.Path, r.Host
		w.Write([]byte(`{"AccessKeyId":"AKIA","SecretAccessKey":"sk","Token":"tok","Expiration":"2030-01-01T00:00:00Z","RoleArn":"arn:aws:iam::1:role/x"}`))
	}))
	defer srv.Close()
	var dialedAddr string
	dial := func(ctx context.Context, addr string) (net.Conn, error) {
		dialedAddr = addr
		return net.Dial("tcp", srv.Listener.Addr().String()) // pretend this is the agent dialing 169.254.170.2
	}
	creds, err := FetchContainerCredentials(context.Background(), dial, "/v2/credentials/abc")
	if err != nil {
		t.Fatal(err)
	}
	if creds.AccessKeyID != "AKIA" || creds.SecretAccessKey != "sk" || creds.SessionToken != "tok" || !creds.CanExpire {
		t.Fatalf("creds = %+v", creds)
	}
	if dialedAddr != "169.254.170.2:80" || gotPath != "/v2/credentials/abc" || gotHost != "169.254.170.2" {
		t.Fatalf("dialed %q path %q host %q", dialedAddr, gotPath, gotHost)
	}
}

func TestCallerIdentity(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			http.Error(w, "unsigned", 403)
			return
		}
		w.Header().Set("Content-Type", "text/xml")
		w.Write([]byte(`<GetCallerIdentityResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/"><GetCallerIdentityResult><Arn>arn:aws:sts::1:assumed-role/task/abc</Arn><UserId>u</UserId><Account>1</Account></GetCallerIdentityResult></GetCallerIdentityResponse>`))
	}))
	defer srv.Close()
	arn, err := CallerIdentity(context.Background(), aws.Credentials{AccessKeyID: "AKIA", SecretAccessKey: "sk", SessionToken: "tok"}, "ap-northeast-1",
		func(o *sts.Options) { o.BaseEndpoint = aws.String(srv.URL) })
	if err != nil {
		t.Fatal(err)
	}
	if arn != "arn:aws:sts::1:assumed-role/task/abc" {
		t.Fatalf("arn = %q", arn)
	}
}
