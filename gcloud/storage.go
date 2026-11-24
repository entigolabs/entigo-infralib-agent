package gcloud

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"strings"
	"time"

	"cloud.google.com/go/storage"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/entigolabs/entigo-infralib-agent/model"
	"github.com/entigolabs/entigo-infralib-agent/util"
	"github.com/googleapis/gax-go/v2"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
)

type GStorage struct {
	ctx           context.Context
	client        storage.Client
	projectId     string
	location      string
	bucket        string
	bucketHandle  *storage.BucketHandle
	bucketCreated *bool
	repoMetadata  *model.RepositoryMetadata
}

func NewStorage(ctx context.Context, options []option.ClientOption, projectId string, location string, bucket string) (*GStorage, error) {
	client, err := storage.NewClient(ctx, options...)
	if err != nil {
		return nil, err
	}
	bucketHandle := client.Bucket(bucket)
	return &GStorage{
		ctx:          ctx,
		client:       *client,
		projectId:    projectId,
		location:     location,
		bucket:       bucket,
		bucketHandle: bucketHandle,
	}, nil
}

func (g *GStorage) CreateBucket(skipDelay bool) error {
	exists, err := g.BucketExists()
	if err != nil {
		return err
	}
	if exists {
		g.ensureLifecycle()
		return nil
	}
	util.DelayResourceCreation("bucket", g.bucket, skipDelay)
	err = g.bucketHandle.Create(g.ctx, g.projectId, &storage.BucketAttrs{
		Location:                 g.location,
		PublicAccessPrevention:   storage.PublicAccessPreventionEnforced,
		VersioningEnabled:        true,
		SoftDeletePolicy:         &storage.SoftDeletePolicy{RetentionDuration: 0},
		UniformBucketLevelAccess: storage.UniformBucketLevelAccess{Enabled: true},
		Lifecycle:                storage.Lifecycle{Rules: bucketLifecycleRules()},
		Labels:                   map[string]string{model.ResourceTagKey: model.ResourceTagValue},
	})
	if err == nil {
		log.Printf("Created GCloud Storage Bucket %s\n", g.bucket)
		g.bucketCreated = aws.Bool(true)
	}
	return err
}

// Lifecycle of the bucket. Versioning is on, so a replaced or deleted file
// stays as a noncurrent version until a rule removes it. The plan of a run is
// one file per step, replaced by the next run, so its versions are covered
// by the first rule; build logs are in Cloud Logging, not in the bucket.
const (
	// A replaced or deleted file can be brought back for this long.
	noncurrentRetentionDays = 30
	// Earlier agent versions kept five versions of every file, for good.
	legacyNewerVersions = 5
)

func bucketLifecycleRules() []storage.LifecycleRule {
	return []storage.LifecycleRule{
		{
			Action:    storage.LifecycleAction{Type: storage.DeleteAction},
			Condition: storage.LifecycleCondition{DaysSinceNoncurrentTime: noncurrentRetentionDays},
		},
	}
}

// sameLifecycleRule compares what the agent's rules use. A rule has no name,
// and one read back from the bucket need not equal the one written field by
// field.
func sameLifecycleRule(a, b storage.LifecycleRule) bool {
	return a.Action.Type == b.Action.Type &&
		a.Condition.DaysSinceNoncurrentTime == b.Condition.DaysSinceNoncurrentTime &&
		a.Condition.NumNewerVersions == b.Condition.NumNewerVersions &&
		a.Condition.AgeInDays == b.Condition.AgeInDays &&
		a.Condition.Liveness == b.Condition.Liveness &&
		strings.Join(a.Condition.MatchesPrefix, "\n") == strings.Join(b.Condition.MatchesPrefix, "\n") &&
		strings.Join(a.Condition.MatchesSuffix, "\n") == strings.Join(b.Condition.MatchesSuffix, "\n")
}

// isOutdatedLifecycleRule tells the rules the agent has set at some time and
// sets no more: five versions of every file kept for good, and the versions
// of the step files gone after a day.
func isOutdatedLifecycleRule(rule storage.LifecycleRule) bool {
	outdated := []storage.LifecycleRule{
		{
			Action:    storage.LifecycleAction{Type: storage.DeleteAction},
			Condition: storage.LifecycleCondition{NumNewerVersions: legacyNewerVersions},
		},
		{
			Action:    storage.LifecycleAction{Type: storage.DeleteAction},
			Condition: storage.LifecycleCondition{DaysSinceNoncurrentTime: 1, MatchesPrefix: []string{"steps/"}},
		},
	}
	for _, o := range outdated {
		if sameLifecycleRule(rule, o) {
			return true
		}
	}
	return false
}

// mergeLifecycleRules adds the wanted rules to the existing ones and drops
// the rules of earlier agent versions. It returns false when there is nothing
// to change. Rules the agent does not know are kept.
func mergeLifecycleRules(existing, wanted []storage.LifecycleRule) ([]storage.LifecycleRule, bool) {
	changed := false
	var merged []storage.LifecycleRule
	for _, rule := range existing {
		if isOutdatedLifecycleRule(rule) {
			changed = true
			continue
		}
		merged = append(merged, rule)
	}
	for _, rule := range wanted {
		found := false
		for _, have := range merged {
			if sameLifecycleRule(rule, have) {
				found = true
				break
			}
		}
		if !found {
			changed = true
			merged = append(merged, rule)
		}
	}
	return merged, changed
}

// ensureLifecycle brings the rules of an existing bucket up to date. A
// failure leaves the bucket as it was, which is no reason to fail the run.
func (g *GStorage) ensureLifecycle() {
	attrs, err := g.bucketHandle.Attrs(g.ctx)
	if err != nil {
		slog.Warn(common.PrefixWarning(fmt.Sprintf("Failed to read the lifecycle of bucket %s: %v", g.bucket, err)))
		return
	}
	rules, changed := mergeLifecycleRules(attrs.Lifecycle.Rules, bucketLifecycleRules())
	if !changed {
		return
	}
	_, err = g.bucketHandle.Update(g.ctx, storage.BucketAttrsToUpdate{Lifecycle: &storage.Lifecycle{Rules: rules}})
	if err != nil {
		slog.Warn(common.PrefixWarning(fmt.Sprintf("Failed to update the lifecycle of bucket %s: %v", g.bucket, err)))
		return
	}
	log.Printf("Updated the lifecycle rules of bucket %s\n", g.bucket)
}

func (g *GStorage) Delete() error {
	exists, err := g.BucketExists()
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}
	log.Printf("Emptying bucket %s...\n", g.bucket)
	it := g.bucketHandle.Objects(g.ctx, &storage.Query{
		Versions: true,
	})
	for {
		attrs, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			return err
		}
		err = g.bucketHandle.Object(attrs.Name).Generation(attrs.Generation).Delete(g.ctx)
		if err != nil {
			return err
		}
	}
	err = g.bucketHandle.Delete(g.ctx)
	if err == nil {
		log.Printf("Deleted GCloud Storage Bucket %s\n", g.bucket)
	}
	return err
}

func (g *GStorage) BucketExists() (bool, error) {
	if g.bucketCreated != nil {
		return *g.bucketCreated, nil
	}
	_, err := g.bucketHandle.Attrs(g.ctx)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, storage.ErrBucketNotExist) {
		return false, nil
	}
	return false, err
}

func (g *GStorage) GetRepoMetadata() (*model.RepositoryMetadata, error) {
	if g.repoMetadata != nil {
		return g.repoMetadata, nil
	}
	exists, err := g.BucketExists()
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, nil
	}
	g.repoMetadata = &model.RepositoryMetadata{
		Name: g.bucket,
		URL:  g.bucket,
	}
	return g.repoMetadata, nil
}

func (g *GStorage) PutFile(file string, content []byte) error {
	obj := g.bucketHandle.Object(file).Retryer(
		storage.WithBackoff(gax.Backoff{
			Initial:    1 * time.Second,
			Max:        30 * time.Second,
			Multiplier: 2.0,
		}),
		storage.WithPolicy(storage.RetryAlways),
	)
	writer := obj.NewWriter(g.ctx)
	if _, err := writer.Write(content); err != nil {
		_ = writer.Close()
		return fmt.Errorf("failed to write content: %w", err)
	}
	return writer.Close()
}

func (g *GStorage) GetFile(file string) ([]byte, error) {
	reader, err := g.bucketHandle.Object(file).NewReader(g.ctx)
	if err != nil {
		if errors.Is(err, storage.ErrObjectNotExist) {
			return nil, nil
		}
		return nil, err
	}
	defer func(reader *storage.Reader) {
		_ = reader.Close()
	}(reader)
	return io.ReadAll(reader)
}

func (g *GStorage) DeleteFiles(files []string) error {
	for _, file := range files {
		err := g.bucketHandle.Object(file).Delete(g.ctx)
		if err != nil {
			return err
		}
	}
	return nil
}

func (g *GStorage) DeleteFile(file string) error {
	err := g.bucketHandle.Object(file).Delete(g.ctx)
	if err == nil {
		return nil
	}
	if errors.Is(err, storage.ErrObjectNotExist) {
		return nil
	}
	return err
}

func (g *GStorage) CheckFolderExists(folder string) (bool, error) {
	it := g.bucketHandle.Objects(g.ctx, &storage.Query{Prefix: folder})
	_, err := it.Next()
	if errors.Is(err, iterator.Done) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	return true, nil
}

func (g *GStorage) ListFolderFiles(folder string) ([]string, error) {
	if !strings.HasSuffix(folder, "/") {
		folder = folder + "/"
	}
	it := g.bucketHandle.Objects(g.ctx, &storage.Query{Prefix: folder})
	var files []string
	for {
		obj, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		} else if err != nil {
			return nil, err
		}
		files = append(files, obj.Name)
	}
	return files, nil
}

func (g *GStorage) ListFolderFilesWithExclude(folder string, excludeFolders model.Set[string]) ([]string, error) {
	if !strings.HasSuffix(folder, "/") {
		folder = folder + "/"
	}
	it := g.bucketHandle.Objects(g.ctx, &storage.Query{
		Prefix:                   folder,
		Delimiter:                "/",
		IncludeFoldersAsPrefixes: true,
	})
	var files []string
	for {
		obj, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		} else if err != nil {
			return nil, err
		}
		if obj.Name != "" {
			files = append(files, obj.Name)
			continue
		}
		if obj.Prefix == "" || excludeFolders.Contains(strings.TrimSuffix(strings.TrimPrefix(obj.Prefix, folder), "/")) {
			continue
		}
		subFiles, err := g.ListFolderFiles(obj.Prefix)
		if err != nil {
			return nil, err
		}
		files = append(files, subFiles...)
	}
	return files, nil
}
