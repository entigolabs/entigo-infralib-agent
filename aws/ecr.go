package aws

import (
	"context"
	"encoding/base64"
	"fmt"
	"regexp"
	"strings"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ecr"
	"github.com/entigolabs/entigo-infralib-agent/model"
)

// ecrRegistryRegex matches the host of a private Amazon ECR registry and
// captures its region: <account>.dkr.ecr.<region>.amazonaws.com, with the FIPS
// and China partition variants. Public ECR (public.ecr.aws) is not one, it
// needs no credentials.
var ecrRegistryRegex = regexp.MustCompile(`^[0-9]{12}\.dkr\.ecr(?:-fips)?\.([a-z0-9-]+)\.amazonaws\.com(?:\.cn)?$`)

// ECR issues credentials for private Amazon ECR registries from the agent's
// own AWS session (ecr:GetAuthorizationToken). A token is bound to a region,
// not to a registry: it opens every registry of the region the session may
// pull from, another account's included when its repository policy allows the
// session's principal. A token stays valid for 12 hours, so one per region is
// fetched per agent run.
type ECR struct {
	ctx    context.Context
	config aws.Config
	mu     sync.Mutex
	tokens map[string]model.SourceAuth
}

func NewECR(ctx context.Context, config aws.Config) *ECR {
	return &ECR{
		ctx:    ctx,
		config: config,
		tokens: make(map[string]model.SourceAuth),
	}
}

// GetRegistryAuth returns the username and token for a private ECR registry
// host, nil for any other host.
func (e *ECR) GetRegistryAuth(registry string) (*model.SourceAuth, error) {
	region, ok := ecrRegistryRegion(registry)
	if !ok {
		return nil, nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if auth, ok := e.tokens[region]; ok {
		return &auth, nil
	}
	client := ecr.NewFromConfig(e.config, func(o *ecr.Options) {
		o.Region = region
	})
	output, err := client.GetAuthorizationToken(e.ctx, &ecr.GetAuthorizationTokenInput{})
	if err != nil {
		return nil, fmt.Errorf("failed to get ECR authorization token for %s: %w", registry, err)
	}
	if len(output.AuthorizationData) == 0 || output.AuthorizationData[0].AuthorizationToken == nil {
		return nil, fmt.Errorf("no ECR authorization token returned for %s", registry)
	}
	username, password, err := decodeECRAuthorizationToken(*output.AuthorizationData[0].AuthorizationToken)
	if err != nil {
		return nil, fmt.Errorf("invalid ECR authorization token for %s: %w", registry, err)
	}
	auth := model.SourceAuth{Username: username, Password: password}
	e.tokens[region] = auth
	return &auth, nil
}

// ecrRegistryRegion returns the region of a private ECR registry host and
// whether the host is one.
func ecrRegistryRegion(registry string) (string, bool) {
	match := ecrRegistryRegex.FindStringSubmatch(strings.ToLower(registry))
	if match == nil {
		return "", false
	}
	return match[1], true
}

// decodeECRAuthorizationToken splits the base64 "username:password" token of
// GetAuthorizationToken into its parts.
func decodeECRAuthorizationToken(token string) (string, string, error) {
	decoded, err := base64.StdEncoding.DecodeString(token)
	if err != nil {
		return "", "", err
	}
	username, password, ok := strings.Cut(string(decoded), ":")
	if !ok || username == "" || password == "" {
		return "", "", fmt.Errorf("token is not username:password")
	}
	return username, password, nil
}
