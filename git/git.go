package git

import (
	"context"
	"errors"
	"fmt"
	"github.com/entigolabs/entigo-infralib-agent/model"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/plumbing/transport/http"
	gitSSH "github.com/go-git/go-git/v5/plumbing/transport/ssh"
	"golang.org/x/crypto/ssh"
	"log"
	"log/slog"
	"os"
	"sync"
)

const (
	PlanBranch  = "plan"
	ApplyBranch = "apply"
)

type Client struct {
	ctx      context.Context
	auth     transport.AuthMethod
	name     string
	worktree *git.Worktree
	repo     *git.Repository
	insecure bool
	mu       sync.Mutex
}

func NewGitClient(ctx context.Context, name string, config model.Git) (*Client, error) {
	log.Printf("Preparing git repository %s", config.URL)
	auth, err := getAuth(config)
	if err != nil {
		return nil, err
	}
	worktree, repo, err := getRepo(ctx, auth, config, os.TempDir()+name)
	if err != nil {
		return nil, err
	}
	return &Client{
		ctx:      ctx,
		auth:     auth,
		name:     name,
		worktree: worktree,
		repo:     repo,
		insecure: config.Insecure,
	}, nil
}

func getAuth(config model.Git) (transport.AuthMethod, error) {
	if config.Key != "" {
		publicKeys, err := gitSSH.NewPublicKeys("git", []byte(config.Key), config.KeyPassword)
		if err != nil {
			return nil, err
		}
		if config.InsecureHostKey {
			publicKeys.HostKeyCallback = ssh.InsecureIgnoreHostKey()
		}
		return publicKeys, nil
	}
	return &http.BasicAuth{
		Username: config.Username,
		Password: config.Password,
	}, nil
}

func getRepo(ctx context.Context, auth transport.AuthMethod, gitConfig model.Git, repoPath string) (*git.Worktree, *git.Repository, error) {
	repo, err := openRepo(ctx, auth, gitConfig, repoPath)
	if errors.Is(err, git.ErrRepositoryNotExists) {
		repo, err = cloneRepo(ctx, auth, gitConfig, repoPath)
	}
	if err != nil {
		return nil, nil, err
	}
	worktree, err := repo.Worktree()
	if err != nil {
		return nil, nil, err
	}
	err = ensureBranch(auth, repo, worktree, PlanBranch, gitConfig.Insecure)
	if err != nil {
		return nil, nil, err
	}
	err = ensureBranch(auth, repo, worktree, ApplyBranch, gitConfig.Insecure)
	if err != nil {
		return nil, nil, err
	}
	return worktree, repo, nil
}

func openRepo(ctx context.Context, auth transport.AuthMethod, gitConfig model.Git, path string) (*git.Repository, error) {
	repo, err := git.PlainOpen(path)
	if err != nil {
		return nil, err
	}
	err = repo.FetchContext(ctx, &git.FetchOptions{
		Auth:            auth,
		RemoteName:      git.DefaultRemoteName,
		InsecureSkipTLS: gitConfig.Insecure,
		Depth:           1,
		Prune:           true,
	})
	if err == nil || errors.Is(err, git.NoErrAlreadyUpToDate) {
		return repo, nil
	}
	return nil, err
}

func cloneRepo(ctx context.Context, auth transport.AuthMethod, gitConfig model.Git, repoPath string) (*git.Repository, error) {
	repo, err := git.PlainCloneContext(ctx, repoPath, false, &git.CloneOptions{
		URL:             gitConfig.URL,
		Depth:           1,
		Auth:            auth,
		InsecureSkipTLS: gitConfig.Insecure,
	})
	if errors.Is(err, transport.ErrEmptyRemoteRepository) {
		repo, err = initRepo(gitConfig, repoPath)
	}
	if err != nil {
		return nil, err
	}
	return repo, nil
}

func initRepo(gitConfig model.Git, repoPath string) (*git.Repository, error) {
	repo, err := git.PlainInit(repoPath, false)
	if err != nil {
		return nil, err
	}
	_, err = repo.CreateRemote(&config.RemoteConfig{
		Name: git.DefaultRemoteName,
		URLs: []string{gitConfig.URL},
	})
	return repo, err
}

func ensureBranch(auth transport.AuthMethod, repo *git.Repository, worktree *git.Worktree, branch string, insecure bool) error {
	remoteExists, err := remoteBranchExists(repo, branch)
	if err != nil {
		return err
	}
	localExists, err := localBranchExists(repo, branch)
	if !remoteExists {
		return createRemoteBranch(auth, repo, worktree, branch, insecure, localExists)
	}
	if localExists {
		return nil
	}
	return createLocalBranch(repo, branch)
}

func remoteBranchExists(repo *git.Repository, branch string) (bool, error) {
	refs, err := repo.References()
	if err != nil {
		return false, err
	}
	remoteBranch := plumbing.NewRemoteReferenceName(git.DefaultRemoteName, branch)
	exists := false
	err = refs.ForEach(func(ref *plumbing.Reference) error {
		if ref.Name() == remoteBranch {
			exists = true
			return nil
		}
		return nil
	})
	return exists, err
}

func localBranchExists(repo *git.Repository, branch string) (bool, error) {
	_, err := repo.Branch(branch)
	if err == nil {
		return true, nil
	}
	if !errors.Is(err, git.ErrBranchNotFound) {
		return false, err
	}
	return false, nil
}

func createLocalBranch(repo *git.Repository, branch string) error {
	err := repo.CreateBranch(&config.Branch{
		Name:  branch,
		Merge: plumbing.NewBranchReferenceName(branch),
	})
	if err != nil {
		return err
	}
	remote, err := repo.Reference(plumbing.NewRemoteReferenceName(git.DefaultRemoteName, branch), true)
	if err != nil {
		return err
	}
	hashRef := plumbing.NewHashReference(plumbing.NewBranchReferenceName(branch), remote.Hash())
	return repo.Storer.SetReference(hashRef)
}

func createRemoteBranch(auth transport.AuthMethod, repo *git.Repository, worktree *git.Worktree, branch string, insecure, localExists bool) error {
	err := worktree.Checkout(&git.CheckoutOptions{
		Branch: plumbing.NewBranchReferenceName(branch),
		Create: !localExists,
	})
	if err != nil {
		return err
	}
	return repo.Push(&git.PushOptions{
		RefSpecs:        []config.RefSpec{config.RefSpec(fmt.Sprintf("refs/heads/%s:refs/heads/%s", branch, branch))},
		Auth:            auth,
		InsecureSkipTLS: insecure,
	})
}

func (g *Client) UpdateFiles(branch, folder string, files map[string][]byte) error {
	if len(files) == 0 {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()

	err := g.checkoutCleanBranch(branch)
	if err != nil {
		return err
	}
	err = g.updateFiles(folder, files)
	if err != nil {
		return err
	}
	changes, err := g.hasChanges()
	if err != nil {
		return err
	}
	if !changes {
		return nil
	}
	_, err = g.worktree.Commit(fmt.Sprintf("Infralib agent updated %s files", folder), &git.CommitOptions{})
	if errors.Is(err, git.ErrEmptyCommit) {
		return nil
	}
	if err != nil {
		return err
	}
	err = g.repo.Push(&git.PushOptions{
		RefSpecs:        []config.RefSpec{config.RefSpec(fmt.Sprintf("refs/heads/%s:refs/heads/%s", branch, branch))},
		Auth:            g.auth,
		InsecureSkipTLS: g.insecure,
	})
	if errors.Is(err, git.NoErrAlreadyUpToDate) {
		return nil
	}
	return err
}

func (g *Client) checkoutCleanBranch(branch string) error {
	branchName := plumbing.NewBranchReferenceName(branch)
	err := g.worktree.Checkout(&git.CheckoutOptions{
		Branch: branchName,
		Force:  true,
	})
	if err != nil {
		return err
	}

	err = g.worktree.PullContext(g.ctx, &git.PullOptions{
		ReferenceName: plumbing.NewBranchReferenceName(branch),
		SingleBranch:  true,
		Auth:          g.auth,
	})
	if err != nil && !errors.Is(err, git.NoErrAlreadyUpToDate) {
		return err
	}
	return nil
}

func (g *Client) updateFiles(folder string, files map[string][]byte) error {
	err := g.worktree.Filesystem.MkdirAll(folder, os.ModeDir)
	if err != nil {
		return err
	}

	currentFiles := model.NewSet[string]()
	for path, content := range files {
		file, err := g.worktree.Filesystem.OpenFile(path, os.O_RDWR|os.O_CREATE, 0666)
		if err != nil {
			return err
		}
		_, err = file.Write(content)
		if err != nil {
			return err
		}
		err = file.Close()
		if err != nil {
			return err
		}
		_, err = g.worktree.Add(path)
		if err != nil {
			return err
		}
		currentFiles.Add(path)
	}
	return g.removeUnusedFiles(folder, currentFiles)
}

func (g *Client) removeUnusedFiles(path string, currentFiles model.Set[string]) error {
	infos, err := g.worktree.Filesystem.ReadDir(path)
	if err != nil {
		return err
	}

	empty := true
	for _, info := range infos {
		fullPath := g.worktree.Filesystem.Join(path, info.Name())
		if info.IsDir() {
			err = g.removeUnusedFiles(fullPath, currentFiles)
			if err != nil {
				return err
			}
			continue
		}
		if currentFiles.Contains(fullPath) {
			empty = false
			continue
		}
		err = g.worktree.Filesystem.Remove(fullPath)
		if err != nil {
			return err
		}
	}

	if !empty {
		return nil
	}
	return g.worktree.Filesystem.Remove(path)
}

func (g *Client) hasChanges() (bool, error) {
	status, err := g.worktree.Status()
	if err != nil {
		return false, err
	}
	hasChanges := !status.IsClean()
	if hasChanges {
		slog.Debug(fmt.Sprintf("Destination %s git status\n%s", g.name, status))
	} else {
		slog.Debug(fmt.Sprintf("Destination %s git status is clean", g.name))
	}
	return hasChanges, nil
}
