package aws

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsS3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	"github.com/entigolabs/entigo-infralib-agent/model"
	"github.com/entigolabs/entigo-infralib-agent/util"
)

const (
	bucketArnFormat  = "arn:aws:s3:::%s"
	allowSSLPolicyId = "AllowSSLRequestsOnly"
)

type S3 struct {
	ctx          context.Context
	awsS3        *awsS3.Client
	region       string
	bucket       string
	repoMetadata *model.RepositoryMetadata
}

func NewS3(ctx context.Context, awsConfig aws.Config, bucket string) *S3 {
	return &S3{
		ctx:    ctx,
		awsS3:  awsS3.NewFromConfig(awsConfig),
		region: awsConfig.Region,
		bucket: bucket,
	}
}

func (s *S3) CreateBucket() (string, bool, error) {
	var createBucketConfiguration *types.CreateBucketConfiguration = nil
	if s.region != "us-east-1" {
		createBucketConfiguration = &types.CreateBucketConfiguration{
			LocationConstraint: types.BucketLocationConstraint(s.region),
		}
	}
	_, err := s.awsS3.CreateBucket(s.ctx, &awsS3.CreateBucketInput{
		Bucket:                    aws.String(s.bucket),
		ACL:                       types.BucketCannedACLPrivate,
		CreateBucketConfiguration: createBucketConfiguration,
	})
	if err != nil {
		var existsError *types.BucketAlreadyExists
		var ownedError *types.BucketAlreadyOwnedByYou
		if errors.As(err, &existsError) || errors.As(err, &ownedError) {
			err = s.checkLifecycle()
			if err != nil {
				return "", false, err
			}
			err = s.ensureAllowSSLRequestsOnlyPolicy()
			if err != nil {
				return "", false, err
			}
			return fmt.Sprintf(bucketArnFormat, s.bucket), false, nil
		}
		return "", false, err
	}
	log.Printf("Created S3 Bucket %s\n", s.bucket)
	err = s.putBucketTags()
	if err != nil {
		return "", false, err
	}
	err = s.putBucketVersioning()
	if err != nil {
		return "", false, err
	}
	err = s.putBucketLifecycle()
	if err != nil {
		return "", false, err
	}
	err = s.putAllowSSLRequestsOnlyPolicy()
	if err != nil {
		return "", false, err
	}
	return fmt.Sprintf(bucketArnFormat, s.bucket), true, nil
}

func (s *S3) Delete() error {
	err := s.truncateBucket()
	if err != nil {
		return checkNotFoundError(err)
	}
	_, err = s.awsS3.DeleteBucket(s.ctx, &awsS3.DeleteBucketInput{
		Bucket: aws.String(s.bucket),
	})
	if err != nil {
		return checkNotFoundError(err)
	}
	log.Printf("Deleted S3 Bucket %s\n", s.bucket)
	return nil
}

func (s *S3) GetRepoMetadata() (*model.RepositoryMetadata, error) {
	if s.repoMetadata != nil {
		return s.repoMetadata, nil
	}
	exists, err := s.BucketExists()
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, nil
	}
	s.repoMetadata = &model.RepositoryMetadata{
		Name: s.bucket,
		URL:  fmt.Sprintf("%s/", s.bucket),
	}
	return s.repoMetadata, nil
}

func (s *S3) addEncryption(kmsArn string) error {
	encryption, err := s.awsS3.GetBucketEncryption(s.ctx, &awsS3.GetBucketEncryptionInput{
		Bucket: aws.String(s.bucket),
	})
	if err != nil {
		return err
	}
	if hasKMSEncryption(encryption.ServerSideEncryptionConfiguration) {
		return nil
	}
	_, err = s.awsS3.PutBucketEncryption(s.ctx, &awsS3.PutBucketEncryptionInput{
		Bucket: aws.String(s.bucket),
		ServerSideEncryptionConfiguration: &types.ServerSideEncryptionConfiguration{
			Rules: []types.ServerSideEncryptionRule{
				{ApplyServerSideEncryptionByDefault: &types.ServerSideEncryptionByDefault{
					KMSMasterKeyID: aws.String(kmsArn),
					SSEAlgorithm:   types.ServerSideEncryptionAwsKms,
				},
					BucketKeyEnabled: aws.Bool(true),
				},
			},
		},
	})
	return err
}

func hasKMSEncryption(config *types.ServerSideEncryptionConfiguration) bool {
	if config == nil {
		return false
	}
	for _, rule := range config.Rules {
		if rule.ApplyServerSideEncryptionByDefault == nil {
			continue
		}
		if rule.ApplyServerSideEncryptionByDefault.SSEAlgorithm == types.ServerSideEncryptionAwsKms {
			return true
		}
	}
	return false
}

func (s *S3) PutFile(file string, content []byte) error {
	_, err := s.awsS3.PutObject(s.ctx, &awsS3.PutObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(file),
		Body:   bytes.NewReader(content),
	})
	return err
}

func (s *S3) GetFile(file string) ([]byte, error) {
	output, err := s.awsS3.GetObject(s.ctx, &awsS3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(file),
	})
	if err != nil {
		var noSuchKey *types.NoSuchKey
		var apiErr smithy.APIError
		if errors.As(err, &noSuchKey) || (errors.As(err, &apiErr) && apiErr.ErrorCode() == "NoSuchKey") {
			return nil, nil
		}
		return nil, err
	}
	defer func(Body io.ReadCloser) {
		_ = Body.Close()
	}(output.Body)
	return io.ReadAll(output.Body)
}

func (s *S3) DeleteFiles(files []string) error {
	if len(files) == 0 {
		return nil
	}
	var objects []types.ObjectIdentifier
	for _, file := range files {
		objects = append(objects, types.ObjectIdentifier{
			Key: aws.String(file),
		})
	}
	for len(objects) > 0 {
		limit := util.MinInt(len(objects), 1000) // Max 1000 objects per request
		_, err := s.awsS3.DeleteObjects(s.ctx, &awsS3.DeleteObjectsInput{
			Bucket: aws.String(s.bucket),
			Delete: &types.Delete{Objects: objects[:limit]},
		})
		err = checkNotFoundError(err)
		if err != nil {
			return err
		}
		objects = objects[limit:]
	}
	return nil
}

func (s *S3) DeleteFile(file string) error {
	_, err := s.awsS3.DeleteObject(s.ctx, &awsS3.DeleteObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(file),
	})
	return checkNotFoundError(err)
}

func (s *S3) CheckFolderExists(folder string) (bool, error) {
	if !strings.HasSuffix(folder, "/") {
		folder += "/"
	}
	output, err := s.awsS3.ListObjectsV2(s.ctx, &awsS3.ListObjectsV2Input{
		Bucket:  aws.String(s.bucket),
		Prefix:  aws.String(folder),
		MaxKeys: aws.Int32(1),
	})
	if err != nil {
		return false, err
	}
	// Err is nil even if folder doesn't exist, can only check if folder with files exists
	return output.Contents != nil, nil
}

func (s *S3) ListFolderFiles(folder string) ([]string, error) {
	if !strings.HasSuffix(folder, "/") {
		folder += "/"
	}
	var files []string
	paginator := awsS3.NewListObjectsV2Paginator(s.awsS3, &awsS3.ListObjectsV2Input{
		Bucket: aws.String(s.bucket),
		Prefix: aws.String(folder),
	})
	for paginator.HasMorePages() {
		output, err := paginator.NextPage(s.ctx)
		if err != nil {
			return nil, err
		}
		for _, object := range output.Contents {
			files = append(files, *object.Key)
		}
	}
	return files, nil
}

func (s *S3) ListFolderFilesWithExclude(folder string, excludeFolders model.Set[string]) ([]string, error) {
	if !strings.HasSuffix(folder, "/") {
		folder += "/"
	}
	output, err := s.awsS3.ListObjectsV2(s.ctx, &awsS3.ListObjectsV2Input{
		Bucket:    aws.String(s.bucket),
		Prefix:    aws.String(folder),
		Delimiter: aws.String("/"),
	})
	if err != nil {
		return nil, err
	}
	var files []string
	for _, object := range output.Contents {
		files = append(files, *object.Key)
	}
	if output.CommonPrefixes == nil {
		return files, nil
	}
	for _, prefix := range output.CommonPrefixes {
		if excludeFolders.Contains(strings.TrimSuffix(strings.TrimPrefix(*prefix.Prefix, folder), "/")) {
			continue
		}
		subFiles, err := s.ListFolderFiles(*prefix.Prefix)
		if err != nil {
			return nil, err
		}
		files = append(files, subFiles...)
	}
	return files, nil
}

func (s *S3) BucketExists() (bool, error) {
	_, err := s.awsS3.HeadBucket(s.ctx, &awsS3.HeadBucketInput{
		Bucket: aws.String(s.bucket),
	})
	if err == nil {
		err = s.checkLifecycle()
		if err != nil {
			return false, err
		}
		return true, s.ensureAllowSSLRequestsOnlyPolicy()
	}
	return false, checkNotFoundError(err)
}

func checkNotFoundError(err error) error {
	var noSuchBucket *types.NoSuchBucket
	var notFound *types.NotFound
	var apiErr smithy.APIError
	if errors.As(err, &noSuchBucket) || errors.As(err, &notFound) || (errors.As(err, &apiErr) && apiErr.ErrorCode() == "NoSuchBucket") {
		return nil
	}
	return err
}

func (s *S3) truncateBucket() error {
	err := s.truncateObjectVersions()
	if err != nil {
		return err
	}
	return s.truncateDeleteMarkers()
}

func (s *S3) truncateObjectVersions() error {
	paginator := awsS3.NewListObjectVersionsPaginator(s.awsS3, &awsS3.ListObjectVersionsInput{
		Bucket: aws.String(s.bucket),
	})
	first := true
	for paginator.HasMorePages() {
		output, err := paginator.NextPage(s.ctx)
		if err != nil {
			return err
		}
		if len(output.Versions) == 0 {
			return nil
		}
		if first {
			log.Printf("Emptying bucket %s...\n", s.bucket)
			first = false
		}
		var objects []types.ObjectIdentifier
		for _, version := range output.Versions {
			objects = append(objects, types.ObjectIdentifier{
				Key:       version.Key,
				VersionId: version.VersionId,
			})
		}
		_, err = s.awsS3.DeleteObjects(s.ctx, &awsS3.DeleteObjectsInput{
			Bucket: aws.String(s.bucket),
			Delete: &types.Delete{Objects: objects},
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *S3) truncateDeleteMarkers() error {
	paginator := awsS3.NewListObjectVersionsPaginator(s.awsS3, &awsS3.ListObjectVersionsInput{
		Bucket: aws.String(s.bucket),
	})
	for paginator.HasMorePages() {
		output, err := paginator.NextPage(s.ctx)
		if err != nil {
			return err
		}
		if len(output.DeleteMarkers) == 0 {
			return nil
		}
		var objects []types.ObjectIdentifier
		for _, marker := range output.DeleteMarkers {
			objects = append(objects, types.ObjectIdentifier{
				Key:       marker.Key,
				VersionId: marker.VersionId,
			})
		}
		_, err = s.awsS3.DeleteObjects(s.ctx, &awsS3.DeleteObjectsInput{
			Bucket: aws.String(s.bucket),
			Delete: &types.Delete{Objects: objects},
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *S3) putBucketTags() error {
	_, err := s.awsS3.PutBucketTagging(s.ctx, &awsS3.PutBucketTaggingInput{
		Bucket: aws.String(s.bucket),
		Tagging: &types.Tagging{
			TagSet: []types.Tag{{
				Key:   aws.String(model.ResourceTagKey),
				Value: aws.String(model.ResourceTagValue),
			}},
		},
	})
	return err
}

func (s *S3) putBucketVersioning() error {
	_, err := s.awsS3.PutBucketVersioning(s.ctx, &awsS3.PutBucketVersioningInput{
		Bucket: aws.String(s.bucket),
		VersioningConfiguration: &types.VersioningConfiguration{
			Status: types.BucketVersioningStatusEnabled,
		},
	})
	return err
}

// Lifecycle of the bucket. Versioning is on, so nothing leaves the bucket
// unless a rule removes it: a replaced or deleted file stays as a noncurrent
// version, and a file with a name of its own (a pipeline artifact, a build
// log) stays as it is.
const (
	// A replaced or deleted file can be brought back for this long.
	noncurrentRetentionDays = 30
	// CodePipeline writes every artifact, the plan of a run among them, under
	// a name of its own.
	artifactRetentionDays = 30
	buildLogRetentionDays = 365

	// The rules the agent owns. A rule whose content changes gets a new ID:
	// the IDs are how an existing bucket is found to be behind.
	lifecycleRulePrefix    = "infralib-"
	noncurrentRule         = lifecycleRulePrefix + "noncurrent-30d"
	buildLogRule           = lifecycleRulePrefix + "build-logs-365d"
	artifactRulePrefix     = lifecycleRulePrefix + "artifacts-30d-"
	legacyLifecycleRule    = "DeleteOlderVersions"
	buildLogPrefix         = "build-log-"
	pipelineNameKeyLimit   = 20 // CodePipeline cuts the pipeline name of an artifact key
	artifactNameKeyLimit   = 10 // and the artifact name
	planArtifactName       = "Plan"
	sourceArtifactName     = "source_output"
	incompleteUploadDays   = 1
	lifecycleRuleIDMaxSize = 255
)

// bucketLifecycleRules are the rules every bucket has. Only two kinds of
// rule remove a file that is in use, and both are tied to names nothing else
// has: the build logs here and the artifact folders of a pipeline in
// artifactLifecycleRules. The state of a step, its outputs and its plan are
// at <prefix>-<step>/, the agent's own files at the top and under config/ and
// steps/; for those only old versions are ever removed.
func bucketLifecycleRules(bucket string) []types.LifecycleRule {
	rules := []types.LifecycleRule{
		{
			ID:     aws.String(noncurrentRule),
			Status: types.ExpirationStatusEnabled,
			Filter: &types.LifecycleRuleFilter{Prefix: aws.String("")},
			NoncurrentVersionExpiration: &types.NoncurrentVersionExpiration{
				NoncurrentDays: aws.Int32(noncurrentRetentionDays),
			},
			// A delete marker with no version left behind it.
			Expiration: &types.LifecycleExpiration{ExpiredObjectDeleteMarker: aws.Bool(true)},
			AbortIncompleteMultipartUpload: &types.AbortIncompleteMultipartUpload{
				DaysAfterInitiation: aws.Int32(incompleteUploadDays),
			},
		},
	}
	// The bucket is named <prefix>-<account>-<region> and a step's state is at
	// <prefix>-<step>/terraform.tfstate: with a prefix that itself begins
	// with "build-log" the state would be under the build logs' prefix.
	if !strings.HasPrefix(bucket, strings.TrimSuffix(buildLogPrefix, "-")) {
		rules = append(rules, expireRule(buildLogRule, buildLogPrefix, buildLogRetentionDays))
	}
	return rules
}

// expireRule removes the files under a prefix after a number of days, and
// their versions a day later: the retention is the days given, not those plus
// the retention of noncurrent versions.
func expireRule(id, prefix string, days int32) types.LifecycleRule {
	return types.LifecycleRule{
		ID:         aws.String(id),
		Status:     types.ExpirationStatusEnabled,
		Filter:     &types.LifecycleRuleFilter{Prefix: aws.String(prefix)},
		Expiration: &types.LifecycleExpiration{Days: aws.Int32(days)},
		NoncurrentVersionExpiration: &types.NoncurrentVersionExpiration{
			NoncurrentDays: aws.Int32(1),
		},
	}
}

// artifactKeyPrefixes are the folders CodePipeline keeps the artifacts of a
// pipeline in: <pipeline name>/<artifact name>/, both cut short. The state
// and the outputs of a step live next to them, in <pipeline name>/, which is
// why the rules name the artifact folders and not the pipeline's.
func artifactKeyPrefixes(pipelineName string) []string {
	if len(pipelineName) > pipelineNameKeyLimit {
		pipelineName = pipelineName[:pipelineNameKeyLimit]
	}
	var prefixes []string
	for _, artifact := range []string{planArtifactName, sourceArtifactName} {
		if len(artifact) > artifactNameKeyLimit {
			artifact = artifact[:artifactNameKeyLimit]
		}
		prefixes = append(prefixes, fmt.Sprintf("%s/%s/", pipelineName, artifact))
	}
	return prefixes
}

func artifactLifecycleRules(pipelineNames ...string) []types.LifecycleRule {
	var rules []types.LifecycleRule
	seen := map[string]bool{}
	for _, name := range pipelineNames {
		for _, prefix := range artifactKeyPrefixes(name) {
			id := artifactRulePrefix + prefix
			if seen[id] || len(id) > lifecycleRuleIDMaxSize {
				continue
			}
			seen[id] = true
			rules = append(rules, expireRule(id, prefix, artifactRetentionDays))
		}
	}
	return rules
}

// mergeLifecycleRules adds the wanted rules to the existing ones. It returns
// false when the bucket has them all already. Rules the agent does not own
// are kept, as are the artifact rules of other pipelines; the rule of earlier
// agent versions is dropped.
func mergeLifecycleRules(existing, wanted []types.LifecycleRule) ([]types.LifecycleRule, bool) {
	ids := map[string]bool{}
	changed := false
	var merged []types.LifecycleRule
	for _, rule := range existing {
		id := aws.ToString(rule.ID)
		// An agent rule that is not an artifact rule and is not wanted any
		// more is one whose content has changed since.
		outdated := strings.HasPrefix(id, lifecycleRulePrefix) && !strings.HasPrefix(id, artifactRulePrefix) && !hasRule(wanted, id)
		if id == legacyLifecycleRule || outdated {
			changed = true
			continue
		}
		ids[id] = true
		merged = append(merged, rule)
	}
	for _, rule := range wanted {
		if ids[aws.ToString(rule.ID)] {
			continue
		}
		changed = true
		merged = append(merged, rule)
	}
	return merged, changed
}

func hasRule(rules []types.LifecycleRule, id string) bool {
	for _, rule := range rules {
		if aws.ToString(rule.ID) == id {
			return true
		}
	}
	return false
}

func (s *S3) checkLifecycle() error {
	return s.ensureLifecycleRules(bucketLifecycleRules(s.bucket))
}

// EnsureArtifactExpiration makes the artifacts of the pipelines expire.
func (s *S3) EnsureArtifactExpiration(pipelineNames ...string) error {
	return s.ensureLifecycleRules(append(bucketLifecycleRules(s.bucket), artifactLifecycleRules(pipelineNames...)...))
}

func (s *S3) ensureLifecycleRules(wanted []types.LifecycleRule) error {
	var existing []types.LifecycleRule
	resp, err := s.awsS3.GetBucketLifecycleConfiguration(s.ctx, &awsS3.GetBucketLifecycleConfigurationInput{
		Bucket: aws.String(s.bucket),
	})
	if err != nil {
		var notFound *types.NotFound
		var apiErr smithy.APIError
		if !errors.As(err, &notFound) && !(errors.As(err, &apiErr) && apiErr.ErrorCode() == "NoSuchLifecycleConfiguration") {
			return err
		}
	} else {
		existing = resp.Rules
	}
	rules, changed := mergeLifecycleRules(existing, wanted)
	if !changed {
		return nil
	}
	_, err = s.awsS3.PutBucketLifecycleConfiguration(s.ctx, &awsS3.PutBucketLifecycleConfigurationInput{
		Bucket:                 aws.String(s.bucket),
		LifecycleConfiguration: &types.BucketLifecycleConfiguration{Rules: rules},
	})
	if err == nil {
		log.Printf("Updated the lifecycle rules of bucket %s\n", s.bucket)
	}
	return err
}

func (s *S3) putBucketLifecycle() error {
	return s.checkLifecycle()
}

func (s *S3) newAllowSSLStatement() PolicyStatement {
	bucketArn := fmt.Sprintf(bucketArnFormat, s.bucket)
	return PolicyStatement{
		Sid:       allowSSLPolicyId,
		Effect:    "Deny",
		Principal: "*",
		Action:    []string{"s3:*"},
		Resource: []string{
			bucketArn,
			bucketArn + "/*",
		},
		Condition: &PolicyCondition{
			Bool: map[string]string{
				"aws:SecureTransport": "false",
			},
		},
	}
}

func (s *S3) putAllowSSLRequestsOnlyPolicy() error {
	policy := PolicyDocument{
		Version: "2012-10-17",
		Id:      allowSSLPolicyId,
		Statement: []PolicyStatement{
			s.newAllowSSLStatement(),
		},
	}
	return s.putBucketPolicy(policy)
}

func (s *S3) ensureAllowSSLRequestsOnlyPolicy() error {
	output, err := s.awsS3.GetBucketPolicy(s.ctx, &awsS3.GetBucketPolicyInput{
		Bucket: aws.String(s.bucket),
	})
	if err != nil {
		var apiErr smithy.APIError
		if errors.As(err, &apiErr) && apiErr.ErrorCode() == "NoSuchBucketPolicy" {
			return s.putAllowSSLRequestsOnlyPolicy()
		}
		return err
	}

	var currentPolicy PolicyDocument
	err = json.Unmarshal([]byte(*output.Policy), &currentPolicy)
	if err != nil {
		return fmt.Errorf("failed to unmarshal existing bucket policy: %w", err)
	}

	for _, statement := range currentPolicy.Statement {
		if statement.Sid == allowSSLPolicyId {
			return nil
		}
	}
	currentPolicy.Statement = append(currentPolicy.Statement, s.newAllowSSLStatement())
	return s.putBucketPolicy(currentPolicy)
}

func (s *S3) putBucketPolicy(policy PolicyDocument) error {
	policyBytes, err := json.MarshalIndent(policy, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal bucket policy: %w", err)
	}

	policyStr := string(policyBytes)

	_, err = s.awsS3.PutBucketPolicy(s.ctx, &awsS3.PutBucketPolicyInput{
		Bucket: aws.String(s.bucket),
		Policy: aws.String(policyStr),
	})
	if err != nil {
		return fmt.Errorf("failed to put bucket policy: %w", err)
	}

	log.Printf("Successfully updated S3 Bucket Policy for %s with AllowSSLRequestsOnly rule.\n", s.bucket)
	return nil
}

func (s *S3) addDummyZip() error {
	buffer := new(bytes.Buffer)
	zipWriter := zip.NewWriter(buffer)
	err := zipWriter.Close()
	if err != nil {
		return err
	}
	return s.PutFile(model.AgentSource, buffer.Bytes())
}
