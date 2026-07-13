package oci

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	ggcrremote "github.com/google/go-containerregistry/pkg/v1/remote"
	protobundle "github.com/sigstore/protobuf-specs/gen/pb-go/bundle/v1"
	protocommon "github.com/sigstore/protobuf-specs/gen/pb-go/common/v1"
	protorekor "github.com/sigstore/protobuf-specs/gen/pb-go/rekor/v1"
	"github.com/sigstore/sigstore-go/pkg/bundle"
	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/verify"
)

// entigolabs release images are signed by GitHub Actions workflows in the
// entigo-infralib repo, so verification pins that OIDC issuer and identity.
const (
	cosignOIDCIssuer     = "https://token.actions.githubusercontent.com"
	cosignIdentityRegexp = `^https://github\.com/entigolabs/entigo-infralib/\.github/workflows/.*@refs/.*$`

	// classic cosign signature layer annotations.
	sigAnnotation   = "dev.cosignproject.cosign/signature"
	certAnnotation  = "dev.sigstore.cosign/certificate"
	chainAnnotation = "dev.sigstore.cosign/chain"
	rekorAnnotation = "dev.sigstore.cosign/bundle"

	// bundle v0.1 requires only an inclusion promise (the SET), which is all a
	// classic cosign rekor bundle carries, and still permits an X.509 chain.
	bundleMediaType = "application/vnd.dev.sigstore.bundle+json;version=0.1"
)

type Verifier interface {
	Verify(ctx context.Context, image string) error
}

// CosignVerifier verifies OCI index image signatures with sigstore-go. The
// trusted root is fetched via TUF, or loaded from a local bundle when
// trustBundlePath is set (offline verification). Initialization is lazy so runs
// that never verify pay no TUF/network cost.
type CosignVerifier struct {
	trustBundlePath string
	once            sync.Once
	verifier        *verify.Verifier
	identity        verify.CertificateIdentity
	initErr         error
}

func NewCosignVerifier(trustBundlePath string) *CosignVerifier {
	return &CosignVerifier{trustBundlePath: trustBundlePath}
}

func (v *CosignVerifier) init() {
	trustedRoot, err := v.loadTrustedRoot()
	if err != nil {
		v.initErr = fmt.Errorf("failed to load sigstore trusted root: %w", err)
		return
	}
	// GitHub keyless signatures carry a Rekor transparency-log entry but no
	// signed timestamp, so require one observer (log) timestamp.
	v.verifier, err = verify.NewVerifier(trustedRoot,
		verify.WithTransparencyLog(1), verify.WithObserverTimestamps(1))
	if err != nil {
		v.initErr = fmt.Errorf("failed to build signature verifier: %w", err)
		return
	}
	v.identity, err = verify.NewShortCertificateIdentity(cosignOIDCIssuer, "", "", cosignIdentityRegexp)
	if err != nil {
		v.initErr = fmt.Errorf("failed to build certificate identity: %w", err)
	}
}

func (v *CosignVerifier) loadTrustedRoot() (root.TrustedMaterial, error) {
	if v.trustBundlePath != "" {
		return root.NewTrustedRootFromPath(v.trustBundlePath)
	}
	return root.FetchTrustedRoot()
}

func (v *CosignVerifier) Verify(ctx context.Context, image string) error {
	v.once.Do(v.init)
	if v.initErr != nil {
		return v.initErr
	}
	entity, payload, err := fetchSignedEntity(ctx, image)
	if err != nil {
		return err
	}
	policy := verify.NewPolicy(verify.WithArtifact(bytes.NewReader(payload)),
		verify.WithCertificateIdentity(v.identity))
	if _, err := v.verifier.Verify(entity, policy); err != nil {
		return fmt.Errorf("signature verification failed for %s: %w", image, err)
	}
	// The signature attests a simplesigning payload; bind that payload to the
	// image we resolved so a valid signature for a different image can't pass.
	if err := verifyImageBinding(payload, image); err != nil {
		return err
	}
	return nil
}

// fetchSignedEntity retrieves the classic cosign signature stored at the
// sha256-<digest>.sig tag and assembles it into a sigstore bundle, returning the
// bundle and the signed simplesigning payload.
func fetchSignedEntity(ctx context.Context, image string) (verify.SignedEntity, []byte, error) {
	repo, digestHex, err := splitDigest(image)
	if err != nil {
		return nil, nil, err
	}
	sigRef, err := name.NewTag(fmt.Sprintf("%s:sha256-%s.sig", repo, digestHex))
	if err != nil {
		return nil, nil, fmt.Errorf("invalid signature reference for %s: %w", image, err)
	}
	img, err := ggcrremote.Image(sigRef,
		ggcrremote.WithContext(ctx), ggcrremote.WithAuthFromKeychain(authn.DefaultKeychain))
	if err != nil {
		return nil, nil, fmt.Errorf("failed to fetch signature image %s: %w", sigRef, err)
	}
	manifest, err := img.Manifest()
	if err != nil {
		return nil, nil, fmt.Errorf("failed to read signature manifest for %s: %w", image, err)
	}
	layers, err := img.Layers()
	if err != nil {
		return nil, nil, fmt.Errorf("failed to read signature layers for %s: %w", image, err)
	}
	for i, desc := range manifest.Layers {
		sig := desc.Annotations[sigAnnotation]
		if sig == "" || i >= len(layers) {
			continue
		}
		payload, err := readLayer(layers[i])
		if err != nil {
			return nil, nil, err
		}
		entity, err := buildBundle(payload, sig, desc.Annotations[certAnnotation],
			desc.Annotations[chainAnnotation], desc.Annotations[rekorAnnotation])
		if err != nil {
			return nil, nil, err
		}
		return entity, payload, nil
	}
	return nil, nil, fmt.Errorf("no cosign signature layer found for %s", image)
}

func readLayer(layer interface{ Uncompressed() (io.ReadCloser, error) }) ([]byte, error) {
	rc, err := layer.Uncompressed()
	if err != nil {
		return nil, fmt.Errorf("failed to open signature payload: %w", err)
	}
	defer func() { _ = rc.Close() }()
	return io.ReadAll(rc)
}

func buildBundle(payload []byte, sigB64, certPEM, chainPEM, rekorJSON string) (*bundle.Bundle, error) {
	signature, err := base64.StdEncoding.DecodeString(sigB64)
	if err != nil {
		return nil, fmt.Errorf("failed to decode signature: %w", err)
	}
	certs, err := parseCertChain(certPEM, chainPEM)
	if err != nil {
		return nil, err
	}
	tlogEntry, err := parseRekorBundle(rekorJSON)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(payload)
	pb := &protobundle.Bundle{
		MediaType: bundleMediaType,
		VerificationMaterial: &protobundle.VerificationMaterial{
			Content: &protobundle.VerificationMaterial_X509CertificateChain{
				X509CertificateChain: &protocommon.X509CertificateChain{Certificates: certs},
			},
			TlogEntries: []*protorekor.TransparencyLogEntry{tlogEntry},
		},
		Content: &protobundle.Bundle_MessageSignature{
			MessageSignature: &protocommon.MessageSignature{
				MessageDigest: &protocommon.HashOutput{
					Algorithm: protocommon.HashAlgorithm_SHA2_256,
					Digest:    digest[:],
				},
				Signature: signature,
			},
		},
	}
	return bundle.NewBundle(pb)
}

func parseCertChain(certPEM, chainPEM string) ([]*protocommon.X509Certificate, error) {
	var certs []*protocommon.X509Certificate
	for _, block := range append(decodePEM(certPEM), decodePEM(chainPEM)...) {
		certs = append(certs, &protocommon.X509Certificate{RawBytes: block})
	}
	if len(certs) == 0 {
		return nil, fmt.Errorf("no certificate found in signature")
	}
	return certs, nil
}

func decodePEM(data string) [][]byte {
	var blocks [][]byte
	rest := []byte(data)
	for {
		block, remaining := pem.Decode(rest)
		if block == nil {
			return blocks
		}
		blocks = append(blocks, block.Bytes)
		rest = remaining
	}
}

func parseRekorBundle(rekorJSON string) (*protorekor.TransparencyLogEntry, error) {
	if rekorJSON == "" {
		return nil, fmt.Errorf("no rekor bundle in signature")
	}
	var rb struct {
		SignedEntryTimestamp string `json:"SignedEntryTimestamp"`
		Payload              struct {
			Body           string `json:"body"`
			IntegratedTime int64  `json:"integratedTime"`
			LogIndex       int64  `json:"logIndex"`
			LogID          string `json:"logID"`
		} `json:"Payload"`
	}
	if err := json.Unmarshal([]byte(rekorJSON), &rb); err != nil {
		return nil, fmt.Errorf("failed to parse rekor bundle: %w", err)
	}
	set, err := base64.StdEncoding.DecodeString(rb.SignedEntryTimestamp)
	if err != nil {
		return nil, fmt.Errorf("failed to decode SignedEntryTimestamp: %w", err)
	}
	body, err := base64.StdEncoding.DecodeString(rb.Payload.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to decode rekor body: %w", err)
	}
	logID, err := hex.DecodeString(rb.Payload.LogID)
	if err != nil {
		return nil, fmt.Errorf("failed to decode rekor logID: %w", err)
	}
	var kind struct {
		APIVersion string `json:"apiVersion"`
		Kind       string `json:"kind"`
	}
	if err := json.Unmarshal(body, &kind); err != nil {
		return nil, fmt.Errorf("failed to parse rekor body kind: %w", err)
	}
	return &protorekor.TransparencyLogEntry{
		LogIndex:          rb.Payload.LogIndex,
		LogId:             &protocommon.LogId{KeyId: logID},
		KindVersion:       &protorekor.KindVersion{Kind: kind.Kind, Version: kind.APIVersion},
		IntegratedTime:    rb.Payload.IntegratedTime,
		InclusionPromise:  &protorekor.InclusionPromise{SignedEntryTimestamp: set},
		CanonicalizedBody: body,
	}, nil
}

func verifyImageBinding(payload []byte, image string) error {
	_, digestHex, err := splitDigest(image)
	if err != nil {
		return err
	}
	var ss struct {
		Critical struct {
			Image struct {
				DockerManifestDigest string `json:"docker-manifest-digest"`
			} `json:"image"`
		} `json:"critical"`
	}
	if err := json.Unmarshal(payload, &ss); err != nil {
		return fmt.Errorf("failed to parse signed payload for %s: %w", image, err)
	}
	if want := "sha256:" + digestHex; ss.Critical.Image.DockerManifestDigest != want {
		return fmt.Errorf("signature payload references %s, not %s",
			ss.Critical.Image.DockerManifestDigest, want)
	}
	return nil
}

func splitDigest(image string) (repo, digestHex string, err error) {
	repo, digestHex, ok := strings.Cut(image, "@sha256:")
	if !ok {
		return "", "", fmt.Errorf("image reference %s is not sha256 digest-pinned", image)
	}
	return repo, digestHex, nil
}
