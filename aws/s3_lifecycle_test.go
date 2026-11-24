package aws

import (
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

const testBucket = "dev-123456789012-eu-north-1"

func ruleIDs(rules []types.LifecycleRule) string {
	var ids []string
	for _, rule := range rules {
		ids = append(ids, aws.ToString(rule.ID))
	}
	return strings.Join(ids, " ")
}

func TestBucketLifecycleRules(t *testing.T) {
	rules := bucketLifecycleRules(testBucket)
	byID := map[string]types.LifecycleRule{}
	for _, rule := range rules {
		byID[aws.ToString(rule.ID)] = rule
	}
	all := byID[noncurrentRule]
	if aws.ToString(all.Filter.Prefix) != "" || aws.ToInt32(all.NoncurrentVersionExpiration.NoncurrentDays) != 30 ||
		all.NoncurrentVersionExpiration.NewerNoncurrentVersions != nil {
		t.Errorf("every file: want noncurrent versions gone after 30 days, none kept by count; got %+v", all.NoncurrentVersionExpiration)
	}
	if !aws.ToBool(all.Expiration.ExpiredObjectDeleteMarker) || all.Expiration.Days != nil {
		t.Errorf("every file: want only the leftover delete markers removed, got %+v", all.Expiration)
	}
	if len(rules) != 2 {
		t.Errorf("want the rule for every file and the one for build logs, got %s", ruleIDs(rules))
	}
	logs := byID[buildLogRule]
	if aws.ToString(logs.Filter.Prefix) != "build-log-" || aws.ToInt32(logs.Expiration.Days) != 365 {
		t.Errorf("build logs: want 365 days, got %+v", logs.Expiration)
	}
}

func TestArtifactKeyPrefixes(t *testing.T) {
	got := strings.Join(artifactKeyPrefixes("dev-net"), " ")
	if got != "dev-net/Plan/ dev-net/source_out/" {
		t.Errorf("got %q", got)
	}
	// CodePipeline cuts the pipeline name at 20 characters.
	got = strings.Join(artifactKeyPrefixes("customer1-infrastructure-destroy"), " ")
	if got != "customer1-infrastruc/Plan/ customer1-infrastruc/source_out/" {
		t.Errorf("got %q", got)
	}
	// A pipeline and its destroy pipeline with the same cut name share their rules.
	rules := artifactLifecycleRules("customer1-infrastructure", "customer1-infrastructure-destroy")
	if len(rules) != 2 {
		t.Errorf("want 2 rules for the one cut name, got %s", ruleIDs(rules))
	}
	for _, rule := range rules {
		prefix := aws.ToString(rule.Filter.Prefix)
		// The state of a step is at <pipeline name>/terraform.tfstate.
		if !strings.HasSuffix(prefix, "/Plan/") && !strings.HasSuffix(prefix, "/source_out/") {
			t.Errorf("rule %s covers %q, which is more than an artifact folder", aws.ToString(rule.ID), prefix)
		}
		if aws.ToInt32(rule.Expiration.Days) != 30 {
			t.Errorf("artifacts: want 30 days, got %+v", rule.Expiration)
		}
	}
}

func TestMergeLifecycleRules(t *testing.T) {
	foreign := types.LifecycleRule{ID: aws.String("someone-elses")}
	legacy := types.LifecycleRule{ID: aws.String(legacyLifecycleRule)}
	outdated := types.LifecycleRule{ID: aws.String(lifecycleRulePrefix + "steps-noncurrent-1d")}
	otherPipeline := artifactLifecycleRules("dev-infra")

	// A bucket of an earlier agent version.
	merged, changed := mergeLifecycleRules([]types.LifecycleRule{legacy, foreign}, bucketLifecycleRules(testBucket))
	if !changed || ruleIDs(merged) != "someone-elses "+ruleIDs(bucketLifecycleRules(testBucket)) {
		t.Errorf("legacy bucket: changed=%v rules=%s", changed, ruleIDs(merged))
	}

	// Up to date: nothing to write.
	if _, changed = mergeLifecycleRules(merged, bucketLifecycleRules(testBucket)); changed {
		t.Error("an up to date bucket must not be written again")
	}

	// The bucket check runs without the artifact rules and must keep them.
	withArtifacts := append(append([]types.LifecycleRule{}, merged...), otherPipeline...)
	kept, changed := mergeLifecycleRules(withArtifacts, bucketLifecycleRules(testBucket))
	if changed || len(kept) != len(withArtifacts) {
		t.Errorf("artifact rules of a pipeline must survive the bucket check: changed=%v rules=%s", changed, ruleIDs(kept))
	}

	// Another pipeline adds its rules and keeps the first one's.
	wanted := append(bucketLifecycleRules(testBucket), artifactLifecycleRules("dev-net", "dev-net-destroy")...)
	added, changed := mergeLifecycleRules(withArtifacts, wanted)
	if !changed || len(added) != len(withArtifacts)+4 {
		t.Errorf("want 4 rules added, got %s", ruleIDs(added))
	}

	// A rule of ours whose content has changed since is replaced.
	replaced, changed := mergeLifecycleRules(append([]types.LifecycleRule{outdated}, bucketLifecycleRules(testBucket)...), bucketLifecycleRules(testBucket))
	if !changed || strings.Contains(ruleIDs(replaced), "steps-noncurrent-1d") {
		t.Errorf("outdated rule kept: %s", ruleIDs(replaced))
	}
}

// No rule may remove a file that is in use, other than a build log or a
// pipeline artifact. These are the keys of a real bucket.
func TestCurrentFilesAreKept(t *testing.T) {
	kept := []string{
		"dev-net/terraform.tfstate",
		"dev-net/terraform-output.json",
		"dev-net/plan.json",
		"dev-apps/plan.json",
		"customer1-infrastructure/terraform.tfstate",
		"customer1-infrastruc/terraform.tfstate",
		"config.yaml",
		"state.yaml",
		"agent-source.zip",
		"config/net/main.yaml",
		"steps/dev-net/main.tf",
		"steps/dev-net/backend.conf",
		"steps/dev-net/.terraform/terraform.lock.hcl",
		"steps/dev-net/.terraform/providers/registry.opentofu.org/hashicorp/aws/6.71.0/linux_amd64/terraform-provider-aws",
	}
	expired := []string{
		"dev-net/Plan/Yz3CZp7",
		"dev-net/source_out/Zj5VPca.zip",
		"dev-net-destroy/source_out/04fuI4Y.zip",
		"customer1-infrastruc/Plan/abc",
		"build-log-dev-net/a7bbaebe-5c68-4c9f-8670-ada66d021a77.gz",
	}
	rules := append(bucketLifecycleRules(testBucket),
		artifactLifecycleRules("dev-net", "dev-net-destroy", "dev-apps", "customer1-infrastructure", "customer1-infrastructure-destroy")...)
	removes := func(key string) string {
		for _, rule := range rules {
			if rule.Expiration != nil && rule.Expiration.Days != nil && strings.HasPrefix(key, aws.ToString(rule.Filter.Prefix)) {
				return aws.ToString(rule.ID)
			}
		}
		return ""
	}
	for _, key := range kept {
		if id := removes(key); id != "" {
			t.Errorf("%s would be removed by rule %s", key, id)
		}
	}
	for _, key := range expired {
		if removes(key) == "" {
			t.Errorf("%s is covered by no rule", key)
		}
	}

	// A prefix that begins with "build-log" puts the state under the build
	// logs' prefix; such a bucket gets no build log rule.
	for _, rule := range bucketLifecycleRules("build-log-123456789012-eu-north-1") {
		if rule.Expiration != nil && rule.Expiration.Days != nil {
			t.Errorf("rule %s removes current files of a bucket whose state is at build-log-<step>/", aws.ToString(rule.ID))
		}
	}
}
