package ecs

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsecs "github.com/aws/aws-sdk-go-v2/service/ecs"
	"github.com/aws/aws-sdk-go-v2/service/ecs/types"
)

// TaskDefAPI is the slice of the ECS API that reads a task definition.
type TaskDefAPI interface {
	DescribeTaskDefinition(ctx context.Context, in *awsecs.DescribeTaskDefinitionInput, opts ...func(*awsecs.Options)) (*awsecs.DescribeTaskDefinitionOutput, error)
}

// SecretNames returns the names of the variables the task definition sources
// from Secrets Manager or SSM Parameter Store. `tetherd env` masks these by
// default: the values are in the child's environment either way, but printing
// them to a terminal puts them in scrollback and shell history (spec §6.5).
func SecretNames(ctx context.Context, api TaskDefAPI, definitionARN string) (map[string]bool, error) {
	td, err := describe(ctx, api, definitionARN)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, c := range td.ContainerDefinitions {
		for _, s := range c.Secrets {
			if n := aws.ToString(s.Name); n != "" {
				out[n] = true
			}
		}
	}
	return out, nil
}

// PIDMode reports the task definition's pidMode, which must be "task" for the
// agent to read the application container's environment (spec §5.3).
func PIDMode(ctx context.Context, api TaskDefAPI, definitionARN string) (string, error) {
	td, err := describe(ctx, api, definitionARN)
	if err != nil {
		return "", err
	}
	return string(td.PidMode), nil
}

func describe(ctx context.Context, api TaskDefAPI, definitionARN string) (*types.TaskDefinition, error) {
	if definitionARN == "" {
		return nil, fmt.Errorf("no task definition to read")
	}
	out, err := api.DescribeTaskDefinition(ctx, &awsecs.DescribeTaskDefinitionInput{
		TaskDefinition: aws.String(definitionARN),
	})
	if err != nil {
		return nil, fmt.Errorf("DescribeTaskDefinition %s: %w", definitionARN, err)
	}
	if out.TaskDefinition == nil {
		return nil, fmt.Errorf("DescribeTaskDefinition %s returned nothing", definitionARN)
	}
	return out.TaskDefinition, nil
}
