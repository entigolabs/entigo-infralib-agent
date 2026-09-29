package azure

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
)

const managementScope = "https://management.azure.com/.default"

// principal is the identity the agent executes as, read from its ARM access token.
type principal struct {
	ObjectId string
	TenantId string
	Type     string
}

func newCredential() (azcore.TokenCredential, error) {
	credential, err := azidentity.NewDefaultAzureCredential(nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create azure credential: %w", err)
	}
	return credential, nil
}

func currentPrincipal(ctx context.Context, credential azcore.TokenCredential) (principal, error) {
	token, err := credential.GetToken(ctx, policy.TokenRequestOptions{Scopes: []string{managementScope}})
	if err != nil {
		return principal{}, fmt.Errorf("failed to get azure access token: %w", err)
	}
	parts := strings.Split(token.Token, ".")
	if len(parts) != 3 {
		return principal{}, errors.New("azure access token is not a JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return principal{}, fmt.Errorf("failed to decode azure access token: %w", err)
	}
	var claims struct {
		Oid    string `json:"oid"`
		Tid    string `json:"tid"`
		IdType string `json:"idtyp"`
		Scope  string `json:"scp"`
	}
	if err = json.Unmarshal(payload, &claims); err != nil {
		return principal{}, fmt.Errorf("failed to parse azure access token: %w", err)
	}
	if claims.Oid == "" || claims.Tid == "" {
		return principal{}, errors.New("azure access token has no oid or tid claim")
	}
	// Only delegated (user) tokens carry scp; guest users have no upn.
	principalType := "ServicePrincipal"
	if claims.IdType == "user" || claims.Scope != "" {
		principalType = "User"
	}
	return principal{ObjectId: claims.Oid, TenantId: claims.Tid, Type: principalType}, nil
}

func isStatus(err error, codes ...int) bool {
	responseError, ok := errors.AsType[*azcore.ResponseError](err)
	return ok && slices.Contains(codes, responseError.StatusCode)
}

func isNotFound(err error) bool {
	return isStatus(err, http.StatusNotFound)
}

func errorCode(err error) string {
	if responseError, ok := errors.AsType[*azcore.ResponseError](err); ok {
		return responseError.ErrorCode
	}
	return ""
}
