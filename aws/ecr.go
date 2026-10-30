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
	"golang.org/x/sync/singleflight"
)

var ecrRegistryRegex = regexp.MustCompile(`^[0-9]{12}\.(?:dkr\.ecr(?:-fips)?\.([a-z0-9-]+)\.amazonaws\.com(?:\.cn)?|dkr-ecr(?:-fips)?\.([a-z0-9-]+)\.on\.aws)$`)

type ECR struct {
	ctx    context.Context
	config aws.Config
	tokens sync.Map
	group  singleflight.Group
}

func NewECR(ctx context.Context, config aws.Config) *ECR {
	return &ECR{
		ctx:    ctx,
		config: config,
	}
}

// GetRegistryAuth returns nil for non-ECR registries. Tokens are cached per region because a token
// grants access to every registry in its region the caller may pull from, other accounts included.
func (e *ECR) GetRegistryAuth(registry string) (*model.SourceAuth, error) {
	region, ok := ecrRegistryRegion(registry)
	if !ok {
		return nil, nil
	}
	if auth, ok := e.tokens.Load(region); ok {
		return auth.(*model.SourceAuth), nil
	}
	auth, err, _ := e.group.Do(region, func() (any, error) {
		if auth, ok := e.tokens.Load(region); ok {
			return auth, nil
		}
		auth, err := e.getAuthorizationToken(registry, region)
		if err != nil {
			return nil, err
		}
		e.tokens.Store(region, auth)
		return auth, nil
	})
	if err != nil {
		return nil, err
	}
	return auth.(*model.SourceAuth), nil
}

func (e *ECR) getAuthorizationToken(registry, region string) (*model.SourceAuth, error) {
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
	return &model.SourceAuth{Username: username, Password: password}, nil
}

func ecrRegistryRegion(registry string) (string, bool) {
	match := ecrRegistryRegex.FindStringSubmatch(strings.ToLower(registry))
	if match == nil {
		return "", false
	}
	return match[1] + match[2], true
}

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
