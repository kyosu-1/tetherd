// Package ecs is the ECS-specific side of tetherd: which task to attach to
// and what its VPC looks like. Everything AWS-flavoured lives here so a
// second provider (Cloud Run) can be added beside it.
package ecs

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsecs "github.com/aws/aws-sdk-go-v2/service/ecs"
	"github.com/aws/aws-sdk-go-v2/service/ecs/types"

	"github.com/kyosu-1/tetherd/internal/transport"
)

// ECSAPI is the subset of the ECS client Discover needs.
type ECSAPI interface {
	ListTasks(ctx context.Context, in *awsecs.ListTasksInput, opts ...func(*awsecs.Options)) (*awsecs.ListTasksOutput, error)
	DescribeTasks(ctx context.Context, in *awsecs.DescribeTasksInput, opts ...func(*awsecs.Options)) (*awsecs.DescribeTasksOutput, error)
}

// Target names the service to attach to.
type Target struct {
	Cluster        string
	Service        string
	TaskID         string // optional: pin one task
	AgentContainer string // default "tetherd-agent"
}

// NotReadyError explains why running tasks were rejected.
type NotReadyError struct {
	Reasons []string
}

func (e *NotReadyError) Error() string {
	if len(e.Reasons) == 0 {
		return "no attachable task (no running tasks matched)"
	}
	return "no attachable task:\n        " + strings.Join(e.Reasons, "\n        ")
}

// DiscoverAll returns every eligible RUNNING task - RUNNING, with the agent
// container present, ECS Exec enabled and its ExecuteCommandAgent running
// (spec §6.2) - sorted oldest first by StartedAt.
//
// Oldest-first is not just tidy: it fixes which task is "the primary".
// Later session-layer code designates the oldest attached task as the one
// that serves dial, DNS and the task environment, so a deploy adding a
// newer task never moves the primary out from under an attached session.
//
// The returned slice is never empty with a nil error: zero eligible tasks
// is always an error, either "no RUNNING tasks in service ..." or a
// *NotReadyError carrying the per-task reasons. Callers - including
// Discover below - may read all[0] unchecked on success.
func DiscoverAll(ctx context.Context, api ECSAPI, t Target) ([]transport.Task, error) {
	if t.AgentContainer == "" {
		t.AgentContainer = "tetherd-agent"
	}
	list, err := api.ListTasks(ctx, &awsecs.ListTasksInput{
		Cluster:       aws.String(t.Cluster),
		ServiceName:   aws.String(t.Service),
		DesiredStatus: types.DesiredStatusRunning,
	})
	if err != nil {
		return nil, fmt.Errorf("ListTasks %s/%s: %w", t.Cluster, t.Service, err)
	}
	if len(list.TaskArns) == 0 {
		return nil, fmt.Errorf("no RUNNING tasks in service %s/%s", t.Cluster, t.Service)
	}
	desc, err := api.DescribeTasks(ctx, &awsecs.DescribeTasksInput{Cluster: aws.String(t.Cluster), Tasks: list.TaskArns})
	if err != nil {
		return nil, fmt.Errorf("DescribeTasks: %w", err)
	}

	var eligible []transport.Task
	var reasons []string
	for _, task := range desc.Tasks {
		id := taskID(aws.ToString(task.TaskArn))
		if t.TaskID != "" && id != t.TaskID {
			continue
		}
		if aws.ToString(task.LastStatus) != "RUNNING" {
			reasons = append(reasons, fmt.Sprintf("task %s: not RUNNING yet (currently %q)", id, aws.ToString(task.LastStatus)))
			continue
		}
		if !task.EnableExecuteCommand {
			reasons = append(reasons, fmt.Sprintf("task %s: ECS Exec is disabled (aws ecs update-service --enable-execute-command, then force a new deployment)", id))
			continue
		}
		var agent *types.Container
		for i := range task.Containers {
			if aws.ToString(task.Containers[i].Name) == t.AgentContainer {
				agent = &task.Containers[i]
			}
		}
		if agent == nil {
			reasons = append(reasons, fmt.Sprintf("task %s: no container named %q in the task", id, t.AgentContainer))
			continue
		}
		execStatus := ""
		for _, m := range agent.ManagedAgents {
			if m.Name == types.ManagedAgentNameExecuteCommandAgent {
				execStatus = aws.ToString(m.LastStatus)
			}
		}
		if execStatus != "RUNNING" {
			reasons = append(reasons, fmt.Sprintf("task %s: ExecuteCommandAgent is %q (not RUNNING yet?)", id, execStatus))
			continue
		}
		tt := transport.Task{
			Cluster:       t.Cluster,
			ARN:           aws.ToString(task.TaskArn),
			ID:            id,
			RuntimeID:     aws.ToString(agent.RuntimeId),
			DefinitionARN: aws.ToString(task.TaskDefinitionArn),
		}
		// Collect every container's runtime id, the agent container first:
		// any connected container's SSM agent can carry the forward
		// (awsvpc shares the network namespace), and DescribeTasks cannot
		// tell which ones SSM can actually reach.
		tt.RuntimeIDs = []string{aws.ToString(agent.RuntimeId)}
		for i := range task.Containers {
			c := &task.Containers[i]
			if aws.ToString(c.Name) == t.AgentContainer {
				continue
			}
			if rt := aws.ToString(c.RuntimeId); rt != "" {
				tt.RuntimeIDs = append(tt.RuntimeIDs, rt)
			}
		}
		if task.StartedAt != nil {
			tt.StartedAt = *task.StartedAt
		}
		for _, a := range task.Attachments {
			if aws.ToString(a.Type) != "ElasticNetworkInterface" {
				continue
			}
			for _, d := range a.Details {
				if aws.ToString(d.Name) == "subnetId" {
					tt.SubnetID = aws.ToString(d.Value)
				}
			}
		}
		eligible = append(eligible, tt)
	}
	if len(eligible) == 0 {
		if t.TaskID != "" && len(reasons) == 0 {
			return nil, fmt.Errorf("task %s is not RUNNING in %s/%s", t.TaskID, t.Cluster, t.Service)
		}
		return nil, &NotReadyError{Reasons: reasons}
	}
	sort.Slice(eligible, func(i, j int) bool { return eligible[i].StartedAt.Before(eligible[j].StartedAt) })
	return eligible, nil
}

// Discover returns the oldest eligible RUNNING task - the primary, which
// is where dial, resolve and the task environment come from. Steal needs
// every task (the ALB chooses), so the session layer uses DiscoverAll.
func Discover(ctx context.Context, api ECSAPI, t Target) (transport.Task, error) {
	all, err := DiscoverAll(ctx, api, t)
	if err != nil {
		return transport.Task{}, err
	}
	return all[0], nil
}

// taskID is the last path segment of a task ARN.
func taskID(arn string) string {
	if i := strings.LastIndex(arn, "/"); i >= 0 {
		return arn[i+1:]
	}
	return arn
}
