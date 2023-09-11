package service

import (
	"context"
	"fmt"
	"github.com/entigolabs/entigo-infralib-agent/common"
	"github.com/google/go-github/v54/github"
	"net/url"
	"sort"
	"strings"
	"time"
)

type Github interface {
	GetLatestReleaseTag() (*Release, error)
	GetReleaseByTag(tag string) (*Release, error)
	GetNewerReleases(after time.Time, until time.Time) ([]Release, error)
	GetRawFileContent(path string, release string) (string, error)
}

type githubClient struct {
	client *github.Client
	owner  string
	repo   string
}

func NewGithub(repoURL string) Github {
	owner, repo, err := getGithubOwnerAndRepo(repoURL)
	if err != nil {
		common.Logger.Fatalf("Failed to get GitHub owner and repo from url: %s; error: %s", repoURL, err)
	}
	return &githubClient{
		client: github.NewClient(nil),
		owner:  owner,
		repo:   repo,
	}
}

func (g *githubClient) GetLatestReleaseTag() (*Release, error) {
	release, _, err := g.client.Repositories.GetLatestRelease(context.Background(), g.owner, g.repo)
	if err != nil {
		return nil, err
	}
	if release == nil {
		return nil, fmt.Errorf("no releases found for %s/%s", g.owner, g.repo)
	}
	return &Release{
		Tag:         release.GetTagName(),
		PublishedAt: release.PublishedAt.Time,
	}, nil
}

func (g *githubClient) GetReleaseByTag(tag string) (*Release, error) {
	release, _, err := g.client.Repositories.GetReleaseByTag(context.Background(), g.owner, g.repo, tag)
	if err != nil {
		return nil, err
	}
	if release == nil {
		return nil, fmt.Errorf("no release found for %s/%s with tag %s", g.owner, g.repo, tag)
	}
	return &Release{
		Tag:         release.GetTagName(),
		PublishedAt: release.PublishedAt.Time,
	}, nil
}

func (g *githubClient) GetNewerReleases(after time.Time, until time.Time) ([]Release, error) {
	newReleases := make([]Release, 0)
	options := &github.ListOptions{Page: 1}
	for {
		releases, response, err := g.client.Repositories.ListReleases(context.Background(), g.owner, g.repo, options)
		if err != nil {
			return nil, err
		}
		for _, release := range releases {
			if release.GetPublishedAt().After(until) {
				continue
			}
			if !release.GetPublishedAt().Before(after) {
				newReleases = append(newReleases, Release{
					Tag:         release.GetTagName(),
					PublishedAt: release.GetPublishedAt().Time,
				})
			} else {
				return sortReleasesByPublishedAt(newReleases), nil
			}
		}
		if response.NextPage == 0 {
			return sortReleasesByPublishedAt(newReleases), nil
		}
		options.Page = response.NextPage
	}
}

func (g *githubClient) GetRawFileContent(path string, release string) (string, error) {
	common.Logger.Printf("Getting raw file content %s/%s/%s with reference %s\n", g.owner, g.repo, path, release)
	fileContent, _, _, err := g.client.Repositories.GetContents(context.Background(), g.owner, g.repo, path,
		&github.RepositoryContentGetOptions{
			Ref: release,
		})
	if err != nil {
		return "", err
	}
	if fileContent == nil {
		return "", fmt.Errorf("no file found for %s/%s with path %s", g.owner, g.repo, path)
	}
	content, err := fileContent.GetContent()
	if err != nil {
		return "", err
	}
	return content, nil
}

func sortReleasesByPublishedAt(releases []Release) []Release {
	sort.Slice(releases, func(i, j int) bool {
		return releases[i].PublishedAt.Before(releases[j].PublishedAt)
	})
	return releases
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
