package oracle

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/entigolabs/entigo-infralib-agent/common"
)

const (
	s3ReadinessTimeout  = 10 * time.Minute
	s3ReadinessInterval = 5 * time.Second
	s3ProbeTimeout      = 30 * time.Second
	// A fresh CSK reaches a region's backend hosts at different times, so one
	// successful probe can be followed by failures on other hosts; require this
	// many consecutive successes before trusting it (streak resets on any failure).
	s3ReadinessStreak = 5
	// Log the "still waiting" line only every Nth attempt to avoid minutes of spam.
	s3ReadinessLogEvery = 6
)

func newS3ProbeClient(endpoint, region, accessKey, secretKey string) *s3.Client {
	return s3.New(s3.Options{
		BaseEndpoint: aws.String(endpoint),
		Region:       region,
		UsePathStyle: true,
		Credentials:  credentials.NewStaticCredentialsProvider(accessKey, secretKey, ""),
	})
}

// probeS3 makes one cheap authenticated read exercising the same signing path as
// the entrypoint copy and the terraform backend.
func probeS3(ctx context.Context, client *s3.Client, bucket string) error {
	_, err := client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
		Bucket:  aws.String(bucket),
		MaxKeys: aws.Int32(1),
	})
	return err
}

// s3CredentialsUsable reports (via a nil error) whether the Customer Secret Key
// authenticates to the S3-compatible endpoint in a single short attempt. Used to
// validate a reused/persisted key before relying on it.
func s3CredentialsUsable(ctx context.Context, endpoint, region, bucket, accessKey, secretKey string) error {
	ctx, cancel := context.WithTimeout(ctx, s3ProbeTimeout)
	defer cancel()
	return probeS3(ctx, newS3ProbeClient(endpoint, region, accessKey, secretKey), bucket)
}

// waitForS3Credentials blocks until a freshly provisioned Customer Secret Key is
// broadly accepted by the S3-compatible endpoint. OCI distributes a new CSK's
// per-region signing key to backend hosts asynchronously, so it requires
// s3ReadinessStreak consecutive successes before returning, so the state backend
// and the entrypoint file-copy don't race the tail of propagation.
func waitForS3Credentials(ctx context.Context, endpoint, region, bucket, accessKey, secretKey string) error {
	client := newS3ProbeClient(endpoint, region, accessKey, secretKey)
	deadline, cancel := context.WithTimeout(ctx, s3ReadinessTimeout)
	defer cancel()
	ticker := time.NewTicker(s3ReadinessInterval)
	defer ticker.Stop()

	streak := 0
	attempts := 0
	var lastErr error
	for {
		// Per-attempt timeout so one hung request can't consume the whole budget.
		attempt, cancelAttempt := context.WithTimeout(deadline, s3ProbeTimeout)
		lastErr = probeS3(attempt, client, bucket)
		cancelAttempt()
		attempts++
		if lastErr == nil {
			streak++
			if streak >= s3ReadinessStreak {
				return nil
			}
		} else {
			streak = 0
			// A fresh CSK is expected to fail for minutes while OCI propagates it,
			// so log the reason once, then only periodically.
			if attempts == 1 {
				slog.Warn(common.PrefixWarning(fmt.Sprintf("Customer Secret Key not yet usable in %s (%v); "+
					"waiting for OCI to propagate it (up to %s)", region, lastErr, s3ReadinessTimeout)))
			} else if attempts%s3ReadinessLogEvery == 0 {
				slog.Warn(common.PrefixWarning(fmt.Sprintf("Still waiting for Customer Secret Key to become usable in %s...", region)))
			}
		}
		select {
		case <-deadline.Done():
			if lastErr == nil {
				lastErr = fmt.Errorf("only %d/%d consecutive probes succeeded", streak, s3ReadinessStreak)
			}
			return fmt.Errorf("customer secret key still not usable on the s3-compatible endpoint after %s "+
				"(re-run to keep waiting on the same persisted key): %w", s3ReadinessTimeout, lastErr)
		case <-ticker.C:
		}
	}
}
