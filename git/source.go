package git

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"github.com/entigolabs/entigo-infralib-agent/model"
	"github.com/go-git/go-billy/v5"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/plumbing/transport/http"
	"github.com/hashicorp/go-version"
	"io"
	"log"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
)

var invalidChars = regexp.MustCompile(`[\\/:*?"<>|.]`)

type SourceClient struct {
	ctx            context.Context
	auth           transport.AuthMethod
	insecure       bool
	repo           *git.Repository
	worktree       *git.Worktree
	releases       []*version.Version
	releasesSet    model.Set[string]
	mu             sync.Mutex
	currentRelease string
	pulled         model.Set[string]
}

func NewSourceClient(ctx context.Context, source model.ConfigSource) (*SourceClient, error) {
	log.Printf("Initializing repository for %s", source.URL)
	auth := getSourceAuth(source)
	repo, err := getSourceRepo(ctx, auth, source)
	if err != nil {
		return nil, err
	}
	worktree, err := repo.Worktree()
	if err != nil {
		return nil, err
	}
	releases, releasesSet, err := getReleases(repo)
	if err != nil {
		return nil, fmt.Errorf("failed to get tags: %v", err)
	}
	return &SourceClient{
		ctx:         ctx,
		auth:        auth,
		insecure:    source.Insecure,
		repo:        repo,
		worktree:    worktree,
		releases:    releases,
		releasesSet: releasesSet,
		pulled:      model.NewSet[string](),
	}, nil
}

func getReleases(repo *git.Repository) ([]*version.Version, model.Set[string], error) {
	tagRefs, err := repo.Tags()
	if err != nil {
		return nil, nil, err
	}
	var releases []*version.Version
	var releaseSet = model.NewSet[string]()
	var invalidTags = model.NewSet[string]()
	err = tagRefs.ForEach(func(t *plumbing.Reference) error {
		if !t.Name().IsTag() {
			return nil
		}
		tag := t.Name().Short()
		releaseSet.Add(tag)
		tagVersion, err := version.NewVersion(tag)
		if err != nil {
			invalidTags.Add(tag)
			return nil
		}
		releases = append(releases, tagVersion)
		return nil
	})
	slog.Debug(fmt.Sprintf("Tags are not a valid semversion: %s", strings.Join(invalidTags.ToSlice(), ", ")))
	sort.Sort(version.Collection(releases))
	return releases, releaseSet, err
}

func getSourceAuth(source model.ConfigSource) transport.AuthMethod {
	if source.Username == "" {
		return nil
	}
	return &http.BasicAuth{
		Username: source.Username,
		Password: source.Password,
	}
}

func getSourceRepo(ctx context.Context, auth transport.AuthMethod, source model.ConfigSource) (*git.Repository, error) {
	repoPath, err := getRepoPath(source)
	if err != nil {
		return nil, err
	}
	slog.Debug(fmt.Sprintf("Cloning repository to %s", repoPath))
	repo, err := openSourceRepo(ctx, auth, source, repoPath)
	if err == nil {
		return repo, nil
	}
	if !errors.Is(err, git.ErrRepositoryNotExists) {
		return nil, err
	}
	return git.PlainCloneContext(ctx, repoPath, false, &git.CloneOptions{
		URL:             source.URL,
		Auth:            auth,
		InsecureSkipTLS: source.Insecure,
	})
}

func getRepoPath(source model.ConfigSource) (string, error) {
	if source.RepoPath != "" {
		return source.RepoPath, nil
	}
	parsedURL, err := url.Parse(source.URL)
	if err != nil {
		return "", err
	}
	host := parsedURL.Host
	path := strings.Trim(parsedURL.Path, "/")
	path = strings.TrimSuffix(path, ".git")
	fullPath := host + "-" + path
	fullPath = invalidChars.ReplaceAllString(fullPath, "-")
	return filepath.Join(os.TempDir(), fullPath), nil
}

func openSourceRepo(ctx context.Context, auth transport.AuthMethod, source model.ConfigSource, path string) (*git.Repository, error) {
	repo, err := git.PlainOpen(path)
	if err != nil {
		return nil, err
	}
	err = repo.FetchContext(ctx, &git.FetchOptions{
		Auth:            auth,
		Prune:           true,
		Tags:            git.AllTags,
		InsecureSkipTLS: source.Insecure,
	})
	if errors.Is(err, git.NoErrAlreadyUpToDate) {
		return repo, nil
	}
	if err != nil {
		return nil, err
	}
	worktree, err := repo.Worktree()
	if err != nil {
		return nil, err
	}
	err = worktree.PullContext(ctx, &git.PullOptions{
		Force:           true,
		Auth:            auth,
		InsecureSkipTLS: source.Insecure,
	})
	if err != nil && !errors.Is(err, git.NoErrAlreadyUpToDate) {
		return nil, err
	}
	return repo, nil
}

func (s *SourceClient) GetLatestReleaseTag() (*version.Version, error) {
	if len(s.releases) == 0 {
		return nil, fmt.Errorf("no releases found")
	}
	return s.releases[len(s.releases)-1], nil
}

func (s *SourceClient) GetRelease(release string) (*version.Version, error) {
	if !s.releasesSet.Contains(release) {
		return nil, fmt.Errorf("release %s not found", release)
	}
	return version.NewVersion(release)
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

func (s *SourceClient) GetFile(path string, release string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	err := s.checkoutClean(release)
	if err != nil {
		return nil, err
	}

	file, err := s.worktree.Filesystem.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, model.NewFileNotFoundError(path)
	}
	if err != nil {
		return nil, err
	}
	defer func(file billy.File) {
		_ = file.Close()
	}(file)

	return io.ReadAll(file)
}

func (s *SourceClient) checkoutClean(release string) error {
	if s.currentRelease == release {
		return nil
	}
	var branch plumbing.ReferenceName
	isTag := s.releasesSet.Contains(release)
	if isTag {
		branch = plumbing.NewTagReferenceName(release)
	} else {
		branch = plumbing.NewBranchReferenceName(release)
	}
	err := s.worktree.Checkout(&git.CheckoutOptions{
		Branch: branch,
		Force:  true,
	})
	if err != nil {
		return err
	}
	s.currentRelease = release
	if isTag || s.pulled.Contains(release) {
		return nil
	}
	err = s.worktree.PullContext(s.ctx, &git.PullOptions{
		Force:           true,
		SingleBranch:    true,
		Auth:            s.auth,
		InsecureSkipTLS: s.insecure,
	})
	if err != nil && !errors.Is(err, git.NoErrAlreadyUpToDate) {
		return err
	}
	s.pulled.Add(release)
	return nil
}

func (s *SourceClient) FileExists(path string, release string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	err := s.checkoutClean(release)
	if err != nil {
		return false
	}

	file, err := s.worktree.Filesystem.Stat(path)
	return err == nil && !file.IsDir()
}

func (s *SourceClient) PathExists(path string, release string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	err := s.checkoutClean(release)
	if err != nil {
		return false
	}

	file, err := s.worktree.Filesystem.Stat(path)
	return err == nil && file.IsDir()
}

func (s *SourceClient) CalculateChecksums(release string) (map[string][]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	err := s.checkoutClean(release)
	if err != nil {
		return nil, err
	}

	checksums := make(map[string][]byte)
	err = s.generateModulesChecksums(checksums)
	if err != nil {
		return nil, err
	}
	err = s.generateProvidersChecksums(checksums)
	if err != nil {
		return nil, err
	}
	return checksums, nil
}

func (s *SourceClient) generateModulesChecksums(checksums map[string][]byte) error {
	exists, err := s.directoryExists("modules")
	if !exists || err != nil {
		return err
	}
	parents, err := s.worktree.Filesystem.ReadDir("modules")
	if err != nil {
		return err
	}
	for _, parent := range parents {
		if !parent.IsDir() {
			continue
		}
		parentPath := filepath.Join("modules", parent.Name())
		modules, err := s.worktree.Filesystem.ReadDir(parentPath)
		if err != nil {
			return err
		}
		for _, module := range modules {
			if !module.IsDir() {
				continue
			}
			fullPath := filepath.Join(parentPath, module.Name())
			sum, err := s.directoryChecksum(fullPath)
			if err != nil {
				return err
			}
			checksums[fullPath] = sum
		}
	}
	return err
}

func (s *SourceClient) generateProvidersChecksums(checksums map[string][]byte) error {
	exists, err := s.directoryExists("providers")
	if err != nil || !exists {
		return err
	}
	infos, err := s.worktree.Filesystem.ReadDir("providers")
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
		sum, err := fileChecksum(s.worktree, fullPath)
		if err != nil {
			return err
		}
		checksums[fullPath] = sum
	}
	return err
}

func (s *SourceClient) directoryExists(path string) (bool, error) {
	stat, err := s.worktree.Filesystem.Stat(path)
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

func (s *SourceClient) directoryChecksum(dir string) ([]byte, error) {
	var keys []string
	sums := make(map[string][]byte)
	infos, err := s.worktree.Filesystem.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, info := range infos {
		if info.IsDir() {
			continue
		}
		if strings.HasPrefix(info.Name(), "test") {
			continue
		}
		sum, err := fileChecksum(s.worktree, filepath.Join(dir, info.Name()))
		if err != nil {
			return nil, err
		}
		keys = append(keys, info.Name())
		sums[info.Name()] = sum
	}

	h := sha256.New()
	sort.Strings(keys)
	for _, key := range keys {
		_, err = h.Write(sums[key])
		if err != nil {
			return nil, err
		}
	}
	return h.Sum(nil), nil
}

func fileChecksum(worktree *git.Worktree, file string) ([]byte, error) {
	f, err := worktree.Filesystem.Open(file)
	if err != nil {
		return nil, err
	}
	defer func(f billy.File) {
		_ = f.Close()
	}(f)

	h := sha256.New()
	if _, err = io.Copy(h, f); err != nil {
		return nil, err
	}
	return h.Sum(nil), nil
}
