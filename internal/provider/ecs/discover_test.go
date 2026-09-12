package ecs

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsecs "github.com/aws/aws-sdk-go-v2/service/ecs"
	"github.com/aws/aws-sdk-go-v2/service/ecs/types"
)

type fakeECS struct {
	arns  []string
	tasks []types.Task
	list  *awsecs.ListTasksInput
}

func (f *fakeECS) ListTasks(_ context.Context, in *awsecs.ListTasksInput, _ ...func(*awsecs.Options)) (*awsecs.ListTasksOutput, error) {
	f.list = in
	return &awsecs.ListTasksOutput{TaskArns: f.arns}, nil
}

func (f *fakeECS) DescribeTasks(_ context.Context, in *awsecs.DescribeTasksInput, _ ...func(*awsecs.Options)) (*awsecs.DescribeTasksOutput, error) {
	return &awsecs.DescribeTasksOutput{Tasks: f.tasks}, nil
}

func task(id string, started time.Time, exec bool, agentStatus string, withAgent bool) types.Task {
	t := types.Task{
		TaskArn:              aws.String("arn:aws:ecs:ap-northeast-1:1:task/c/" + id),
		TaskDefinitionArn:    aws.String("arn:aws:ecs:ap-northeast-1:1:task-definition/" + id + ":1"),
		LastStatus:           aws.String("RUNNING"),
		EnableExecuteCommand: exec,
		StartedAt:            aws.Time(started),
		Attachments: []types.Attachment{{
			Type:    aws.String("ElasticNetworkInterface"),
			Details: []types.KeyValuePair{{Name: aws.String("subnetId"), Value: aws.String("subnet-" + id)}},
		}},
		Containers: []types.Container{{Name: aws.String("app"), RuntimeId: aws.String(id + "-app")}},
	}
	if withAgent {
		c := types.Container{Name: aws.String("tetherd-agent"), RuntimeId: aws.String(id + "-rt")}
		if agentStatus != "" {
			c.ManagedAgents = []types.ManagedAgent{{Name: types.ManagedAgentNameExecuteCommandAgent, LastStatus: aws.String(agentStatus)}}
		}
		t.Containers = append(t.Containers, c)
	}
	return t
}

func TestDiscoverPicksOldestEligible(t *testing.T) {
	now := time.Now()
	f := &fakeECS{
		arns: []string{"a", "b", "c"},
		tasks: []types.Task{
			task("b", now.Add(-1*time.Hour), true, "RUNNING", true),
			task("a", now.Add(-2*time.Hour), true, "RUNNING", true),
			task("c", now.Add(-3*time.Hour), false, "RUNNING", true), // exec disabled → skipped
		},
	}
	got, err := Discover(context.Background(), f, Target{Cluster: "c", Service: "api"})
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "a" || got.RuntimeID != "a-rt" || got.SubnetID != "subnet-a" || got.Cluster != "c" {
		t.Fatalf("got %+v", got)
	}
	if got.DefinitionARN != "arn:aws:ecs:ap-northeast-1:1:task-definition/a:1" {
		t.Fatalf("DefinitionARN = %q, want it filled from DescribeTasks' taskDefinitionArn", got.DefinitionARN)
	}
	if aws.ToString(f.list.Cluster) != "c" || aws.ToString(f.list.ServiceName) != "api" || f.list.DesiredStatus != types.DesiredStatusRunning {
		t.Fatalf("ListTasks input = %+v", f.list)
	}
}

func TestDiscoverExplicitTaskID(t *testing.T) {
	now := time.Now()
	f := &fakeECS{arns: []string{"a", "b"}, tasks: []types.Task{
		task("a", now.Add(-2*time.Hour), true, "RUNNING", true),
		task("b", now.Add(-1*time.Hour), true, "RUNNING", true),
	}}
	got, err := Discover(context.Background(), f, Target{Cluster: "c", Service: "api", TaskID: "b"})
	if err != nil || got.ID != "b" {
		t.Fatalf("got %+v, err %v", got, err)
	}
}

func TestDiscoverNotReadyReasons(t *testing.T) {
	now := time.Now()
	d := task("d", now, true, "RUNNING", true)
	d.LastStatus = aws.String("PENDING")
	f := &fakeECS{arns: []string{"a", "b", "c", "d"}, tasks: []types.Task{
		task("a", now, false, "RUNNING", true),
		task("b", now, true, "PENDING", true),
		task("c", now, true, "", false),
		d,
	}}
	_, err := Discover(context.Background(), f, Target{Cluster: "c", Service: "api"})
	var nr *NotReadyError
	if !errors.As(err, &nr) || len(nr.Reasons) != 4 {
		t.Fatalf("want NotReadyError with 4 reasons, got %v", err)
	}
	found := false
	for _, r := range nr.Reasons {
		if strings.Contains(r, "not RUNNING yet") {
			found = true
		}
	}
	if !found {
		t.Fatalf("want a reason containing %q, got %v", "not RUNNING yet", nr.Reasons)
	}
}

func TestDiscoverCollectsAllRuntimeIDsAgentFirst(t *testing.T) {
	now := time.Now()
	f := &fakeECS{arns: []string{"a"}, tasks: []types.Task{task("a", now, true, "RUNNING", true)}}
	got, err := Discover(context.Background(), f, Target{Cluster: "c", Service: "api"})
	if err != nil {
		t.Fatal(err)
	}
	// task() builds containers in the order: app, then tetherd-agent.
	want := []string{"a-rt", "a-app"}
	if len(got.RuntimeIDs) != len(want) {
		t.Fatalf("RuntimeIDs = %v, want %v", got.RuntimeIDs, want)
	}
	for i := range want {
		if got.RuntimeIDs[i] != want[i] {
			t.Fatalf("RuntimeIDs = %v, want %v (agent container first)", got.RuntimeIDs, want)
		}
	}
	if got.RuntimeID != want[0] {
		t.Fatalf("RuntimeID = %q, want the agent container's %q", got.RuntimeID, want[0])
	}
}

func TestNotReadyErrorEmpty(t *testing.T) {
	nr := &NotReadyError{}
	if nr.Error() != "no attachable task (no running tasks matched)" {
		t.Fatalf("got %q", nr.Error())
	}
}

func TestDiscoverNoTasks(t *testing.T) {
	_, err := Discover(context.Background(), &fakeECS{}, Target{Cluster: "c", Service: "api"})
	if err == nil {
		t.Fatal("want error when no tasks are running")
	}
}

// runningTask builds a fully eligible RUNNING task with the agent
// container's ExecuteCommandAgent running, matching the shape task() above
// builds for the existing tests.
func runningTask(id string, started time.Time) types.Task {
	return task(id, started, true, "RUNNING", true)
}

// noExecTask is a RUNNING task with ECS Exec disabled - the one field
// DiscoverAll rejects it on.
func noExecTask(id string, started time.Time) types.Task {
	return task(id, started, false, "RUNNING", true)
}

func TestDiscoverAllReturnsEveryEligibleTaskOldestFirst(t *testing.T) {
	// Two eligible tasks: the steal path needs both, because the ALB
	// picks which one a request lands on and a session attached to only
	// one of them silently misses half the traffic.
	api := &fakeECS{
		arns: []string{"arn:aws:ecs:r:1:task/c/newer", "arn:aws:ecs:r:1:task/c/older"},
		tasks: []types.Task{
			runningTask("newer", time.Unix(2000, 0)),
			runningTask("older", time.Unix(1000, 0)),
		},
	}
	all, err := DiscoverAll(context.Background(), api, Target{Cluster: "c", Service: "s"})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("got %d tasks, want 2", len(all))
	}
	if all[0].ID != "older" || all[1].ID != "newer" {
		t.Fatalf("order = %s, %s; want older first (primary is the oldest task)", all[0].ID, all[1].ID)
	}
}

func TestDiscoverStillReturnsTheOldestOnly(t *testing.T) {
	api := &fakeECS{
		arns: []string{"arn:aws:ecs:r:1:task/c/newer", "arn:aws:ecs:r:1:task/c/older"},
		tasks: []types.Task{
			runningTask("newer", time.Unix(2000, 0)),
			runningTask("older", time.Unix(1000, 0)),
		},
	}
	task, err := Discover(context.Background(), api, Target{Cluster: "c", Service: "s"})
	if err != nil {
		t.Fatal(err)
	}
	if task.ID != "older" {
		t.Fatalf("Discover returned %s, want the oldest", task.ID)
	}
}

func TestDiscoverAllSkipsTheIneligibleAndSaysWhy(t *testing.T) {
	// One task RUNNING with the agent, one without ECS Exec. The eligible
	// one must come back and the reason for the other must not be lost -
	// a developer whose deploy is half-rolled needs to know which task is
	// not attachable and why.
	api := &fakeECS{
		arns: []string{"arn:aws:ecs:r:1:task/c/good", "arn:aws:ecs:r:1:task/c/noexec"},
		tasks: []types.Task{
			runningTask("good", time.Unix(1000, 0)),
			noExecTask("noexec", time.Unix(1100, 0)),
		},
	}
	all, err := DiscoverAll(context.Background(), api, Target{Cluster: "c", Service: "s"})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[0].ID != "good" {
		t.Fatalf("got %v, want only the eligible task", all)
	}
}

func TestDiscoverAllWithNoEligibleTaskReportsEveryReason(t *testing.T) {
	api := &fakeECS{
		arns:  []string{"arn:aws:ecs:r:1:task/c/noexec"},
		tasks: []types.Task{noExecTask("noexec", time.Unix(1100, 0))},
	}
	_, err := DiscoverAll(context.Background(), api, Target{Cluster: "c", Service: "s"})
	var nr *NotReadyError
	if !errors.As(err, &nr) {
		t.Fatalf("err = %v, want a *NotReadyError", err)
	}
	if len(nr.Reasons) != 1 || !strings.Contains(nr.Reasons[0], "ECS Exec is disabled") {
		t.Fatalf("reasons = %v, want the ECS Exec explanation", nr.Reasons)
	}
}

// Target.AgentContainer had a default and, until v0.4, no configuration
// path to set it - so this pins the other half of exposing it as
// target.agent_container: a task whose sidecar is named something else is
// eligible when the Target says so, and the default name is then *not* what
// is looked for. Without the second half of this test a Target that ignored
// the field entirely and always used the default would still pass the
// first.
func TestDiscoverHonoursARenamedAgentContainer(t *testing.T) {
	now := time.Now()
	renamed := func(id string) types.Task {
		tk := task(id, now, true, "RUNNING", false)
		tk.Containers = append(tk.Containers, types.Container{
			Name:      aws.String("sidecar"),
			RuntimeId: aws.String(id + "-rt"),
			ManagedAgents: []types.ManagedAgent{{
				Name:       types.ManagedAgentNameExecuteCommandAgent,
				LastStatus: aws.String("RUNNING"),
			}},
		})
		return tk
	}
	f := &fakeECS{arns: []string{"a"}, tasks: []types.Task{renamed("a")}}
	got, err := Discover(context.Background(), f, Target{Cluster: "c", Service: "api", AgentContainer: "sidecar"})
	if err != nil {
		t.Fatalf("a task whose sidecar is named %q must be eligible when the target says so: %v", "sidecar", err)
	}
	if got.RuntimeID != "a-rt" {
		t.Errorf("RuntimeID = %q, want the renamed container's (%q)", got.RuntimeID, "a-rt")
	}

	// The same task, with the field left empty, must be rejected by name:
	// the default is DefaultAgentContainer and this task has no such
	// container.
	f2 := &fakeECS{arns: []string{"a"}, tasks: []types.Task{renamed("a")}}
	_, err = Discover(context.Background(), f2, Target{Cluster: "c", Service: "api"})
	if err == nil {
		t.Fatal("with no agent_container set, a task without a container named tetherd-agent must be rejected")
	}
	if !strings.Contains(err.Error(), DefaultAgentContainer) {
		t.Errorf("the reason must name the container it looked for: %v", err)
	}
}
