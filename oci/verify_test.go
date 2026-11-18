package oci

import (
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
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
		"public.ecr.aws/entigolabs/entigo-infralib": {infralibRepo},
		"ghcr.io/entigolabs/-release":               {"https://github.com/entigolabs/-release"},
		"ghcr.io/modules-release":                   nil,
		"ghcr.io//modules":                          nil,
		"":                                          nil,
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

func TestSplitDigest(t *testing.T) {
	repo, digest, err := splitDigest("public.ecr.aws/entigolabs/entigo-infralib-release/index@sha256:abc123")
	if err != nil {
		t.Fatal(err)
	}
	if repo != "public.ecr.aws/entigolabs/entigo-infralib-release/index" || digest != "abc123" {
		t.Fatalf("repo=%q digest=%q", repo, digest)
	}
	if _, _, err := splitDigest("public.ecr.aws/entigolabs/index:1.21.2"); err == nil {
		t.Fatal("expected error for non-digest reference")
	}
}

func TestVerifyImageBinding(t *testing.T) {
	const digest = "14a95480023a264c758bbaeb9f12cb9ad6cda229674d63de5c3c5c563e2ef944"
	image := "ghcr.io/entigolabs/entigo-infralib-release/index@sha256:" + digest
	match := []byte(`{"critical":{"image":{"docker-manifest-digest":"sha256:` + digest + `"}}}`)
	if err := verifyImageBinding(match, image); err != nil {
		t.Fatalf("matching payload rejected: %v", err)
	}
	mismatch := []byte(`{"critical":{"image":{"docker-manifest-digest":"sha256:deadbeef"}}}`)
	if err := verifyImageBinding(mismatch, image); err == nil {
		t.Fatal("expected mismatch error")
	}
	if err := verifyImageBinding([]byte("not json"), image); err == nil {
		t.Fatal("expected parse error")
	}
}

func TestParseRekorBundle(t *testing.T) {
	body := base64.StdEncoding.EncodeToString([]byte(`{"apiVersion":"0.0.1","kind":"hashedrekord"}`))
	set := base64.StdEncoding.EncodeToString([]byte("signed-entry-timestamp"))
	logID := hex.EncodeToString([]byte("logid"))
	rekorJSON := `{"SignedEntryTimestamp":"` + set + `","Payload":{"body":"` + body +
		`","integratedTime":1700000000,"logIndex":42,"logID":"` + logID + `"}}`

	entry, err := parseRekorBundle(rekorJSON)
	if err != nil {
		t.Fatal(err)
	}
	if entry.LogIndex != 42 || entry.IntegratedTime != 1700000000 {
		t.Fatalf("logIndex=%d integratedTime=%d", entry.LogIndex, entry.IntegratedTime)
	}
	if entry.KindVersion.Kind != "hashedrekord" || entry.KindVersion.Version != "0.0.1" {
		t.Fatalf("kindVersion=%+v", entry.KindVersion)
	}
	if string(entry.InclusionPromise.SignedEntryTimestamp) != "signed-entry-timestamp" {
		t.Fatalf("SET=%q", entry.InclusionPromise.SignedEntryTimestamp)
	}
	if string(entry.LogId.KeyId) != "logid" {
		t.Fatalf("logID=%q", entry.LogId.KeyId)
	}

	if _, err := parseRekorBundle(""); err == nil {
		t.Fatal("expected error for empty bundle")
	}
	if _, err := parseRekorBundle("{bad"); err == nil {
		t.Fatal("expected error for invalid json")
	}
}

func TestDecodePEM(t *testing.T) {
	pemData := "-----BEGIN CERTIFICATE-----\nYWJj\n-----END CERTIFICATE-----\n" +
		"-----BEGIN CERTIFICATE-----\nZGVm\n-----END CERTIFICATE-----\n"
	blocks := decodePEM(pemData)
	if len(blocks) != 2 {
		t.Fatalf("got %d blocks, want 2", len(blocks))
	}
	if string(blocks[0]) != "abc" || string(blocks[1]) != "def" {
		t.Fatalf("blocks=%q,%q", blocks[0], blocks[1])
	}
	if len(decodePEM("not pem")) != 0 {
		t.Fatal("expected no blocks")
	}
}
