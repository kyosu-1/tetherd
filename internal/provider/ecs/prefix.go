package ecs

import (
	"context"
	"fmt"
	"net/netip"
	"strings"

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
		// prefix-list-name is not unique across owners: a customer-managed
		// prefix list can be given the same name as the AWS-managed one, so
		// filtering on the name alone would let a same-named list from
		// another owner (potentially containing 0.0.0.0/0) stand in for it.
		desc, err := api.DescribeManagedPrefixLists(ctx, &awsec2.DescribeManagedPrefixListsInput{
			Filters: []types.Filter{
				{Name: aws.String("prefix-list-name"), Values: []string{name}},
				{Name: aws.String("owner-id"), Values: []string{"AWS"}},
			},
		})
		if err != nil {
			return nil, fmt.Errorf("DescribeManagedPrefixLists %s: %w", name, err)
		}
		// The response order is unspecified and the owner-id filter above is
		// belt, not braces (a name collision within "AWS"-owned lists is not
		// something this code can rule out on its own), so require exactly
		// one match rather than trusting PrefixLists[0].
		if len(desc.PrefixLists) != 1 {
			if len(desc.PrefixLists) == 0 {
				return nil, fmt.Errorf("no AWS-managed prefix list named %s in this region", name)
			}
			var ids []string
			for _, pl := range desc.PrefixLists {
				ids = append(ids, aws.ToString(pl.PrefixListId))
			}
			return nil, fmt.Errorf("expected exactly one AWS-managed prefix list named %s, found %d: %s", name, len(desc.PrefixLists), strings.Join(ids, ", "))
		}
		id := aws.ToString(desc.PrefixLists[0].PrefixListId)

		// GetManagedPrefixListEntries caps at 100 entries per call and the
		// real com.amazonaws.<region>.s3 list has hundreds, so this must
		// follow NextToken to the end rather than reading only the first
		// page.
		var token *string
		for {
			entries, err := api.GetManagedPrefixListEntries(ctx, &awsec2.GetManagedPrefixListEntriesInput{PrefixListId: aws.String(id), NextToken: token})
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
			if entries.NextToken == nil {
				break
			}
			token = entries.NextToken
		}
	}
	return out, nil
}
