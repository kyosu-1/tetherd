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
