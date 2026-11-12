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

const infralibRepo = "https://github.com/entigolabs/entigo-infralib"

func TestReleaseIdentity(t *testing.T) {
	const otherRepo = "https://github.com/entigolabs/entigo-infralib-example-source"
	tests := []struct {
		name   string
		signer string
		mutate func(*certificate.Summary)
		ok     bool
	}{
		{name: "release certificate", signer: infralibRepo, ok: true},
		{name: "release of another repo", signer: otherRepo},
		{name: "same shared workflow called from another repo", signer: infralibRepo, mutate: func(s *certificate.Summary) {
			s.SourceRepositoryURI = otherRepo
			s.BuildConfigURI = otherRepo + cosignBuildConfig
		}},
		{name: "another workflow of the source repo", signer: infralibRepo, mutate: func(s *certificate.Summary) {
			s.BuildConfigURI = infralibRepo + "/.github/workflows/pull-request.yaml@refs/heads/main"
		}},
		{name: "release workflow on another branch", signer: infralibRepo, mutate: func(s *certificate.Summary) {
			s.BuildConfigURI = infralibRepo + "/.github/workflows/release.yaml@refs/heads/feature"
		}},
		{name: "workflow of the source repo signing directly", signer: infralibRepo, mutate: func(s *certificate.Summary) {
			s.SubjectAlternativeName = infralibRepo + cosignBuildConfig
		}},
		{name: "another shared workflow", signer: infralibRepo, mutate: func(s *certificate.Summary) {
			s.SubjectAlternativeName = "https://github.com/entigolabs/entigo-infralib-test/.github/workflows/module-pull-request.yaml@refs/heads/main"
		}},
		{name: "another issuer", signer: infralibRepo, mutate: func(s *certificate.Summary) {
			s.Issuer = "https://accounts.google.com"
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			identity, err := releaseIdentity(tt.signer)
			if err != nil {
				t.Fatal(err)
			}
			summary := releaseCertSummary(t)
			if tt.mutate != nil {
				tt.mutate(&summary)
			}
			err = identity.Verify(summary)
			if tt.ok && err != nil {
				t.Fatalf("expected the identity to match, got %v", err)
			}
			if !tt.ok && err == nil {
				t.Fatal("expected the identity to be rejected")
			}
		})
	}
}

func TestSignerRepositories(t *testing.T) {
	tests := map[string][]string{
		"ghcr.io/entigolabs/entigo-infralib-release": {
			infralibRepo, infralibRepo + "-source"},
		"public.ecr.aws/entigolabs/entigo-infralib-example-release/": {
			"https://github.com/entigolabs/entigo-infralib-example",
			"https://github.com/entigolabs/entigo-infralib-example-source"},
		"registry.example.com/mirror/acme/modules-release": {
			"https://github.com/acme/modules", "https://github.com/acme/modules-source"},
		"ghcr.io/entigolabs/entigo-infralib": nil,
		"ghcr.io/entigolabs/-release":        nil,
		"ghcr.io/modules-release":            nil,
		"":                                   nil,
	}
	for source, want := range tests {
		got := SignerRepositories(source)
		if len(got) != len(want) {
			t.Errorf("%q: got %v, want %v", source, got, want)
			continue
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("%q: got %v, want %v", source, got, want)
			}
		}
	}
}
