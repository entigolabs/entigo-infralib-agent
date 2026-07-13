package oci

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/entigolabs/entigo-infralib-agent/model"
	"github.com/entigolabs/entigo-infralib-agent/util"
	"github.com/hashicorp/go-version"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/retry"
)

// indexPackage is the name of the OCI package that holds the release index
// (the list of available versions and the manifest describing every module).
const indexPackage = "index"

var _ model.SourceRepository = (*SourceClient)(nil)
var _ model.OCIImageResolver = (*SourceClient)(nil)

// SourceClient serves infralib releases published as OCI artifacts. It mirrors
// git.SourceClient: it enumerates releases (tags of the "index" package),
// downloads the requested version and extracts its zip so the extracted tree
// (modules/, providers/, manifest.json, ...) can be served through the
// model.Storage interface exactly like a git worktree.
type SourceClient struct {
	ctx         context.Context
	url         string
	repo        *remote.Repository
	releases    []*version.Version
	releasesSet model.Set[string]
	basePath    string
	extracted   map[string]string
	mu          sync.Mutex
}

func NewSourceClient(ctx context.Context, source model.ConfigSource, CABundle []byte) (*SourceClient, error) {
	log.Printf("Initializing OCI source for %s", source.GetSourceKey())
	repoRef := fmt.Sprintf("%s/%s", util.TrimOCIScheme(source.URL), indexPackage)
	repo, err := remote.NewRepository(repoRef)
	if err != nil {
		return nil, fmt.Errorf("failed to parse OCI reference %s: %w", repoRef, err)
	}
	httpClient, err := newHTTPClient(source.Insecure, CABundle)
	if err != nil {
		return nil, err
	}
	authClient := &auth.Client{
		Client: httpClient,
		Cache:  auth.NewCache(),
	}
	if source.Username != "" {
		authClient.Credential = auth.StaticCredential(repo.Reference.Registry, auth.Credential{
			Username: source.Username,
			Password: source.Password,
		})
	}
	repo.Client = authClient

	releases, releasesSet, err := getReleases(ctx, repo)
	if err != nil {
		return nil, fmt.Errorf("failed to list OCI index tags for %s: %w", repoRef, err)
	}
	if source.ForceVersion && source.Version != "" {
		if err := validateForcedVersion(ctx, repo, releasesSet, source.Version); err != nil {
			return nil, err
		}
	}
	basePath := source.RepoPath
	if basePath == "" {
		basePath = filepath.Join(os.TempDir(), "infralib-oci-"+util.HashCode(source.GetSourceKey().String()))
	}
	return &SourceClient{
		ctx:         ctx,
		url:         source.URL,
		repo:        repo,
		releases:    releases,
		releasesSet: releasesSet,
		basePath:    basePath,
		extracted:   make(map[string]string),
	}, nil
}

// validateForcedVersion fails fast when a pinned version cannot be found,
// rather than deep inside the first file access.
func validateForcedVersion(ctx context.Context, repo *remote.Repository, releasesSet model.Set[string], forcedVersion string) error {
	if util.IsOCIDigest(forcedVersion) {
		if _, err := repo.Resolve(ctx, forcedVersion); err != nil {
			return fmt.Errorf("forced version %s not found in OCI source: %w", forcedVersion, err)
		}
		return nil
	}
	if !releasesSet.Contains(util.NormalizeOCIVersion(forcedVersion)) {
		return fmt.Errorf("release %s not found in OCI source", forcedVersion)
	}
	return nil
}

// canonicalOCIVersion converts a raw OCI tag into the agent's canonical,
// v-prefixed form so the service layer can compare it against git-sourced and
// state versions uniformly. Digests, already-prefixed tags, and non-semver tags
// (e.g. branch names) pass through untouched; util.NormalizeOCIVersion strips
// the "v" again at the OCI wire boundary.
func canonicalOCIVersion(tag string) string {
	if util.IsOCIDigest(tag) || strings.HasPrefix(tag, "v") {
		return tag
	}
	if _, err := version.NewVersion(tag); err != nil {
		return tag
	}
	return "v" + tag
}

func getReleases(ctx context.Context, repo *remote.Repository) ([]*version.Version, model.Set[string], error) {
	var releases []*version.Version
	releaseSet := model.NewSet[string]()
	err := repo.Tags(ctx, "", func(tags []string) error {
		for _, tag := range tags {
			releaseSet.Add(tag)
			tagVersion, err := version.NewVersion(canonicalOCIVersion(tag))
			if err != nil {
				continue
			}
			releases = append(releases, tagVersion)
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	sort.Sort(version.Collection(releases))
	return releases, releaseSet, nil
}

// GetImageReference resolves the release to the digest-pinned index image
// reference used for signature verification (registry/path/index@sha256:...).
func (s *SourceClient) GetImageReference(release string) (string, error) {
	desc, err := s.repo.Resolve(s.ctx, util.NormalizeOCIVersion(release))
	if err != nil {
		return "", fmt.Errorf("failed to resolve OCI index %s: %w", release, err)
	}
	return fmt.Sprintf("%s/%s@%s", util.TrimOCIScheme(s.url), indexPackage, desc.Digest.String()), nil
}

func (s *SourceClient) GetLatestReleaseTag() (*version.Version, error) {
	if len(s.releases) == 0 {
		return nil, fmt.Errorf("no releases found")
	}
	return s.releases[len(s.releases)-1], nil
}

func (s *SourceClient) GetRelease(release string) (*version.Version, error) {
	normalized := util.NormalizeOCIVersion(release)
	if !s.releasesSet.Contains(normalized) {
		return nil, fmt.Errorf("release %s not found", release)
	}
	return version.NewVersion(canonicalOCIVersion(normalized))
}

func (s *SourceClient) GetReleases(oldestRelease, newestRelease *version.Version) ([]*version.Version, error) {
	var newReleases []*version.Version
	for _, release := range s.releases {
		if release.LessThan(oldestRelease) {
			continue
		}
		if newestRelease != nil && release.GreaterThan(newestRelease) {
			break
		}
		newReleases = append(newReleases, release)
	}
	return newReleases, nil
}

func (s *SourceClient) GetFile(path, release string) ([]byte, error) {
	dir, err := s.ensureExtracted(release)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(filepath.Join(dir, path))
	if errors.Is(err, os.ErrNotExist) {
		return nil, model.NewNotFoundError(path)
	}
	return data, err
}

func (s *SourceClient) FileExists(path, release string) bool {
	dir, err := s.ensureExtracted(release)
	if err != nil {
		return false
	}
	info, err := os.Stat(filepath.Join(dir, path))
	return err == nil && !info.IsDir()
}

func (s *SourceClient) PathExists(path, release string) (bool, error) {
	dir, err := s.ensureExtracted(release)
	if err != nil {
		return false, err
	}
	info, err := os.Stat(filepath.Join(dir, path))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return info.IsDir(), nil
}

func (s *SourceClient) CalculateChecksums(release string) (map[string][]byte, error) {
	dir, err := s.ensureExtracted(release)
	if err != nil {
		return nil, err
	}
	checksums := make(map[string][]byte)
	if err := generateModulesChecksums(dir, checksums); err != nil {
		return nil, err
	}
	if err := generateProvidersChecksums(dir, checksums); err != nil {
		return nil, err
	}
	return checksums, nil
}

// ensureExtracted downloads the index artifact for the release (a tag or a
// sha256 digest) and unzips it, caching the extraction directory per release.
func (s *SourceClient) ensureExtracted(release string) (string, error) {
	release = util.NormalizeOCIVersion(release)
	s.mu.Lock()
	defer s.mu.Unlock()
	if dir, ok := s.extracted[release]; ok {
		return dir, nil
	}
	// Always re-pull: a tag can be re-published with new content, so reusing a
	// stale on-disk extraction would silently provision the old module tree. The
	// in-memory cache above dedupes within a single run.
	dir := filepath.Join(s.basePath, util.HashCode(release))
	zipData, err := s.pullIndexZip(release)
	if err != nil {
		return "", err
	}
	if err := os.RemoveAll(dir); err != nil {
		return "", fmt.Errorf("failed to clean OCI extraction dir %s: %w", dir, err)
	}
	if err := extractZip(zipData, dir); err != nil {
		return "", fmt.Errorf("failed to extract OCI index %s: %w", release, err)
	}
	s.extracted[release] = dir
	return dir, nil
}

func (s *SourceClient) pullIndexZip(release string) ([]byte, error) {
	desc, err := s.repo.Resolve(s.ctx, release)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve OCI index %s: %w", release, err)
	}
	manifestData, err := content.FetchAll(s.ctx, s.repo, desc)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch OCI index manifest %s: %w", release, err)
	}
	var manifest ocispec.Manifest
	if err := json.Unmarshal(manifestData, &manifest); err != nil {
		return nil, fmt.Errorf("failed to parse OCI index manifest %s: %w", release, err)
	}
	layer, ok := selectZipLayer(manifest.Layers)
	if !ok {
		// An empty layer set usually means the tag resolved to an image index
		// (manifest list) rather than a single artifact manifest.
		return nil, fmt.Errorf("no zip layer found in OCI index %s (manifest media type %q)", release, desc.MediaType)
	}
	return content.FetchAll(s.ctx, s.repo, layer)
}

func selectZipLayer(layers []ocispec.Descriptor) (ocispec.Descriptor, bool) {
	for _, layer := range layers {
		if strings.HasSuffix(layer.Annotations[ocispec.AnnotationTitle], ".zip") {
			return layer, true
		}
	}
	return ocispec.Descriptor{}, false
}

func extractZip(data []byte, dir string) error {
	reader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return err
	}
	cleanDir := filepath.Clean(dir)
	for _, f := range reader.File {
		target := filepath.Join(cleanDir, f.Name)
		// Guard against zip-slip: the extracted path must stay within dir.
		if target != cleanDir && !strings.HasPrefix(target, cleanDir+string(os.PathSeparator)) {
			return fmt.Errorf("invalid file path in zip: %s", f.Name)
		}
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := writeZipEntry(f, target); err != nil {
			return err
		}
	}
	return nil
}

func writeZipEntry(f *zip.File, target string) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	src, err := f.Open()
	if err != nil {
		return err
	}
	defer func() { _ = src.Close() }()
	dst, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, f.Mode())
	if err != nil {
		return err
	}
	defer func() { _ = dst.Close() }()
	_, err = io.Copy(dst, src)
	return err
}

func generateModulesChecksums(root string, checksums map[string][]byte) error {
	exists, err := directoryExists(filepath.Join(root, "modules"))
	if !exists || err != nil {
		return err
	}
	parents, err := os.ReadDir(filepath.Join(root, "modules"))
	if err != nil {
		return err
	}
	for _, parent := range parents {
		if !parent.IsDir() {
			continue
		}
		if err := checksumModuleParent(root, parent.Name(), checksums); err != nil {
			return err
		}
	}
	return nil
}

func checksumModuleParent(root, parentName string, checksums map[string][]byte) error {
	parentPath := filepath.Join("modules", parentName)
	modules, err := os.ReadDir(filepath.Join(root, parentPath))
	if err != nil {
		return err
	}
	for _, module := range modules {
		if !module.IsDir() {
			continue
		}
		fullPath := filepath.Join(parentPath, module.Name())
		sum, err := directoryChecksum(filepath.Join(root, fullPath))
		if err != nil {
			return err
		}
		checksums[fullPath] = sum
	}
	return nil
}

func generateProvidersChecksums(root string, checksums map[string][]byte) error {
	exists, err := directoryExists(filepath.Join(root, "providers"))
	if err != nil || !exists {
		return err
	}
	infos, err := os.ReadDir(filepath.Join(root, "providers"))
	if err != nil {
		return err
	}
	for _, info := range infos {
		if info.IsDir() {
			continue
		}
		if strings.HasPrefix(info.Name(), "go.") || strings.HasPrefix(info.Name(), "README.") || strings.HasPrefix(info.Name(), "test") {
			continue
		}
		fullPath := filepath.Join("providers", info.Name())
		sum, err := fileChecksum(filepath.Join(root, fullPath))
		if err != nil {
			return err
		}
		checksums[fullPath] = sum
	}
	return nil
}

func directoryExists(path string) (bool, error) {
	stat, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	if !stat.IsDir() {
		return false, fmt.Errorf("%s is not a directory", path)
	}
	return true, nil
}

func directoryChecksum(dir string) ([]byte, error) {
	var keys []string
	sums := make(map[string][]byte)
	infos, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, info := range infos {
		lowered := strings.ToLower(info.Name())
		if strings.HasPrefix(lowered, "test") || strings.HasPrefix(lowered, "readme") || strings.HasPrefix(lowered, "go.") {
			continue
		}
		var sum []byte
		if info.IsDir() {
			sum, err = directoryChecksum(filepath.Join(dir, info.Name()))
		} else {
			sum, err = fileChecksum(filepath.Join(dir, info.Name()))
		}
		if err != nil {
			return nil, err
		}
		keys = append(keys, info.Name())
		sums[info.Name()] = sum
	}

	h := sha256.New()
	sort.Strings(keys)
	for _, key := range keys {
		if _, err = h.Write(sums[key]); err != nil {
			return nil, err
		}
	}
	return h.Sum(nil), nil
}

func fileChecksum(file string) ([]byte, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	h := sha256.New()
	if _, err = io.Copy(h, f); err != nil {
		return nil, err
	}
	return h.Sum(nil), nil
}

func newHTTPClient(insecure bool, CABundle []byte) (*http.Client, error) {
	if !insecure && len(CABundle) == 0 {
		return retry.DefaultClient, nil
	}
	tlsConfig := &tls.Config{InsecureSkipVerify: insecure}
	if len(CABundle) > 0 {
		pool, err := x509.SystemCertPool()
		if err != nil || pool == nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(CABundle) {
			return nil, fmt.Errorf("failed to append CA bundle to cert pool")
		}
		tlsConfig.RootCAs = pool
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = tlsConfig
	return &http.Client{Transport: retry.NewTransport(transport)}, nil
}
