package git

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/entigolabs/entigo-infralib-agent/model"
	"github.com/entigolabs/entigo-infralib-agent/util"
	"github.com/go-git/go-billy/v5"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/plumbing/transport/http"
	"github.com/hashicorp/go-version"
)

var repoMutex = sync.Mutex{}

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
	CABundle       []byte
}

func NewSourceClient(ctx context.Context, source model.ConfigSource, CABundle []byte) (*SourceClient, error) {
	log.Printf("Initializing repository for %s", source.GetSourceKey())
	auth := getSourceAuth(source)
	repo, err := getSourceRepo(ctx, auth, source, CABundle)
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
		CABundle:    CABundle,
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
	if len(invalidTags) > 0 {
		slog.Debug(fmt.Sprintf("Tags are not a valid semversion: %s", strings.Join(invalidTags.ToSlice(), ", ")))
	}
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

func getSourceRepo(ctx context.Context, auth transport.AuthMethod, source model.ConfigSource, CABundle []byte) (*git.Repository, error) {
	repoPath, err := getRepoPath(source)
	if err != nil {
		return nil, err
	}
	repoMutex.Lock()
	defer repoMutex.Unlock()
	repo, err := openSourceRepo(ctx, auth, source, repoPath, CABundle)
	if err == nil {
		slog.Debug(fmt.Sprintf("Repository path %s", repoPath))
		return repo, nil
	}
	if !errors.Is(err, git.ErrRepositoryNotExists) {
		return nil, err
	}
	slog.Debug(fmt.Sprintf("Cloning repository to %s", repoPath))
	return git.PlainCloneContext(ctx, repoPath, false, &git.CloneOptions{
		URL:             source.URL,
		Auth:            auth,
		InsecureSkipTLS: source.Insecure,
		CABundle:        CABundle,
	})
}

func getRepoPath(source model.ConfigSource) (string, error) {
	if source.RepoPath != "" {
		return source.RepoPath, nil
	}
	return filepath.Join(os.TempDir(), util.HashCode(source.GetSourceKey().String())), nil
}

func openSourceRepo(ctx context.Context, auth transport.AuthMethod, source model.ConfigSource, path string, CABundle []byte) (*git.Repository, error) {
	repo, err := git.PlainOpen(path)
	if err != nil {
		return nil, err
	}
	err = repo.FetchContext(ctx, &git.FetchOptions{
		Auth:            auth,
		Prune:           true,
		Tags:            git.AllTags,
		InsecureSkipTLS: source.Insecure,
		CABundle:        CABundle,
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
	ref, err := getDefaultBranchRef(repo)
	if err != nil {
		return nil, err
	}
	err = worktree.Reset(&git.ResetOptions{
		Commit: ref.Hash(),
		Mode:   git.HardReset,
	})
	if err != nil {
		return nil, err
	}
	return repo, nil
}

func getDefaultBranchRef(repo *git.Repository) (*plumbing.Reference, error) {
	ref, err := repo.Reference(plumbing.NewRemoteReferenceName(git.DefaultRemoteName, "HEAD"), true)
	if err == nil {
		return ref, nil
	}
	ref, err = repo.Reference(plumbing.NewRemoteReferenceName(git.DefaultRemoteName, "main"), true)
	if err == nil {
		return ref, nil
	}
	ref, err = repo.Reference(plumbing.NewRemoteReferenceName(git.DefaultRemoteName, "master"), true)
	if err == nil {
		return ref, nil
	}
	return nil, fmt.Errorf("failed to find default branch (HEAD, main, or master): %w", err)
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
	var checkoutRef plumbing.ReferenceName
	isTag := s.releasesSet.Contains(release)
	if isTag {
		checkoutRef = plumbing.NewTagReferenceName(release)
	} else {
		if !s.pulled.Contains(release) {
			if err := s.fetchBranch(release); err != nil {
				return err
			}
		}
		checkoutRef = plumbing.NewRemoteReferenceName(git.DefaultRemoteName, release)
	}
	err := s.worktree.Checkout(&git.CheckoutOptions{
		Branch: checkoutRef,
		Force:  true,
	})
	if err != nil {
		return err
	}
	s.currentRelease = release
	return nil
}

func (s *SourceClient) fetchBranch(release string) error {
	err := s.repo.Fetch(&git.FetchOptions{
		RefSpecs: []config.RefSpec{
			config.RefSpec(fmt.Sprintf("+refs/heads/%[1]s:refs/remotes/origin/%[1]s", release)),
		},
		Auth:            s.auth,
		InsecureSkipTLS: s.insecure,
		CABundle:        s.CABundle,
	})
	if err != nil && !errors.Is(err, git.NoErrAlreadyUpToDate) {
		return fmt.Errorf("failed to fetch branch %s: %w", release, err)
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

func (s *SourceClient) PathExists(path string, release string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	err := s.checkoutClean(release)
	if err != nil {
		return false, err
	}

	file, err := s.worktree.Filesystem.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	return file.IsDir(), nil
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
		lowered := strings.ToLower(info.Name())
		if strings.HasPrefix(lowered, "test") || strings.HasPrefix(lowered, "readme") || strings.HasPrefix(lowered, "go.") {
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
