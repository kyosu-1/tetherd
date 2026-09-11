package ecs

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsec2 "github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

type fakeEC2 struct{ subnets, vpcs []string }

func (f *fakeEC2) DescribeSubnets(_ context.Context, in *awsec2.DescribeSubnetsInput, _ ...func(*awsec2.Options)) (*awsec2.DescribeSubnetsOutput, error) {
	f.subnets = in.SubnetIds
	return &awsec2.DescribeSubnetsOutput{Subnets: []types.Subnet{{VpcId: aws.String("vpc-1")}}}, nil
}

func (f *fakeEC2) DescribeVpcs(_ context.Context, in *awsec2.DescribeVpcsInput, _ ...func(*awsec2.Options)) (*awsec2.DescribeVpcsOutput, error) {
	f.vpcs = in.VpcIds
	return &awsec2.DescribeVpcsOutput{Vpcs: []types.Vpc{{CidrBlockAssociationSet: []types.VpcCidrBlockAssociation{
		{CidrBlock: aws.String("10.0.0.0/16"), CidrBlockState: &types.VpcCidrBlockState{State: types.VpcCidrBlockStateCodeAssociated}},
		{CidrBlock: aws.String("10.1.0.0/16"), CidrBlockState: &types.VpcCidrBlockState{State: types.VpcCidrBlockStateCodeAssociated}},
		{CidrBlock: aws.String("10.9.0.0/16"), CidrBlockState: &types.VpcCidrBlockState{State: types.VpcCidrBlockStateCodeDisassociated}},
	}}}}, nil
}

func TestVPCCIDRs(t *testing.T) {
	f := &fakeEC2{}
	got, err := VPCCIDRs(context.Background(), f, "subnet-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].String() != "10.0.0.0/16" || got[1].String() != "10.1.0.0/16" {
		t.Fatalf("got %v", got)
	}
	if f.subnets[0] != "subnet-a" || f.vpcs[0] != "vpc-1" {
		t.Fatalf("inputs: %v %v", f.subnets, f.vpcs)
	}
}
