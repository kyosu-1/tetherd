package ecs

import (
	"context"
	"fmt"
	"net/netip"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsec2 "github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// EC2API is the subset of the EC2 client VPCCIDRs needs.
type EC2API interface {
	DescribeSubnets(ctx context.Context, in *awsec2.DescribeSubnetsInput, opts ...func(*awsec2.Options)) (*awsec2.DescribeSubnetsOutput, error)
	DescribeVpcs(ctx context.Context, in *awsec2.DescribeVpcsInput, opts ...func(*awsec2.Options)) (*awsec2.DescribeVpcsOutput, error)
}

// TaskRoleCIDR covers the task-role credential and metadata endpoints
// (169.254.170.2). It is always routed through the agent (spec §4.1).
var TaskRoleCIDR = netip.MustParsePrefix("169.254.170.0/24")

// TaskRoleAddr is the address inside TaskRoleCIDR that those endpoints
// actually answer on. Checking that a captured range contains this address
// is stricter than checking that it overlaps TaskRoleCIDR: a range like
// 169.254.170.16/28 overlaps the /24 without covering the endpoint.
var TaskRoleAddr = netip.MustParseAddr("169.254.170.2")

// VPCCIDRs returns every associated IPv4 CIDR of the VPC that subnetID
// belongs to (primary and secondary blocks).
func VPCCIDRs(ctx context.Context, api EC2API, subnetID string) ([]netip.Prefix, error) {
	sn, err := api.DescribeSubnets(ctx, &awsec2.DescribeSubnetsInput{SubnetIds: []string{subnetID}})
	if err != nil {
		return nil, fmt.Errorf("DescribeSubnets %s: %w", subnetID, err)
	}
	if len(sn.Subnets) == 0 {
		return nil, fmt.Errorf("subnet %s not found", subnetID)
	}
	vpcID := aws.ToString(sn.Subnets[0].VpcId)
	vp, err := api.DescribeVpcs(ctx, &awsec2.DescribeVpcsInput{VpcIds: []string{vpcID}})
	if err != nil {
		return nil, fmt.Errorf("DescribeVpcs %s: %w", vpcID, err)
	}
	if len(vp.Vpcs) == 0 {
		return nil, fmt.Errorf("vpc %s not found", vpcID)
	}
	var out []netip.Prefix
	for _, a := range vp.Vpcs[0].CidrBlockAssociationSet {
		if a.CidrBlockState == nil || a.CidrBlockState.State != types.VpcCidrBlockStateCodeAssociated {
			continue
		}
		p, err := netip.ParsePrefix(aws.ToString(a.CidrBlock))
		if err != nil || !p.Addr().Is4() {
			continue
		}
		out = append(out, p.Masked())
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("vpc %s has no associated IPv4 CIDR", vpcID)
	}
	return out, nil
}
