package ecs

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsecs "github.com/aws/aws-sdk-go-v2/service/ecs"
	"github.com/aws/aws-sdk-go-v2/service/ecs/types"
	awselb "github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2"
	elbtypes "github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2/types"

	"github.com/kyosu-1/tetherd/internal/doctor"
)

type fakeServices struct {
	in       *awsecs.DescribeServicesInput
	out      []types.Service
	failures []types.Failure
	err      error
}

func (f *fakeServices) DescribeServices(_ context.Context, in *awsecs.DescribeServicesInput, _ ...func(*awsecs.Options)) (*awsecs.DescribeServicesOutput, error) {
	f.in = in
	if f.err != nil {
		return nil, f.err
	}
	return &awsecs.DescribeServicesOutput{Services: f.out, Failures: f.failures}, nil
}

type fakeTargetGroups struct {
	in  *awselb.DescribeTargetGroupsInput
	out []elbtypes.TargetGroup
	err error
}

func (f *fakeTargetGroups) DescribeTargetGroups(_ context.Context, in *awselb.DescribeTargetGroupsInput, _ ...func(*awselb.Options)) (*awselb.DescribeTargetGroupsOutput, error) {
	f.in = in
	if f.err != nil {
		return nil, f.err
	}
	return &awselb.DescribeTargetGroupsOutput{TargetGroups: f.out}, nil
}

// oneTargetGroup is the dev environment's shape: one ALB target group, the
// agent container registered in it (deploy/dev-env/ecs.tf).
func oneTargetGroup(container string) []types.Service {
	return []types.Service{{
		ServiceName: aws.String("api"),
		LoadBalancers: []types.LoadBalancer{{
			TargetGroupArn: aws.String("arn:aws:elasticloadbalancing:ap-northeast-1:1:targetgroup/tethrd-dev/abc"),
			ContainerName:  aws.String(container),
			ContainerPort:  aws.Int32(8080),
		}},
	}}
}

func http1Group(port int32) []elbtypes.TargetGroup {
	return []elbtypes.TargetGroup{{
		TargetGroupName: aws.String("tethrd-dev"),
		Protocol:        elbtypes.ProtocolEnumHttp,
		ProtocolVersion: aws.String("HTTP1"),
		Port:            aws.Int32(port),
	}}
}

func agentDef(env map[string]string) *types.TaskDefinition {
	c := types.ContainerDefinition{Name: aws.String(DefaultAgentContainer)}
	for k, v := range env {
		c.Environment = append(c.Environment, types.KeyValuePair{Name: aws.String(k), Value: aws.String(v)})
	}
	return &types.TaskDefinition{
		PidMode:              types.PidModeTask,
		ContainerDefinitions: []types.ContainerDefinition{{Name: aws.String("app")}, c},
	}
}

func target() Target { return Target{Cluster: "tetherd-dev", Service: "api"} }

// TestTargetGroupReadsTheThreeFactsTheRowJudges: the protocol version comes
// from ELB and nowhere else, the port with it, and TETHERD_PROXY out of the
// agent container's environment in the task definition. Each read is asked
// for by the previous one's answer, so the inputs are pinned too - an ARN
// this function invented instead of using the service's would report a
// verdict about someone else's target group.
func TestTargetGroupReadsTheThreeFactsTheRowJudges(t *testing.T) {
	svc := &fakeServices{out: oneTargetGroup(DefaultAgentContainer)}
	elb := &fakeTargetGroups{out: http1Group(9090)}
	td := &fakeTaskDef{out: agentDef(map[string]string{"TETHERD_ENV": "dev", "TETHERD_PROXY": "0.0.0.0:9090"})}

	got, err := TargetGroup(context.Background(), svc, elb, td, target(), "arn:def:7")
	if err != nil {
		t.Fatal(err)
	}
	want := doctor.TargetGroup{
		Name: "tethrd-dev", Protocol: "HTTP", ProtocolVersion: "HTTP1",
		Port: 9090, AgentProxy: "0.0.0.0:9090",
	}
	if got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
	if aws.ToString(svc.in.Cluster) != "tetherd-dev" || len(svc.in.Services) != 1 || svc.in.Services[0] != "api" {
		t.Errorf("DescribeServices asked about %+v", svc.in)
	}
	if len(elb.in.TargetGroupArns) != 1 || !strings.HasSuffix(elb.in.TargetGroupArns[0], "targetgroup/tethrd-dev/abc") {
		t.Errorf("DescribeTargetGroups asked about %v, not the ARN the service gave", elb.in.TargetGroupArns)
	}
	if td.in != "arn:def:7" {
		t.Errorf("the task definition read asked for %q", td.in)
	}
}

// An agent container that sets no TETHERD_PROXY is the ordinary case, and it
// must read as "unset" rather than as an error: the judgement applies the
// agent's own default to it.
func TestTargetGroupReportsAnUnsetProxyAsUnset(t *testing.T) {
	got, err := TargetGroup(context.Background(),
		&fakeServices{out: oneTargetGroup(DefaultAgentContainer)},
		&fakeTargetGroups{out: http1Group(8080)},
		&fakeTaskDef{out: agentDef(map[string]string{"TETHERD_ENV": "dev"})},
		target(), "arn:def")
	if err != nil {
		t.Fatal(err)
	}
	if got.AgentProxy != "" {
		t.Errorf("AgentProxy = %q, want empty for a container that sets none", got.AgentProxy)
	}
}

// Every way the read can fail is one error for the row to report as not
// checked. None of them may come back as a zero-value doctor.TargetGroup
// with a nil error, because that reads as "HTTP1 was not reported" and lands
// on a different arm of the judgement.
func TestTargetGroupRefusesRatherThanGuess(t *testing.T) {
	agent := DefaultAgentContainer
	twoGroups := []types.Service{{
		ServiceName: aws.String("api"),
		LoadBalancers: []types.LoadBalancer{
			{TargetGroupArn: aws.String("arn:tg/a"), ContainerName: aws.String("app")},
			{TargetGroupArn: aws.String("arn:tg/b"), ContainerName: aws.String("sidecar")},
		},
	}}
	cases := []struct {
		name string
		svc  *fakeServices
		elb  *fakeTargetGroups
		td   *fakeTaskDef
		want string
	}{
		{
			"no permission on DescribeTargetGroups",
			&fakeServices{out: oneTargetGroup(agent)},
			&fakeTargetGroups{err: errors.New("AccessDenied")},
			&fakeTaskDef{out: agentDef(nil)},
			"AccessDenied",
		},
		{
			"no permission on DescribeServices",
			&fakeServices{err: errors.New("AccessDenied")},
			&fakeTargetGroups{out: http1Group(8080)},
			&fakeTaskDef{out: agentDef(nil)},
			"AccessDenied",
		},
		{
			"no such service",
			&fakeServices{failures: []types.Failure{{Reason: aws.String("MISSING")}}},
			&fakeTargetGroups{out: http1Group(8080)},
			&fakeTaskDef{out: agentDef(nil)},
			"MISSING",
		},
		{
			// A service with no load balancer is an ordinary setup for
			// somebody who runs with --no-incoming, so it is a refusal to
			// judge and not an invented HTTP1.
			"the service registers no target group",
			&fakeServices{out: []types.Service{{ServiceName: aws.String("api")}}},
			&fakeTargetGroups{out: http1Group(8080)},
			&fakeTaskDef{out: agentDef(nil)},
			"registers no target group",
		},
		{
			"several target groups and none for the agent container",
			&fakeServices{out: twoGroups},
			&fakeTargetGroups{out: http1Group(8080)},
			&fakeTaskDef{out: agentDef(nil)},
			"ambiguous",
		},
		{
			"the target group is gone",
			&fakeServices{out: oneTargetGroup(agent)},
			&fakeTargetGroups{},
			&fakeTaskDef{out: agentDef(nil)},
			"no target group",
		},
		{
			"the task definition cannot be read",
			&fakeServices{out: oneTargetGroup(agent)},
			&fakeTargetGroups{out: http1Group(8080)},
			&fakeTaskDef{err: errors.New("AccessDeniedException")},
			"AccessDenied",
		},
		{
			"the task definition has no agent container",
			&fakeServices{out: oneTargetGroup(agent)},
			&fakeTargetGroups{out: http1Group(8080)},
			&fakeTaskDef{out: &types.TaskDefinition{ContainerDefinitions: []types.ContainerDefinition{{Name: aws.String("app")}}}},
			"no container named",
		},
	}
	for _, c := range cases {
		got, err := TargetGroup(context.Background(), c.svc, c.elb, c.td, target(), "arn:def")
		if err == nil {
			t.Errorf("%s: no error, and the facts came back as %+v", c.name, got)
			continue
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: error %q does not mention %q", c.name, err, c.want)
		}
		if got != (doctor.TargetGroup{}) {
			t.Errorf("%s: a refusal must carry no facts: %+v", c.name, got)
		}
	}
}

// A single-target-group service is taken whatever container it names: that
// is the deployment deploy/dev-env/alb.tf describes, and the container's
// name is not this row's business. Two or more without the agent's own is
// the ambiguous case above.
func TestTargetGroupTakesTheOnlyGroupWhateverContainerItNames(t *testing.T) {
	got, err := TargetGroup(context.Background(),
		&fakeServices{out: oneTargetGroup("something-else")},
		&fakeTargetGroups{out: http1Group(8080)},
		&fakeTaskDef{out: agentDef(nil)},
		target(), "arn:def")
	if err != nil {
		t.Fatal(err)
	}
	if got.ProtocolVersion != "HTTP1" {
		t.Errorf("got %+v", got)
	}
}

// The agent container's own registration wins when a service has several, so
// the row judges the group that fronts the agent rather than whichever one
// ECS listed first.
func TestTargetGroupPrefersTheAgentContainersRegistration(t *testing.T) {
	svc := &fakeServices{out: []types.Service{{
		ServiceName: aws.String("api"),
		LoadBalancers: []types.LoadBalancer{
			{TargetGroupArn: aws.String("arn:tg/app"), ContainerName: aws.String("app")},
			{TargetGroupArn: aws.String("arn:tg/agent"), ContainerName: aws.String(DefaultAgentContainer)},
		},
	}}}
	elb := &fakeTargetGroups{out: http1Group(8080)}
	if _, err := TargetGroup(context.Background(), svc, elb, &fakeTaskDef{out: agentDef(nil)}, target(), "arn:def"); err != nil {
		t.Fatal(err)
	}
	if len(elb.in.TargetGroupArns) != 1 || elb.in.TargetGroupArns[0] != "arn:tg/agent" {
		t.Errorf("asked about %v, want the agent container's target group", elb.in.TargetGroupArns)
	}
}

// A protocol version ELB does not report - a TCP target group - comes
// through as the empty string rather than as HTTP1. Substituting a default
// here would turn "nothing was reported" into a green row.
func TestTargetGroupCarriesAnUnreportedProtocolVersionThrough(t *testing.T) {
	got, err := TargetGroup(context.Background(),
		&fakeServices{out: oneTargetGroup(DefaultAgentContainer)},
		&fakeTargetGroups{out: []elbtypes.TargetGroup{{
			TargetGroupName: aws.String("tethrd-dev"),
			Protocol:        elbtypes.ProtocolEnumTcp,
			Port:            aws.Int32(8080),
		}}},
		&fakeTaskDef{out: agentDef(nil)},
		target(), "arn:def")
	if err != nil {
		t.Fatal(err)
	}
	if got.ProtocolVersion != "" || got.Protocol != "TCP" {
		t.Errorf("got %+v, want a TCP group with no protocol version", got)
	}
}

// TETHERD_PROXY in a place this call cannot read is refused, not reported as
// unset: an env-file lives in S3 and a secret in Secrets Manager, so
// answering "unset" would claim the agent's default applies to a deployment
// that may have moved the port.
func TestAgentProxyRefusesWhatItCannotRead(t *testing.T) {
	fromSecret := &types.TaskDefinition{ContainerDefinitions: []types.ContainerDefinition{{
		Name:    aws.String(DefaultAgentContainer),
		Secrets: []types.Secret{{Name: aws.String("TETHERD_PROXY")}},
	}}}
	if _, err := AgentProxy(context.Background(), &fakeTaskDef{out: fromSecret}, "arn", ""); err == nil {
		t.Error("a TETHERD_PROXY sourced from a secret must be refused, not read as unset")
	}
	fromFile := &types.TaskDefinition{ContainerDefinitions: []types.ContainerDefinition{{
		Name:             aws.String(DefaultAgentContainer),
		EnvironmentFiles: []types.EnvironmentFile{{Value: aws.String("arn:aws:s3:::bucket/env")}},
	}}}
	if _, err := AgentProxy(context.Background(), &fakeTaskDef{out: fromFile}, "arn", ""); err == nil {
		t.Error("an environmentFiles container must be refused, not read as unset")
	}
	// An env-file container that also sets TETHERD_PROXY literally is read
	// from the literal, on ECS's documented precedence (the container
	// definition's environment overrides an environmentFiles entry). That is
	// read from the API reference, not measured here.
	both := &types.TaskDefinition{ContainerDefinitions: []types.ContainerDefinition{{
		Name:             aws.String(DefaultAgentContainer),
		Environment:      []types.KeyValuePair{{Name: aws.String("TETHERD_PROXY"), Value: aws.String("0.0.0.0:9090")}},
		EnvironmentFiles: []types.EnvironmentFile{{Value: aws.String("arn:aws:s3:::bucket/env")}},
	}}}
	got, err := AgentProxy(context.Background(), &fakeTaskDef{out: both}, "arn", "")
	if err != nil || got != "0.0.0.0:9090" {
		t.Errorf("got %q, %v", got, err)
	}
}

// The sidecar's name is one constant. A second literal would drift after a
// rename and leave discovery and this read looking at different containers.
func TestAgentProxyDefaultsToTheDiscoveryAgentContainer(t *testing.T) {
	td := &fakeTaskDef{out: agentDef(map[string]string{"TETHERD_PROXY": "0.0.0.0:9090"})}
	got, err := AgentProxy(context.Background(), td, "arn", "")
	if err != nil {
		t.Fatal(err)
	}
	if got != "0.0.0.0:9090" {
		t.Errorf("got %q; the default container name must be %q", got, DefaultAgentContainer)
	}
	// And a Target that names its own container is honoured.
	named := &types.TaskDefinition{ContainerDefinitions: []types.ContainerDefinition{
		{Name: aws.String(DefaultAgentContainer), Environment: []types.KeyValuePair{{Name: aws.String("TETHERD_PROXY"), Value: aws.String("0.0.0.0:1")}}},
		{Name: aws.String("tunnel"), Environment: []types.KeyValuePair{{Name: aws.String("TETHERD_PROXY"), Value: aws.String("0.0.0.0:2")}}},
	}}
	if got, err := AgentProxy(context.Background(), &fakeTaskDef{out: named}, "arn", "tunnel"); err != nil || got != "0.0.0.0:2" {
		t.Errorf("got %q, %v", got, err)
	}
}
