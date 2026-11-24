package gcloud

import (
	"testing"

	"cloud.google.com/go/storage"
)

func TestBucketLifecycleRules(t *testing.T) {
	rules := bucketLifecycleRules()
	if len(rules) != 2 {
		t.Fatalf("want 2 rules, got %d", len(rules))
	}
	all, steps := rules[0], rules[1]
	if all.Condition.DaysSinceNoncurrentTime != 30 || len(all.Condition.MatchesPrefix) != 0 || all.Condition.NumNewerVersions != 0 {
		t.Errorf("every file: want noncurrent versions gone after 30 days, got %+v", all.Condition)
	}
	if steps.Condition.DaysSinceNoncurrentTime != 1 || len(steps.Condition.MatchesPrefix) != 1 || steps.Condition.MatchesPrefix[0] != "steps/" {
		t.Errorf("step files: want noncurrent versions gone after a day, got %+v", steps.Condition)
	}
	for _, rule := range rules {
		// A rule without a noncurrent condition would delete live files.
		if rule.Action.Type != storage.DeleteAction || rule.Condition.DaysSinceNoncurrentTime == 0 || rule.Condition.AgeInDays != 0 {
			t.Errorf("rule must only delete noncurrent versions: %+v", rule)
		}
	}
}

func TestMergeLifecycleRules(t *testing.T) {
	legacy := storage.LifecycleRule{
		Action:    storage.LifecycleAction{Type: storage.DeleteAction},
		Condition: storage.LifecycleCondition{NumNewerVersions: 5},
	}
	foreign := storage.LifecycleRule{
		Action:    storage.LifecycleAction{Type: storage.SetStorageClassAction, StorageClass: "NEARLINE"},
		Condition: storage.LifecycleCondition{AgeInDays: 90},
	}

	merged, changed := mergeLifecycleRules([]storage.LifecycleRule{legacy, foreign}, bucketLifecycleRules())
	if !changed || len(merged) != 3 || !sameLifecycleRule(merged[0], foreign) {
		t.Fatalf("legacy bucket: changed=%v rules=%+v", changed, merged)
	}

	// Read back from the bucket a rule can have an empty list where a nil one was written.
	merged[2].Condition.MatchesSuffix = []string{}
	if _, changed = mergeLifecycleRules(merged, bucketLifecycleRules()); changed {
		t.Error("an up to date bucket must not be written again")
	}

	if rules, changed := mergeLifecycleRules(nil, bucketLifecycleRules()); !changed || len(rules) != 2 {
		t.Errorf("a bucket without rules gets both: changed=%v rules=%+v", changed, rules)
	}
}
