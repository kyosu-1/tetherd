package ecs

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsecs "github.com/aws/aws-sdk-go-v2/service/ecs"
	"github.com/aws/aws-sdk-go-v2/service/ecs/types"
	awselb "github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2"

	"github.com/kyosu-1/tetherd/internal/doctor"
)

// proxyEnvVar is the agent's own name for the address it serves the ALB on.
// internal/agent reads the identical string out of the process environment
// (ConfigFromEnv); this package reads it out of the task definition, which
// is the only place the CLI can see it - the control stream carries no such
// field, and the port the CLI connects to is the control port, not this one.
const proxyEnvVar = "TETHERD_PROXY"

// ServiceAPI is the slice of the ECS API that reads a service's load
// balancer configuration - which target group its tasks are registered in.
type ServiceAPI interface {
	DescribeServices(ctx context.Context, in *awsecs.DescribeServicesInput, opts ...func(*awsecs.Options)) (*awsecs.DescribeServicesOutput, error)
}

// TargetGroupAPI is the slice of the Elastic Load Balancing v2 API that
// reads a target group's own configuration. It is the only API that reports
// protocol_version: the ECS service says which target group it registers
// tasks in and on which container port, and nothing more about it (measured
// against the SDK - ecs/types has no ProtocolVersion field at all), and the
// agent cannot know either, because an HTTP2 target group reaches it only as
// requests it cannot parse.
type TargetGroupAPI interface {
	DescribeTargetGroups(ctx context.Context, in *awselb.DescribeTargetGroupsInput, opts ...func(*awselb.Options)) (*awselb.DescribeTargetGroupsOutput, error)
}

// TargetGroup gathers the facts `tetherd doctor`'s target group row judges:
// the ALB target group the service registers its tasks in, and the port the
// agent container serves the ALB on.
//
// Three reads, in this order, because each one names the input of the next:
// the ECS service gives the target group's ARN, ELB gives that group's
// protocol version and port, and the task definition gives the agent's
// TETHERD_PROXY. Any of them failing is one error for the row to report as
// not checked - the judgement is CheckTargetGroup's and it makes no claim
// off a partial read.
//
// The task definition is read here rather than left to the caller even
// though the pidMode row already reads it, because the comparison this row
// makes is between two numbers and half of it is meaningless: a row that
// reported the target group's port against the agent's *default* while the
// deployment had moved it would be a warning about a service that works.
// The two reads are the same call shape and the same grant
// (ecs:DescribeTaskDefinition), so a developer who can pass the pidMode row
// can pass this one.
func TargetGroup(ctx context.Context, svc ServiceAPI, elb TargetGroupAPI, td TaskDefAPI, t Target, definitionARN string) (doctor.TargetGroup, error) {
	agentContainer := t.AgentContainer
	if agentContainer == "" {
		agentContainer = DefaultAgentContainer
	}
	arn, err := targetGroupARN(ctx, svc, t, agentContainer)
	if err != nil {
		return doctor.TargetGroup{}, err
	}
	out, err := elb.DescribeTargetGroups(ctx, &awselb.DescribeTargetGroupsInput{TargetGroupArns: []string{arn}})
	if err != nil {
		return doctor.TargetGroup{}, fmt.Errorf("DescribeTargetGroups %s: %w", arn, err)
	}
	if len(out.TargetGroups) == 0 {
		// The ARN came from the service a moment ago, so this is a target
		// group deleted underneath a service that still forwards to it -
		// reported as itself rather than as a nil dereference.
		return doctor.TargetGroup{}, fmt.Errorf("DescribeTargetGroups %s returned no target group", arn)
	}
	tg := out.TargetGroups[0]
	proxy, err := AgentProxy(ctx, td, definitionARN, agentContainer)
	if err != nil {
		return doctor.TargetGroup{}, err
	}
	return doctor.TargetGroup{
		Name:     aws.ToString(tg.TargetGroupName),
		Protocol: string(tg.Protocol),
		// Verbatim, including the empty string ELB answers for a target
		// group whose protocol has no version (TCP, UDP, TLS, GENEVE).
		// Substituting HTTP1 for it would turn "nothing was reported" into
		// a green row.
		ProtocolVersion: aws.ToString(tg.ProtocolVersion),
		Port:            int(aws.ToInt32(tg.Port)),
		AgentProxy:      proxy,
	}, nil
}

// targetGroupARN is the target group the service registers the agent
// container in.
//
// A service may register several. The agent container's own entry is the one
// this row is about - it is the container on the ALB's data path (spec §5.1)
// - so it is preferred by name, and a service with exactly one entry is
// taken whatever container it names, because that is the single-target-group
// deployment deploy/dev-env/alb.tf describes and its container name is not
// this row's business. Anything else is ambiguous and says so: picking one
// of three target groups by position would report a verdict about
// whichever one ECS happened to list first.
func targetGroupARN(ctx context.Context, svc ServiceAPI, t Target, agentContainer string) (string, error) {
	out, err := svc.DescribeServices(ctx, &awsecs.DescribeServicesInput{
		Cluster:  aws.String(t.Cluster),
		Services: []string{t.Service},
	})
	if err != nil {
		return "", fmt.Errorf("DescribeServices %s/%s: %w", t.Cluster, t.Service, err)
	}
	if len(out.Services) == 0 {
		// ECS answers a service it cannot describe in Failures rather than
		// with an error, so a bare "returned nothing" would hide the reason
		// it gave (MISSING for a wrong cluster, and so on).
		return "", fmt.Errorf("DescribeServices %s/%s returned no service%s", t.Cluster, t.Service, failureSuffix(out.Failures))
	}
	var registered []string
	for _, lb := range out.Services[0].LoadBalancers {
		arn := aws.ToString(lb.TargetGroupArn)
		if arn == "" {
			// A classic load balancer: named, and with no target group at
			// all. Nothing here can describe it.
			continue
		}
		if aws.ToString(lb.ContainerName) == agentContainer {
			return arn, nil
		}
		registered = append(registered, arn)
	}
	switch len(registered) {
	case 0:
		return "", fmt.Errorf("service %s/%s registers no target group, so no ALB delivers to the agent", t.Cluster, t.Service)
	case 1:
		return registered[0], nil
	default:
		return "", fmt.Errorf("service %s/%s registers %d target groups and none of them for container %q, so which one fronts the agent is ambiguous", t.Cluster, t.Service, len(registered), agentContainer)
	}
}

// failureSuffix renders DescribeServices' per-service failures, which is
// where ECS puts "there is no such service" rather than in an error.
func failureSuffix(failures []types.Failure) string {
	for _, f := range failures {
		if reason := aws.ToString(f.Reason); reason != "" {
			return ": " + reason
		}
	}
	return ""
}

// AgentProxy returns TETHERD_PROXY as the agent container's environment sets
// it in the task definition, and "" when the container sets none - which is
// the state in which the agent applies its own default.
//
// Only the literal environment is read, and the two ways a value could be
// somewhere this call cannot see it are refused rather than reported as
// unset: an env-file lives in S3 and a secret in Secrets Manager or SSM, so
// answering "" for either would tell the caller the default applies when the
// deployment may have moved the port. "Not checked" is the honest answer and
// the row says so.
//
// A container that sets the variable literally *and* takes an env-file is
// answered from the literal. That rests on ECS's documented precedence - a
// container definition's environment overrides an environmentFiles entry -
// which is documentation rather than something this suite can measure. If it
// were the other way round, such a deployment would be reported against the
// wrong port; the refusal below is what covers every other env-file case.
func AgentProxy(ctx context.Context, api TaskDefAPI, definitionARN, agentContainer string) (string, error) {
	td, err := describe(ctx, api, definitionARN)
	if err != nil {
		return "", err
	}
	if agentContainer == "" {
		agentContainer = DefaultAgentContainer
	}
	for _, c := range td.ContainerDefinitions {
		if aws.ToString(c.Name) != agentContainer {
			continue
		}
		for _, e := range c.Environment {
			if aws.ToString(e.Name) == proxyEnvVar {
				return aws.ToString(e.Value), nil
			}
		}
		for _, s := range c.Secrets {
			if aws.ToString(s.Name) == proxyEnvVar {
				return "", fmt.Errorf("container %q sources %s from a secret, whose value tetherd cannot read", agentContainer, proxyEnvVar)
			}
		}
		if len(c.EnvironmentFiles) > 0 {
			return "", fmt.Errorf("container %q takes its environment from %d environmentFiles, so whether it sets %s cannot be read from the task definition", agentContainer, len(c.EnvironmentFiles), proxyEnvVar)
		}
		return "", nil
	}
	return "", fmt.Errorf("no container named %q in %s", agentContainer, definitionARN)
}
