/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/google/go-github/v88/github"
)

// githubClient is the minimal GitHub surface the Release controller needs to
// write components/<component>/{environments,values}/<env>.yaml into an
// existing, non-empty repository (application-repositories) as one atomic
// commit. Unlike scaffold-operator's githubClient, this never has to handle
// a brand-new/empty repository — application-repositories always exists
// with history — so there's no RepositoryState/CommitExists bootstrapping
// here, just: read current file content (to skip a no-op commit), and
// commit files on top of the branch's current head.
type githubClient interface {
	// GetFileContent returns path's content at ref, or found=false if it
	// doesn't exist yet.
	GetFileContent(ctx context.Context, owner, repo, path, ref string) (content []byte, found bool, err error)

	// GetBranchHead returns branch's current head commit SHA and that
	// commit's tree SHA (used as base_tree for CommitFiles).
	GetBranchHead(ctx context.Context, owner, repo, branch string) (headSHA, treeSHA string, err error)

	// CommitFiles creates one commit containing files (path -> content) on
	// top of parentSHA (using baseTreeSHA as the new tree's base_tree so
	// every other file in the repo is preserved untouched), and moves
	// branch's ref to it. Returns the new commit's SHA.
	CommitFiles(ctx context.Context, owner, repo, branch, message string, files map[string][]byte, parentSHA, baseTreeSHA string) (string, error)

	// LatestSuccessfulRun returns the newest successful run of workflowFile
	// triggered by a push to branch, or nil if there is none yet (including
	// when the workflow file doesn't exist). Only auto-deploy calls this.
	LatestSuccessfulRun(ctx context.Context, owner, repo, workflowFile, branch string) (*workflowRun, error)

	// CompareCommits returns how head relates to base: "ahead", "behind",
	// "identical" or "diverged" (GitHub's compare API). Auto-deploy only
	// moves a Release's version to a commit that is ahead of it.
	CompareCommits(ctx context.Context, owner, repo, base, head string) (string, error)
}

// workflowRun is the part of a GitHub Actions run auto-deploy needs.
type workflowRun struct {
	HeadSHA string
	HTMLURL string
}

// goGithubClient is githubClient backed by the real GitHub API.
type goGithubClient struct {
	gh *github.Client
}

// githubTimeout bounds every GitHub API call, so a hung request can't stall
// a reconcile worker.
const githubTimeout = 30 * time.Second

// newGoGithubClient authenticates with a static token (the PAT).
func newGoGithubClient(token string) (githubClient, error) {
	gh, err := github.NewClient(github.WithAuthToken(token), github.WithTimeout(githubTimeout))
	if err != nil {
		return nil, err
	}
	return &goGithubClient{gh: gh}, nil
}

// newGoGithubClientWithTransport authenticates through transport (a GitHub
// App installation transport that mints its own tokens).
func newGoGithubClientWithTransport(transport http.RoundTripper) (githubClient, error) {
	gh, err := github.NewClient(github.WithTransport(transport), github.WithTimeout(githubTimeout))
	if err != nil {
		return nil, err
	}
	return &goGithubClient{gh: gh}, nil
}

func (c *goGithubClient) GetFileContent(ctx context.Context, owner, repo, path, ref string) ([]byte, bool, error) {
	fileContent, _, _, err := c.gh.Repositories.GetContents(ctx, owner, repo, path, &github.RepositoryContentGetOptions{Ref: ref})
	if err != nil {
		if isNotFound(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	content, err := fileContent.GetContent()
	if err != nil {
		return nil, false, fmt.Errorf("decoding %s: %w", path, err)
	}
	return []byte(content), true, nil
}

func (c *goGithubClient) GetBranchHead(ctx context.Context, owner, repo, branch string) (string, string, error) {
	ref, _, err := c.gh.Git.GetRef(ctx, owner, repo, "heads/"+branch)
	if err != nil {
		return "", "", fmt.Errorf("getting ref heads/%s: %w", branch, err)
	}
	headSHA := ref.GetObject().GetSHA()

	commit, _, err := c.gh.Git.GetCommit(ctx, owner, repo, headSHA)
	if err != nil {
		return "", "", fmt.Errorf("getting head commit %s: %w", headSHA, err)
	}
	return headSHA, commit.GetTree().GetSHA(), nil
}

func (c *goGithubClient) CommitFiles(ctx context.Context, owner, repo, branch, message string, files map[string][]byte, parentSHA, baseTreeSHA string) (string, error) {
	entries := make([]*github.TreeEntry, 0, len(files))
	for path, content := range files {
		blob, _, err := c.gh.Git.CreateBlob(ctx, owner, repo, github.Blob{
			Content:  github.Ptr(base64.StdEncoding.EncodeToString(content)),
			Encoding: github.Ptr("base64"),
		})
		if err != nil {
			return "", fmt.Errorf("creating blob for %s: %w", path, err)
		}
		entries = append(entries, &github.TreeEntry{
			Path: github.Ptr(path),
			Mode: github.Ptr("100644"),
			Type: github.Ptr("blob"),
			SHA:  blob.SHA,
		})
	}

	tree, _, err := c.gh.Git.CreateTree(ctx, owner, repo, baseTreeSHA, entries)
	if err != nil {
		return "", fmt.Errorf("creating tree: %w", err)
	}

	commit, _, err := c.gh.Git.CreateCommit(ctx, owner, repo, github.Commit{
		Message: github.Ptr(message),
		Tree:    tree,
		Parents: []*github.Commit{{SHA: github.Ptr(parentSHA)}},
	}, nil)
	if err != nil {
		return "", fmt.Errorf("creating commit: %w", err)
	}

	if _, _, err := c.gh.Git.UpdateRef(ctx, owner, repo, "refs/heads/"+branch, github.UpdateRef{SHA: commit.GetSHA()}); err != nil {
		return "", fmt.Errorf("updating ref refs/heads/%s: %w", branch, err)
	}

	return commit.GetSHA(), nil
}

func (c *goGithubClient) LatestSuccessfulRun(ctx context.Context, owner, repo, workflowFile, branch string) (*workflowRun, error) {
	// Same query as Backstage's DeployableVersionReader (the Create
	// deployment Version picker), so auto-deploy and a person picking a
	// version agree on what "deployable" means. A push run on main only
	// succeeds once its push-image job has pushed the SHA-tagged image.
	runs, _, err := c.gh.Actions.ListWorkflowRunsByFileName(ctx, owner, repo, workflowFile, &github.ListWorkflowRunsOptions{
		Branch:      branch,
		Event:       "push",
		Status:      "success",
		ListOptions: github.ListOptions{PerPage: 1},
	})
	if err != nil {
		if isNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("listing %s runs on %s/%s: %w", workflowFile, owner, repo, err)
	}
	if len(runs.WorkflowRuns) == 0 {
		return nil, nil
	}
	run := runs.WorkflowRuns[0]
	return &workflowRun{
		HeadSHA: run.GetHeadSHA(),
		HTMLURL: run.GetHTMLURL(),
	}, nil
}

func (c *goGithubClient) CompareCommits(ctx context.Context, owner, repo, base, head string) (string, error) {
	comparison, _, err := c.gh.Repositories.CompareCommits(ctx, owner, repo, base, head, &github.ListOptions{PerPage: 1})
	if err != nil {
		return "", fmt.Errorf("comparing %s...%s on %s/%s: %w", base, head, owner, repo, err)
	}
	return comparison.GetStatus(), nil
}

func isNotFound(err error) bool {
	var ghErr *github.ErrorResponse
	return errors.As(err, &ghErr) && ghErr.Response != nil && ghErr.Response.StatusCode == http.StatusNotFound
}
