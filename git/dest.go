package git

import (
	"context"
	"errors"
	"fmt"
	"github.com/entigolabs/entigo-infralib-agent/model"
	"github.com/go-git/go-billy/v5"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/plumbing/transport/http"
	gitSSH "github.com/go-git/go-git/v5/plumbing/transport/ssh"
	"golang.org/x/crypto/ssh"
	"log"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	PlanBranch  = "plan"
	ApplyBranch = "apply"

	readmeContent = `### Generated by Entigo Infralib Agent\n
Agent will push changes to named subfolders inside the steps folder.
Changes are pushed after the files have been generated for a step and after the step has been successfully applied.
Changes are pushed to the plan and apply branches.`
	authorName  = "Entigo Infralib Agent"
	authorEmail = "no-reply@localhost"
)

type DestClient struct {
	ctx      context.Context
	auth     transport.AuthMethod
	author   *object.Signature
	name     string
	worktree *git.Worktree
	repo     *git.Repository
	insecure bool
	mu       sync.Mutex
	CABundle []byte
}

func NewDestClient(ctx context.Context, name string, config model.Git, CABundle []byte) (*DestClient, error) {
	log.Printf("Preparing git repository %s", config.URL)
	auth, err := getAuth(config)
	if err != nil {
		return nil, err
	}
	worktree, repo, err := getRepo(ctx, auth, config, name, CABundle)
	if err != nil {
		return nil, err
	}
	return &DestClient{
		ctx:      ctx,
		auth:     auth,
		author:   getAuthor(config),
		name:     name,
		worktree: worktree,
		repo:     repo,
		insecure: config.Insecure,
		CABundle: CABundle,
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

func getAuthor(config model.Git) *object.Signature {
	name := config.AuthorName
	if name == "" {
		name = authorName
	}
	email := config.AuthorEmail
	if email == "" {
		email = authorEmail
	}
	return &object.Signature{
		Name:  name,
		Email: email,
		When:  time.Now().UTC(),
	}
}

func getRepo(ctx context.Context, auth transport.AuthMethod, gitConfig model.Git, name string, CABundle []byte) (*git.Worktree, *git.Repository, error) {
	repoPath := filepath.Join(os.TempDir(), name)
	repo, err := openRepo(ctx, auth, gitConfig, repoPath, CABundle)
	if errors.Is(err, git.ErrRepositoryNotExists) {
		repo, err = cloneRepo(ctx, auth, gitConfig, repoPath, CABundle)
	}
	if err != nil {
		return nil, nil, err
	}
	worktree, err := repo.Worktree()
	if err != nil {
		return nil, nil, err
	}
	err = ensureBranch(auth, repo, worktree, PlanBranch, gitConfig.Insecure, CABundle)
	if err != nil {
		return nil, nil, err
	}
	err = ensureBranch(auth, repo, worktree, ApplyBranch, gitConfig.Insecure, CABundle)
	if err != nil {
		return nil, nil, err
	}
	return worktree, repo, nil
}

func openRepo(ctx context.Context, auth transport.AuthMethod, gitConfig model.Git, path string, CABundle []byte) (*git.Repository, error) {
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
		CABundle:        CABundle,
	})
	if err == nil || errors.Is(err, git.NoErrAlreadyUpToDate) {
		return repo, nil
	}
	return nil, err
}

func cloneRepo(ctx context.Context, auth transport.AuthMethod, gitConfig model.Git, repoPath string, CABundle []byte) (*git.Repository, error) {
	repo, err := git.PlainCloneContext(ctx, repoPath, false, &git.CloneOptions{
		URL:             gitConfig.URL,
		Depth:           1,
		Auth:            auth,
		InsecureSkipTLS: gitConfig.Insecure,
		CABundle:        CABundle,
	})
	if errors.Is(err, transport.ErrEmptyRemoteRepository) {
		repo, err = initRepo(auth, gitConfig, repoPath, CABundle)
	}
	if err != nil {
		return nil, err
	}
	return repo, nil
}

func initRepo(auth transport.AuthMethod, gitConfig model.Git, repoPath string, CABundle []byte) (*git.Repository, error) {
	repo, err := git.PlainInit(repoPath, false)
	if err != nil {
		return nil, err
	}
	_, err = repo.CreateRemote(&config.RemoteConfig{
		Name: git.DefaultRemoteName,
		URLs: []string{gitConfig.URL},
	})
	if err != nil {
		return nil, err
	}
	worktree, err := repo.Worktree()
	if err != nil {
		return nil, err
	}
	err = updateFile(worktree, "README.md", []byte(readmeContent))
	if err != nil {
		return nil, err
	}
	_, err = worktree.Commit("Initial commit", &git.CommitOptions{Author: getAuthor(gitConfig)})
	if err != nil {
		return nil, err
	}
	err = repo.Push(&git.PushOptions{
		Auth:            auth,
		InsecureSkipTLS: gitConfig.Insecure,
		CABundle:        CABundle,
	})
	return repo, err
}

func ensureBranch(auth transport.AuthMethod, repo *git.Repository, worktree *git.Worktree, branch string, insecure bool, CABundle []byte) error {
	remoteExists, err := remoteBranchExists(repo, branch)
	if err != nil {
		return err
	}
	localExists, err := localBranchExists(repo, branch)
	if err != nil {
		return err
	}
	if !remoteExists {
		return createRemoteBranch(auth, repo, worktree, branch, insecure, localExists, CABundle)
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

func createRemoteBranch(auth transport.AuthMethod, repo *git.Repository, worktree *git.Worktree, branch string, insecure, localExists bool, CABundle []byte) error {
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
		CABundle:        CABundle,
	})
}

func (g *DestClient) UpdateFiles(branch, folder string, files map[string]model.File) error {
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
	changes, err := g.hasChanges(folder)
	if err != nil {
		return err
	}
	if !changes {
		return nil
	}
	_, err = g.worktree.Commit(fmt.Sprintf("Infralib agent updated %s files", folder), &git.CommitOptions{
		Author: g.author,
	})
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
		CABundle:        g.CABundle,
	})
	if errors.Is(err, git.NoErrAlreadyUpToDate) {
		return nil
	}
	return err
}

func (g *DestClient) checkoutCleanBranch(branch string) error {
	branchName := plumbing.NewBranchReferenceName(branch)
	err := g.worktree.Checkout(&git.CheckoutOptions{
		Branch: branchName,
		Force:  true,
	})
	if err != nil {
		return err
	}

	err = g.worktree.PullContext(g.ctx, &git.PullOptions{
		ReferenceName:   branchName,
		SingleBranch:    true,
		Auth:            g.auth,
		InsecureSkipTLS: g.insecure,
		CABundle:        g.CABundle,
	})
	if err != nil && !errors.Is(err, git.NoErrAlreadyUpToDate) {
		return err
	}
	return nil
}

func (g *DestClient) updateFiles(folder string, files map[string]model.File) error {
	err := g.worktree.Filesystem.MkdirAll(folder, os.ModeDir)
	if err != nil {
		return err
	}

	currentFiles := model.NewSet[string]()
	for path, file := range files {
		err = updateFile(g.worktree, path, file.Content)
		if err != nil {
			return err
		}
		currentFiles.Add(path)
	}
	return g.removeUnusedFiles(folder, currentFiles)
}

func updateFile(worktree *git.Worktree, path string, content []byte) error {
	dir := filepath.Dir(path)
	if dir != "." {
		if err := worktree.Filesystem.MkdirAll(dir, os.ModePerm); err != nil {
			return err
		}
	}
	file, err := worktree.Filesystem.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0666)
	if err != nil {
		return err
	}
	defer func(file billy.File) {
		_ = file.Close()
	}(file)
	_, err = file.Write(content)
	if err != nil {
		return err
	}
	_, err = worktree.Add(path)
	return err
}

func (g *DestClient) removeUnusedFiles(path string, currentFiles model.Set[string]) error {
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
		_, err = g.worktree.Remove(fullPath)
		if err != nil {
			return err
		}
	}

	if !empty {
		return nil
	}
	return g.worktree.Filesystem.Remove(path)
}

func (g *DestClient) hasChanges(folder string) (bool, error) {
	status, err := g.worktree.Status()
	if err != nil {
		return false, err
	}
	hasChanges := !status.IsClean()
	if hasChanges {
		slog.Debug(fmt.Sprintf("Destination %s folder %s git status\n%s", g.name, folder, status))
	} else {
		slog.Debug(fmt.Sprintf("Destination %s folder %s git status is clean", g.name, folder))
	}
	return hasChanges, nil
}
