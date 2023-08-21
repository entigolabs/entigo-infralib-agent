package service

import (
	"context"
	"fmt"
	"github.com/google/go-github/v54/github"
	"net/url"
	"strings"
)

func GetLatestReleaseTag(url string) (string, error) {
	client := github.NewClient(nil)
	owner, repo, err := getGithubOwnerAndRepo(url)
	if err != nil {
		return "", err
	}
	release, _, err := client.Repositories.GetLatestRelease(context.Background(), owner, repo)
	if err != nil {
		return "", err
	}
	if release == nil {
		return "", fmt.Errorf("no releases found for %s/%s", owner, repo)
	}

	return release.GetTagName(), nil
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
