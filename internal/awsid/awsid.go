// Package awsid proves the task role reaches the laptop: it fetches the
// container credentials from 169.254.170.2 through the agent (the same
// path the child's AWS SDK takes) and calls sts:GetCallerIdentity with
// them (spec §6.3 step 7).
package awsid

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

// CredentialsHost is the ECS task-role credential endpoint.
const CredentialsHost = "169.254.170.2"

type containerCreds struct {
	AccessKeyID     string    `json:"AccessKeyId"`
	SecretAccessKey string    `json:"SecretAccessKey"`
	Token           string    `json:"Token"`
	Expiration      time.Time `json:"Expiration"`
}

// FetchContainerCredentials GETs http://169.254.170.2<relativeURI> using
// dial for the TCP connection (session.Client.DialTCP in production).
func FetchContainerCredentials(ctx context.Context, dial func(ctx context.Context, addr string) (net.Conn, error), relativeURI string) (aws.Credentials, error) {
	client := &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) { return dial(ctx, addr) },
		},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+CredentialsHost+relativeURI, nil)
	if err != nil {
		return aws.Credentials{}, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return aws.Credentials{}, fmt.Errorf("fetch task credentials via agent: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return aws.Credentials{}, fmt.Errorf("task credentials endpoint: HTTP %d", resp.StatusCode)
	}
	var c containerCreds
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&c); err != nil {
		return aws.Credentials{}, fmt.Errorf("task credentials: %w", err)
	}
	return aws.Credentials{
		AccessKeyID: c.AccessKeyID, SecretAccessKey: c.SecretAccessKey, SessionToken: c.Token,
		CanExpire: true, Expires: c.Expiration, Source: "tetherd(169.254.170.2)",
	}, nil
}

// CallerIdentity returns the ARN sts sees for creds.
func CallerIdentity(ctx context.Context, creds aws.Credentials, region string, optFns ...func(*sts.Options)) (string, error) {
	cfg := aws.Config{
		Region:      region,
		Credentials: credentials.NewStaticCredentialsProvider(creds.AccessKeyID, creds.SecretAccessKey, creds.SessionToken),
	}
	out, err := sts.NewFromConfig(cfg, optFns...).GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return "", fmt.Errorf("sts:GetCallerIdentity with task credentials: %w", err)
	}
	return aws.ToString(out.Arn), nil
}
