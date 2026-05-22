package util

import (
	"context"
	"fmt"
	"time"

	"github.com/entigolabs/entigo-infralib-agent/model"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/clientcredentials"
)

const tokenExpiry = 5 * time.Minute

func GetTokenSource(ctx context.Context, auth *model.ApiOauth) (oauth2.TokenSource, error) {
	if auth == nil {
		return nil, nil
	}
	config := clientcredentials.Config{
		ClientID:     auth.ClientId,
		ClientSecret: auth.ClientSecret,
		TokenURL:     auth.TokenURL,
		Scopes:       auth.Scopes,
	}
	tokenSource := oauth2.ReuseTokenSourceWithExpiry(nil, config.TokenSource(ctx), tokenExpiry)
	if _, err := tokenSource.Token(); err != nil {
		return nil, fmt.Errorf("failed to get oauth2 token: %w", err)
	}
	return tokenSource, nil
}
