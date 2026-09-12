package ecs

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsec2 "github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// fakePrefix stands in for the EC2 API. Its zero value behaves like a
// single-page, single-list, AWS-owned "com.amazonaws.ap-northeast-1.s3": the
// fixture TestServiceCIDRs and friends use. lists and pages let a test
// override either independently (a wrong owner/count, or multiple pages) to
// pin the two defenses ServiceCIDRs must apply on top of the raw SDK
// response.
type fakePrefix struct {
	described    []string // "prefix-list-name" filter values seen
	ownerFilters []string // "owner-id" filter values seen
	listID       string
	getCalls     int

	lists []types.ManagedPrefixList // nil = the default single AWS-owned s3 list
	pages [][]types.PrefixListEntry // nil = the default single-page fixture
}

func (f *fakePrefix) DescribeManagedPrefixLists(_ context.Context, in *awsec2.DescribeManagedPrefixListsInput, _ ...func(*awsec2.Options)) (*awsec2.DescribeManagedPrefixListsOutput, error) {
	for _, filter := range in.Filters {
		switch aws.ToString(filter.Name) {
		case "prefix-list-name":
			f.described = append(f.described, filter.Values...)
		case "owner-id":
			f.ownerFilters = append(f.ownerFilters, filter.Values...)
		}
	}
	if f.lists != nil {
		return &awsec2.DescribeManagedPrefixListsOutput{PrefixLists: f.lists}, nil
	}
	return &awsec2.DescribeManagedPrefixListsOutput{PrefixLists: []types.ManagedPrefixList{
		{PrefixListId: aws.String("pl-1"), PrefixListName: aws.String("com.amazonaws.ap-northeast-1.s3"), OwnerId: aws.String("AWS")},
	}}, nil
}

func (f *fakePrefix) GetManagedPrefixListEntries(_ context.Context, in *awsec2.GetManagedPrefixListEntriesInput, _ ...func(*awsec2.Options)) (*awsec2.GetManagedPrefixListEntriesOutput, error) {
	f.listID = aws.ToString(in.PrefixListId)
	f.getCalls++
	pages := f.pages
	if pages == nil {
		pages = [][]types.PrefixListEntry{{
			// 52.219.0.1/20 is deliberately not canonical (a non-zero host
			// part): only .Masked() in ServiceCIDRs turns it into
			// 52.219.0.0/20, so this pins that call.
			{Cidr: aws.String("52.219.0.1/20")},
			{Cidr: aws.String("3.5.152.0/21")},
			{Cidr: aws.String("2600:1f00::/40")}, // IPv6 is skipped in v1
		}}
	}
	idx := 0
	if in.NextToken != nil {
		i, err := strconv.Atoi(aws.ToString(in.NextToken))
		if err != nil {
			return nil, err
		}
		idx = i
	}
	out := &awsec2.GetManagedPrefixListEntriesOutput{Entries: pages[idx]}
	if idx+1 < len(pages) {
		out.NextToken = aws.String(strconv.Itoa(idx + 1))
	}
	return out, nil
}

func TestServiceCIDRs(t *testing.T) {
	f := &fakePrefix{}
	got, err := ServiceCIDRs(context.Background(), f, "ap-northeast-1", []string{"s3"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].String() != "52.219.0.0/20" || got[1].String() != "3.5.152.0/21" {
		t.Fatalf("got %v", got)
	}
	if len(f.described) != 1 || f.described[0] != "com.amazonaws.ap-northeast-1.s3" {
		t.Fatalf("filtered on %v", f.described)
	}
	if f.listID != "pl-1" {
		t.Fatalf("entries fetched for %q", f.listID)
	}
}

func TestServiceCIDRsRejectsUnknownService(t *testing.T) {
	if _, err := ServiceCIDRs(context.Background(), &fakePrefix{}, "ap-northeast-1", []string{"rds"}); err == nil || !strings.Contains(err.Error(), "rds") {
		t.Fatalf("only the services AWS publishes a prefix list for are allowed: %v", err)
	}
}

func TestServiceCIDRsEmpty(t *testing.T) {
	got, err := ServiceCIDRs(context.Background(), &fakePrefix{}, "ap-northeast-1", nil)
	if err != nil || got != nil {
		t.Fatalf("got %v, %v", got, err)
	}
}

// TestServiceCIDRsPaginatesEntries pins that ServiceCIDRs follows NextToken:
// the real com.amazonaws.<region>.s3 list has hundreds of entries, well past
// the 100-per-call cap, so a version that reads only the first page would
// still pass every other test here while silently dropping most of S3's
// ranges.
func TestServiceCIDRsPaginatesEntries(t *testing.T) {
	f := &fakePrefix{pages: [][]types.PrefixListEntry{
		{{Cidr: aws.String("52.219.0.0/20")}},
		{{Cidr: aws.String("3.5.152.0/21")}},
		{{Cidr: aws.String("15.230.0.0/17")}},
	}}
	got, err := ServiceCIDRs(context.Background(), f, "ap-northeast-1", []string{"s3"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"52.219.0.0/20": true, "3.5.152.0/21": true, "15.230.0.0/17": true}
	if len(got) != len(want) {
		t.Fatalf("got %v, want every entry from all 3 pages: %v", got, want)
	}
	for _, p := range got {
		if !want[p.String()] {
			t.Fatalf("unexpected %s in %v", p, got)
		}
		delete(want, p.String())
	}
	if len(want) != 0 {
		t.Fatalf("missing from result: %v (only page 1 was read?)", want)
	}
	if f.getCalls != 3 {
		t.Fatalf("GetManagedPrefixListEntries called %d times, want 3 (one per page)", f.getCalls)
	}
}

// TestServiceCIDRsFiltersByOwner pins that the DescribeManagedPrefixLists
// call itself asks for an AWS-owned list, not merely a same-named one: a
// customer-managed prefix list can share the "com.amazonaws.<region>.s3"
// name with the real AWS-managed one, and prefix-list-name alone is not
// unique across owners.
func TestServiceCIDRsFiltersByOwner(t *testing.T) {
	f := &fakePrefix{}
	if _, err := ServiceCIDRs(context.Background(), f, "ap-northeast-1", []string{"s3"}); err != nil {
		t.Fatal(err)
	}
	if len(f.ownerFilters) != 1 || f.ownerFilters[0] != "AWS" {
		t.Fatalf("owner-id filter = %v, want [AWS]", f.ownerFilters)
	}
}

// TestServiceCIDRsRejectsZeroLists pins the "found nothing" half of the
// exactly-one-list guard.
func TestServiceCIDRsRejectsZeroLists(t *testing.T) {
	f := &fakePrefix{lists: []types.ManagedPrefixList{}}
	_, err := ServiceCIDRs(context.Background(), f, "ap-northeast-1", []string{"s3"})
	if err == nil || !strings.Contains(err.Error(), "com.amazonaws.ap-northeast-1.s3") {
		t.Fatalf("err = %v, want it to name the list it looked for", err)
	}
}

// TestServiceCIDRsRejectsMultipleLists pins the "found more than one" half:
// an unspecified response order plus a same-named customer-managed list
// (even one the owner-id filter should have excluded server-side) must
// never silently pick whichever list the SDK happened to return first - a
// customer-managed com.amazonaws.<region>.s3 containing 0.0.0.0/0 would
// otherwise capture the entire internet for a "remote_services: [s3]" user.
func TestServiceCIDRsRejectsMultipleLists(t *testing.T) {
	f := &fakePrefix{lists: []types.ManagedPrefixList{
		{PrefixListId: aws.String("pl-aws"), PrefixListName: aws.String("com.amazonaws.ap-northeast-1.s3"), OwnerId: aws.String("AWS")},
		{PrefixListId: aws.String("pl-evil"), PrefixListName: aws.String("com.amazonaws.ap-northeast-1.s3"), OwnerId: aws.String("111111111111")},
	}}
	_, err := ServiceCIDRs(context.Background(), f, "ap-northeast-1", []string{"s3"})
	if err == nil {
		t.Fatal("expected an error when more than one prefix list matches")
	}
	if !strings.Contains(err.Error(), "pl-aws") || !strings.Contains(err.Error(), "pl-evil") {
		t.Fatalf("err = %v, want it to name every list it found", err)
	}
}
