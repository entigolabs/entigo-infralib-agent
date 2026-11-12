package oci

import (
	"crypto/x509"
	"os"
	"testing"

	"github.com/sigstore/sigstore-go/pkg/fulcio/certificate"
)

// testdata/release-cert.pem is the Fulcio certificate of a real entigo-infralib
// release signature (public.ecr.aws/entigolabs/entigo-infralib-release/index).
func releaseCertSummary(t *testing.T) certificate.Summary {
	t.Helper()
	data, err := os.ReadFile("testdata/release-cert.pem")
	if err != nil {
		t.Fatal(err)
	}
	blocks := decodePEM(string(data))
	if len(blocks) != 1 {
		t.Fatalf("expected 1 certificate, got %d", len(blocks))
	}
	cert, err := x509.ParseCertificate(blocks[0])
	if err != nil {
		t.Fatal(err)
	}
	summary, err := certificate.SummarizeCertificate(cert)
	if err != nil {
		t.Fatal(err)
	}
	return summary
}

func TestReleaseIdentity(t *testing.T) {
	identity, err := releaseIdentity()
	if err != nil {
		t.Fatal(err)
	}
	const otherRepo = "https://github.com/entigolabs/entigo-infralib-example-source"
	tests := []struct {
		name   string
		mutate func(*certificate.Summary)
		ok     bool
	}{
		{name: "release certificate", mutate: func(*certificate.Summary) {}, ok: true},
		{name: "same shared workflow called from another repo", mutate: func(s *certificate.Summary) {
			s.SourceRepositoryURI = otherRepo
			s.BuildConfigURI = otherRepo + "/.github/workflows/release.yaml@refs/heads/main"
		}},
		{name: "another workflow of the source repo", mutate: func(s *certificate.Summary) {
			s.BuildConfigURI = cosignSourceRepo + "/.github/workflows/pull-request.yaml@refs/heads/main"
		}},
		{name: "release workflow on another branch", mutate: func(s *certificate.Summary) {
			s.BuildConfigURI = cosignSourceRepo + "/.github/workflows/release.yaml@refs/heads/feature"
		}},
		{name: "workflow of the source repo signing directly", mutate: func(s *certificate.Summary) {
			s.SubjectAlternativeName = cosignBuildConfig
		}},
		{name: "another shared workflow", mutate: func(s *certificate.Summary) {
			s.SubjectAlternativeName = "https://github.com/entigolabs/entigo-infralib-test/.github/workflows/module-pull-request.yaml@refs/heads/main"
		}},
		{name: "another issuer", mutate: func(s *certificate.Summary) {
			s.Issuer = "https://accounts.google.com"
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			summary := releaseCertSummary(t)
			tt.mutate(&summary)
			err := identity.Verify(summary)
			if tt.ok && err != nil {
				t.Fatalf("expected the identity to match, got %v", err)
			}
			if !tt.ok && err == nil {
				t.Fatal("expected the identity to be rejected")
			}
		})
	}
}
