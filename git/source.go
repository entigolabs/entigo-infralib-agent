package git

import (
	"context"
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
	err = tagRefs.ForEach(func(t *plumbing.Reference) error {
		if !t.Name().IsTag() {
			return nil
		}
		tag := t.Name().Short()
		releaseSet.Add(tag)
		tagVersion, err := version.NewVersion(tag)
		if err != nil {
			slog.Debug(fmt.Sprintf("Tag '%s' is not a valid semversion", tag))
			return nil
		}
		releases = append(releases, tagVersion)
		return nil
	})
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
	if s.releasesSet.Contains(release) {
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
	if s.pulled.Contains(release) {
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
