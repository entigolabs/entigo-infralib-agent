package aws

import (
	"encoding/base64"
	"slices"
	"testing"

	"github.com/entigolabs/entigo-infralib-agent/model"
)

func TestECRRegistryRegion(t *testing.T) {
	tests := []struct {
		registry string
		region   string
		ok       bool
	}{
		{"123456789012.dkr.ecr.eu-north-1.amazonaws.com", "eu-north-1", true},
		{"123456789012.dkr.ecr-fips.us-gov-west-1.amazonaws.com", "us-gov-west-1", true},
		{"123456789012.dkr.ecr.cn-north-1.amazonaws.com.cn", "cn-north-1", true},
		{"123456789012.DKR.ECR.EU-NORTH-1.AMAZONAWS.COM", "eu-north-1", true},
		{"123456789012.dkr-ecr.eu-north-1.on.aws", "eu-north-1", true},
		{"123456789012.dkr-ecr-fips.us-gov-west-1.on.aws", "us-gov-west-1", true},
		{"123456789012.dkr.ecr.eu-north-1.on.aws", "", false},
		{"123456789012.dkr-ecr.eu-north-1.amazonaws.com", "", false},
		{"public.ecr.aws", "", false},
		{"ghcr.io", "", false},
		{"123456789012.dkr.ecr.eu-north-1.amazonaws.com/entigolabs/release", "", false},
		{"12345.dkr.ecr.eu-north-1.amazonaws.com", "", false},
		{"evil.com/123456789012.dkr.ecr.eu-north-1.amazonaws.com", "", false},
		{"", "", false},
	}
	for _, test := range tests {
		region, ok := ecrRegistryRegion(test.registry)
		if ok != test.ok || region != test.region {
			t.Errorf("ecrRegistryRegion(%q) = %q, %t; want %q, %t", test.registry, region, ok, test.region, test.ok)
		}
	}
}

func TestDecodeECRAuthorizationToken(t *testing.T) {
	username, password, err := decodeECRAuthorizationToken(base64.StdEncoding.EncodeToString([]byte("AWS:ey:J:token")))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if username != "AWS" || password != "ey:J:token" {
		t.Errorf("got %q / %q, want AWS / ey:J:token", username, password)
	}
	for _, token := range []string{"not base64!", base64.StdEncoding.EncodeToString([]byte("AWS")), base64.StdEncoding.EncodeToString([]byte(":token"))} {
		if _, _, err := decodeECRAuthorizationToken(token); err == nil {
			t.Errorf("decodeECRAuthorizationToken(%q) expected an error", token)
		}
	}
}

func TestECRRepositoryArns(t *testing.T) {
	arns := ecrRepositoryArns([]model.ConfigSource{
		{URL: "oci://123456789012.dkr.ecr.eu-north-1.amazonaws.com/entigolabs/release"},
		{URL: "oci://123456789012.dkr.ecr.eu-north-1.amazonaws.com/entigolabs/release/"},
		{URL: "oci://210987654321.dkr.ecr.us-east-1.amazonaws.com"},
		{URL: "oci://123456789012.dkr.ecr.eu-north-1.amazonaws.com/private", Username: "user"},
		{URL: "oci://ghcr.io/entigolabs/entigo-infralib-release"},
		{URL: "https://github.com/entigolabs/entigo-infralib-release"},
	})
	want := []string{
		"arn:aws:ecr:eu-north-1:123456789012:repository/entigolabs/release/*",
		"arn:aws:ecr:us-east-1:210987654321:repository/*",
	}
	if !slices.Equal(arns, want) {
		t.Errorf("ecrRepositoryArns() = %v, want %v", arns, want)
	}
}
