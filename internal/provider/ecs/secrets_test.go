package ecs

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsecs "github.com/aws/aws-sdk-go-v2/service/ecs"
	"github.com/aws/aws-sdk-go-v2/service/ecs/types"
)

type fakeTaskDef struct {
	in  string
	out *types.TaskDefinition
	err error
}

func (f *fakeTaskDef) DescribeTaskDefinition(_ context.Context, in *awsecs.DescribeTaskDefinitionInput, _ ...func(*awsecs.Options)) (*awsecs.DescribeTaskDefinitionOutput, error) {
	f.in = aws.ToString(in.TaskDefinition)
	if f.err != nil {
		return nil, f.err
	}
	return &awsecs.DescribeTaskDefinitionOutput{TaskDefinition: f.out}, nil
}

func def() *types.TaskDefinition {
	return &types.TaskDefinition{
		PidMode: types.PidModeTask,
		ContainerDefinitions: []types.ContainerDefinition{
			{
				Name: aws.String("app"),
				Secrets: []types.Secret{
					{Name: aws.String("DATABASE_PASSWORD")},
					{Name: aws.String("API_KEY")},
				},
			},
			{Name: aws.String("tetherd-agent")},
		},
	}
}

func TestSecretNames(t *testing.T) {
	api := &fakeTaskDef{out: def()}
	got, err := SecretNames(context.Background(), api, "arn:aws:ecs:...:task-definition/api:7")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || !got["DATABASE_PASSWORD"] || !got["API_KEY"] {
		t.Fatalf("got %v", got)
	}
	if api.in != "arn:aws:ecs:...:task-definition/api:7" {
		t.Errorf("asked for %q", api.in)
	}
}

func TestPIDMode(t *testing.T) {
	got, err := PIDMode(context.Background(), &fakeTaskDef{out: def()}, "arn")
	if err != nil {
		t.Fatal(err)
	}
	if got != "task" {
		t.Fatalf("pidMode = %q", got)
	}
}

// TestSecretNamesRejectsAnEmptyDefinitionARN pins the guard for --transport
// direct, where a task has no DefinitionARN at all: SecretNames must refuse
// rather than call the API with an empty task definition id (which would
// either error confusingly or, worse, resolve to something unrelated).
func TestSecretNamesRejectsAnEmptyDefinitionARN(t *testing.T) {
	api := &fakeTaskDef{out: def()}
	if _, err := SecretNames(context.Background(), api, ""); err == nil {
		t.Fatal("an empty definitionARN must be rejected")
	}
	if api.in != "" {
		t.Fatalf("the API must not be called at all, got asked for %q", api.in)
	}
}

// TestSecretNamesPropagatesAPIErrors pins that a DescribeTaskDefinition
// failure (e.g. no ecs:DescribeTaskDefinition permission) surfaces as an
// error rather than an empty, falsely-reassuring set of secret names.
func TestSecretNamesPropagatesAPIErrors(t *testing.T) {
	api := &fakeTaskDef{err: context.DeadlineExceeded}
	if _, err := SecretNames(context.Background(), api, "arn"); err == nil {
		t.Fatal("an API error must be propagated")
	}
}
