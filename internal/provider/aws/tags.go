package aws

import (
	"github.com/aws/aws-sdk-go-v2/aws"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

// Every Bonsai-managed AWS resource is tagged with this set. Tag-based lookup
// is the only state model — there's no separate database or manifest.
const (
	TagCluster = "bonsai:cluster"
	TagEnv     = "bonsai:env"
	TagManaged = "bonsai:managed"
	TagRole    = "bonsai:role" // vpc | subnet | igw | rtb | sg-server | sg-worker | iam | ...
)

func clusterTags(name, env, role string) []ec2types.Tag {
	return []ec2types.Tag{
		{Key: aws.String(TagCluster), Value: aws.String(name)},
		{Key: aws.String(TagEnv), Value: aws.String(env)},
		{Key: aws.String(TagManaged), Value: aws.String("true")},
		{Key: aws.String(TagRole), Value: aws.String(role)},
		{Key: aws.String("Name"), Value: aws.String("bonsai-" + name + "-" + env + "-" + role)},
	}
}

func tagSpec(rt ec2types.ResourceType, name, env, role string) ec2types.TagSpecification {
	return ec2types.TagSpecification{ResourceType: rt, Tags: clusterTags(name, env, role)}
}

func roleFilters(name, env, role string) []ec2types.Filter {
	return []ec2types.Filter{
		{Name: aws.String("tag:" + TagCluster), Values: []string{name}},
		{Name: aws.String("tag:" + TagEnv), Values: []string{env}},
		{Name: aws.String("tag:" + TagRole), Values: []string{role}},
	}
}
