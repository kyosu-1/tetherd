package ecs

import (
	"context"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsec2 "github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

type fakePrefix struct {
	described []string
	listID    string
}

func (f *fakePrefix) DescribeManagedPrefixLists(_ context.Context, in *awsec2.DescribeManagedPrefixListsInput, _ ...func(*awsec2.Options)) (*awsec2.DescribeManagedPrefixListsOutput, error) {
	for _, filter := range in.Filters {
		f.described = append(f.described, filter.Values...)
	}
	return &awsec2.DescribeManagedPrefixListsOutput{PrefixLists: []types.ManagedPrefixList{
		{PrefixListId: aws.String("pl-1"), PrefixListName: aws.String("com.amazonaws.ap-northeast-1.s3")},
	}}, nil
}

func (f *fakePrefix) GetManagedPrefixListEntries(_ context.Context, in *awsec2.GetManagedPrefixListEntriesInput, _ ...func(*awsec2.Options)) (*awsec2.GetManagedPrefixListEntriesOutput, error) {
	f.listID = aws.ToString(in.PrefixListId)
	return &awsec2.GetManagedPrefixListEntriesOutput{Entries: []types.PrefixListEntry{
		{Cidr: aws.String("52.219.0.0/20")},
		{Cidr: aws.String("3.5.152.0/21")},
		{Cidr: aws.String("2600:1f00::/40")}, // IPv6 is skipped in v1
	}}, nil
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
