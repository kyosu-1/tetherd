package ecs

import (
	"context"
	"fmt"
	"net/netip"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsec2 "github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// PrefixListAPI is the subset of the EC2 client ServiceCIDRs needs.
type PrefixListAPI interface {
	DescribeManagedPrefixLists(ctx context.Context, in *awsec2.DescribeManagedPrefixListsInput, opts ...func(*awsec2.Options)) (*awsec2.DescribeManagedPrefixListsOutput, error)
	GetManagedPrefixListEntries(ctx context.Context, in *awsec2.GetManagedPrefixListEntriesInput, opts ...func(*awsec2.Options)) (*awsec2.GetManagedPrefixListEntriesOutput, error)
}

// gatewayServices are the services AWS publishes a managed prefix list for.
// They are the ones reached over a gateway endpoint, where the traffic keeps
// public addresses and a network condition in a policy can only be satisfied
// by leaving through the task's ENI (spec §4.2).
var gatewayServices = map[string]bool{"s3": true, "dynamodb": true}

// ServiceCIDRs resolves service names to the IPv4 prefixes of
// com.amazonaws.<region>.<service>.
func ServiceCIDRs(ctx context.Context, api PrefixListAPI, region string, services []string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, s := range services {
		if !gatewayServices[s] {
			return nil, fmt.Errorf("network.remote_services: %q has no managed prefix list; only s3 and dynamodb do (interface endpoints are reached through network.remote_domains instead)", s)
		}
		name := fmt.Sprintf("com.amazonaws.%s.%s", region, s)
		desc, err := api.DescribeManagedPrefixLists(ctx, &awsec2.DescribeManagedPrefixListsInput{
			Filters: []types.Filter{{Name: aws.String("prefix-list-name"), Values: []string{name}}},
		})
		if err != nil {
			return nil, fmt.Errorf("DescribeManagedPrefixLists %s: %w", name, err)
		}
		if len(desc.PrefixLists) == 0 {
			return nil, fmt.Errorf("no managed prefix list named %s in this region", name)
		}
		id := aws.ToString(desc.PrefixLists[0].PrefixListId)
		entries, err := api.GetManagedPrefixListEntries(ctx, &awsec2.GetManagedPrefixListEntriesInput{PrefixListId: aws.String(id)})
		if err != nil {
			return nil, fmt.Errorf("GetManagedPrefixListEntries %s: %w", id, err)
		}
		for _, e := range entries.Entries {
			p, err := netip.ParsePrefix(aws.ToString(e.Cidr))
			if err != nil || !p.Addr().Is4() {
				continue
			}
			out = append(out, p.Masked())
		}
	}
	return out, nil
}
