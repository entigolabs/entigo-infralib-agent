package github

import (
	"context"
	"fmt"
	"github.com/entigolabs/entigo-infralib-agent/model"
	"github.com/google/go-github/v60/github"
	"github.com/hashicorp/go-version"
	"log"
	"net/url"
	"sort"
	"strings"
	"time"
)

type Github interface {
	GetLatestReleaseTag(repoURL string) (*Release, error)
	GetReleaseByTag(repoURL string, tag string) (*Release, error)
	GetReleases(repoURL string, oldestRelease Release, newestRelease *Release) ([]*version.Version, error)
	GetRawFileContent(repoURL string, path string, release string) ([]byte, error)
}

const rawGithubUrl = "https://raw.githubusercontent.com"

type githubClient struct {
	ctx    context.Context
	client *github.Client
	cache  *FileCache
}

func NewGithub(ctx context.Context, token string) Github {
	client := github.NewClient(nil)
	if token != "" {
		client.WithAuthToken(token)
	}
	return &githubClient{
		ctx:    ctx,
		client: client,
		cache:  NewFileCache(),
	}
}

func (g *githubClient) GetLatestReleaseTag(repoURL string) (*Release, error) {
	owner, repo, err := getGithubOwnerAndRepo(repoURL)
	if err != nil {
		log.Fatalf("Failed to get GitHub owner and repo from url: %s; error: %s", repoURL, err)
	}
	release, _, err := g.client.Repositories.GetLatestRelease(g.ctx, owner, repo)
	if err != nil {
		return nil, fmt.Errorf("failed to get latest release for %s/%s: %w", owner, repo, err)
	}
	if release == nil {
		return nil, fmt.Errorf("no releases found for %s/%s", owner, repo)
	}
	return &Release{
		Tag:         release.GetTagName(),
		PublishedAt: release.PublishedAt.Time,
	}, nil
}

func (g *githubClient) GetReleaseByTag(repoURL string, tag string) (*Release, error) {
	owner, repo, err := getGithubOwnerAndRepo(repoURL)
	if err != nil {
		log.Fatalf("Failed to get GitHub owner and repo from url: %s; error: %s", repoURL, err)
	}
	release, _, err := g.client.Repositories.GetReleaseByTag(g.ctx, owner, repo, tag)
	if err != nil {
		return nil, err
	}
	if release == nil {
		return nil, fmt.Errorf("no release found for %s/%s with tag %s", owner, repo, tag)
	}
	return &Release{
		Tag:         release.GetTagName(),
		PublishedAt: release.PublishedAt.Time,
	}, nil
}

func (g *githubClient) GetReleases(repoURL string, oldestRelease Release, newestRelease *Release) ([]*version.Version, error) {
	owner, repo, err := getGithubOwnerAndRepo(repoURL)
	if err != nil {
		log.Fatalf("Failed to get GitHub owner and repo from url: %s; error: %s", repoURL, err)
	}

	oldestVersion, err := version.NewVersion(oldestRelease.Tag)
	if err != nil {
		return nil, err
	}
	var newestVersion *version.Version
	if newestRelease != nil {
		newestVersion, err = version.NewVersion(newestRelease.Tag)
		if err != nil {
			return nil, err
		}
	}

	return g.getReleases(owner, repo, oldestRelease, oldestVersion, newestRelease, newestVersion)
}

func (g *githubClient) getReleases(owner, repo string, oldestRelease Release, oldestVersion *version.Version, newestRelease *Release, newestVersion *version.Version) ([]*version.Version, error) {
	newReleases := make([]*version.Version, 0)
	options := &github.ListOptions{Page: 1}
	for {
		releases, response, err := g.client.Repositories.ListReleases(g.ctx, owner, repo, options)
		if err != nil {
			return nil, err
		}
		for _, release := range releases {
			if newestRelease != nil && release.GetPublishedAt().After(newestRelease.PublishedAt) {
				continue
			}
			if !release.GetPublishedAt().Before(oldestRelease.PublishedAt) {
				newVersion, err := version.NewVersion(release.GetTagName())
				if err != nil {
					return nil, fmt.Errorf("failed to parse version %s in %s/%s: %v", release.GetTagName(),
						owner, repo, err)
				}
				if newVersion.LessThan(oldestVersion) || (newestVersion != nil && newVersion.GreaterThan(newestVersion)) {
					continue
				}
				newReleases = append(newReleases, newVersion)
			} else {
				sort.Sort(version.Collection(newReleases))
				return newReleases, nil
			}
		}
		if response.NextPage == 0 {
			sort.Sort(version.Collection(newReleases))
			return newReleases, nil
		}
		options.Page = response.NextPage
	}
}

func (g *githubClient) GetRawFileContent(repoURL string, path string, release string) ([]byte, error) {
	owner, repo, err := getGithubOwnerAndRepo(repoURL)
	if err != nil {
		log.Fatalf("Failed to get GitHub owner and repo from url: %s; error: %s", repoURL, err)
	}
	fileUrl := fmt.Sprintf("%s/%s/%s/%s/%s", rawGithubUrl, owner, repo, release, path)
	fileContent, err := g.cache.GetFile(fileUrl)
	if err != nil {
		return nil, err
	}
	if fileContent == nil {
		return nil, model.NewFileNotFoundError(path)
	}
	return fileContent, nil
}

func getGithubOwnerAndRepo(repoURL string) (string, string, error) {
	u, err := url.Parse(repoURL)
	if err != nil {
		return "", "", err
	}
	if u.Hostname() != "github.com" {
		return "", "", fmt.Errorf("not a GitHub URL: %s", repoURL)
	}

	parts := strings.Split(strings.TrimPrefix(u.Path, "/"), "/")
	if len(parts) != 2 {
		return "", "", fmt.Errorf("invalid GitHub URL: %s", repoURL)
	}

	return parts[0], parts[1], nil
}

type Release struct {
	Tag         string
	PublishedAt time.Time
}
