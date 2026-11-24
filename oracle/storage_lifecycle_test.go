package oracle

import (
	"testing"

	"github.com/oracle/oci-go-sdk/v65/objectstorage"
)

func TestBucketLifecycleRules(t *testing.T) {
	byName := map[string]objectstorage.ObjectLifecycleRule{}
	for _, rule := range bucketLifecycleRules() {
		byName[*rule.Name] = rule
		if rule.Target == nil || *rule.Target == "objects" || !*rule.IsEnabled {
			t.Errorf("rule %s must be enabled and must not act on current objects", *rule.Name)
		}
	}
	all := byName[previousVersionRule]
	if *all.Action != "DELETE" || *all.Target != "previous-object-versions" || *all.TimeAmount != 30 || all.ObjectNameFilter != nil {
		t.Errorf("every file: want previous versions deleted after 30 days, got %+v", all)
	}
	if len(byName) != 2 {
		t.Errorf("want the rule for previous versions and the one for unfinished uploads, got %d", len(byName))
	}
	uploads := byName[incompleteUploadRule]
	if *uploads.Action != "ABORT" || *uploads.Target != "multipart-uploads" {
		t.Errorf("incomplete uploads: want them aborted, got %+v", uploads)
	}
}

func TestMergeLifecycleRules(t *testing.T) {
	foreign := objectstorage.ObjectLifecycleRule{Name: new("someone-elses")}
	outdated := objectstorage.ObjectLifecycleRule{Name: new(lifecycleRulePrefix + "steps-previous-versions-1d")}

	merged, changed := mergeLifecycleRules([]objectstorage.ObjectLifecycleRule{foreign, outdated}, bucketLifecycleRules())
	if !changed || len(merged) != 3 || *merged[0].Name != "someone-elses" {
		t.Fatalf("changed=%v rules=%d", changed, len(merged))
	}
	for _, rule := range merged {
		if *rule.Name == *outdated.Name {
			t.Error("a rule of ours whose content has changed since must be dropped")
		}
	}
	if _, changed = mergeLifecycleRules(merged, bucketLifecycleRules()); changed {
		t.Error("an up to date bucket must not be written again")
	}
}

func TestObjectStorageServiceStatements(t *testing.T) {
	statements := objectStorageServiceStatements("eu-frankfurt-1", "ocid1.compartment.x", "ocid1.key.y", "dev-eu-frankfurt-1")
	if len(statements) != 2 {
		t.Fatalf("want the key and the bucket statement, got %d", len(statements))
	}
	want := "Allow service objectstorage-eu-frankfurt-1 to manage object-family in compartment id ocid1.compartment.x where target.bucket.name = 'dev-eu-frankfurt-1'"
	if statements[1] != want {
		t.Errorf("got  %s\nwant %s", statements[1], want)
	}
}
