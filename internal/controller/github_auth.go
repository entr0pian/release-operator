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
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"sync"

	"github.com/bradleyfalzon/ghinstallation/v2"
	"github.com/google/go-github/v88/github"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// GitHubAuthApp authenticates as the platform's GitHub App
	// (taskapp-platform-deployer) with short-lived installation tokens.
	GitHubAuthApp = "app"
	// GitHubAuthPAT authenticates with the shared crossplane-github-credentials
	// personal access token. Kept for clusters without the App; never used as
	// a fallback when the App's credentials are missing.
	GitHubAuthPAT = "pat"

	// Keys of the GitHub App credentials Secret, as the chart's
	// ExternalSecret writes them from taskapp/platform/github-app.
	appIDKey          = "appId"
	installationIDKey = "installationId"
	privateKeyKey     = "privateKey"
)

// GitHubClientSource hands the reconciler a GitHub client and the account
// it writes as. It's resolved on every use, so rotated credentials are
// picked up without a restart.
type GitHubClientSource interface {
	clientFor(ctx context.Context) (githubClient, string, error)
}

// NewGitHubAppSource authenticates as a GitHub App installation whose App
// ID, installation ID and private key are in secret. owner is the account
// the App is installed on.
func NewGitHubAppSource(reader client.Reader, secret types.NamespacedName, owner string) GitHubClientSource {
	return &appSource{reader: reader, secret: secret, owner: owner, newTransport: newInstallationTransport}
}

// NewGitHubPATSource authenticates with the token in the shared
// crossplane-system/crossplane-github-credentials Secret.
func NewGitHubPATSource(reader client.Reader) GitHubClientSource {
	return &patSource{reader: reader, newClient: newGoGithubClient}
}

// patSource reads the shared crossplane-github-credentials Secret via a
// direct client.Get (Secrets don't mount cross-namespace).
type patSource struct {
	reader    client.Reader
	newClient func(token string) (githubClient, error)
}

// githubCredentials mirrors the single "credentials" key on the
// crossplane-github-credentials Secret — a JSON blob, not separate Secret
// keys (same shape scaffold-operator already reads).
type githubCredentials struct {
	Token string `json:"token"`
	Owner string `json:"owner"`
}

func (s *patSource) clientFor(ctx context.Context) (githubClient, string, error) {
	secret := &corev1.Secret{}
	if err := s.reader.Get(ctx, types.NamespacedName{Name: credentialsSecretName, Namespace: credentialsSecretNamespace}, secret); err != nil {
		return nil, "", fmt.Errorf("reading %s/%s credentials secret: %w", credentialsSecretNamespace, credentialsSecretName, err)
	}

	raw, ok := secret.Data["credentials"]
	if !ok {
		return nil, "", fmt.Errorf("%s/%s secret has no \"credentials\" key", credentialsSecretNamespace, credentialsSecretName)
	}

	var creds githubCredentials
	if err := json.Unmarshal(raw, &creds); err != nil {
		return nil, "", fmt.Errorf("parsing %s/%s credentials: %w", credentialsSecretNamespace, credentialsSecretName, err)
	}
	gh, err := s.newClient(creds.Token)
	if err != nil {
		return nil, "", fmt.Errorf("%s/%s credentials: %w", credentialsSecretNamespace, credentialsSecretName, err)
	}
	return gh, creds.Owner, nil
}

// appSource builds clients from the App credentials Secret. Each client's
// transport mints and refreshes its own installation tokens, so the clients
// are cached and only rebuilt when the Secret changes; a new transport per
// reconcile would mint a new token on every poll.
type appSource struct {
	reader       client.Reader
	secret       types.NamespacedName
	owner        string
	newTransport func(appID, installationID int64, privateKey []byte, opts *github.InstallationTokenOptions) (http.RoundTripper, error)

	mu              sync.Mutex
	resourceVersion string
	cached          githubClient
}

func (s *appSource) clientFor(ctx context.Context) (githubClient, string, error) {
	if s.owner == "" {
		return nil, "", fmt.Errorf("GitHub App auth needs the owner the App is installed on (--github-owner)")
	}

	secret := &corev1.Secret{}
	if err := s.reader.Get(ctx, s.secret, secret); err != nil {
		return nil, "", fmt.Errorf("reading GitHub App credentials secret %s: %w", s.secret, err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cached != nil && s.resourceVersion == secret.ResourceVersion {
		return s.cached, s.owner, nil
	}

	gh, err := s.build(secret)
	if err != nil {
		return nil, "", err
	}
	s.cached, s.resourceVersion = gh, secret.ResourceVersion
	return gh, s.owner, nil
}

func (s *appSource) build(secret *corev1.Secret) (githubClient, error) {
	appID, err := secretInt(secret, appIDKey)
	if err != nil {
		return nil, err
	}
	installationID, err := secretInt(secret, installationIDKey)
	if err != nil {
		return nil, err
	}
	privateKey := secret.Data[privateKeyKey]
	if len(privateKey) == 0 {
		return nil, fmt.Errorf("GitHub App credentials secret %s has no %q key", s.secret, privateKeyKey)
	}

	// Each token gets only what its calls need: commits go to
	// application-repositories alone, and service repositories are only
	// ever read (their CI runs and commit history).
	write, err := s.newTransport(appID, installationID, privateKey, &github.InstallationTokenOptions{
		Repositories: []string{applicationRepositoriesRepo},
		Permissions:  &github.InstallationPermissions{Contents: github.Ptr("write")},
	})
	if err != nil {
		return nil, fmt.Errorf("GitHub App credentials secret %s: %w", s.secret, err)
	}
	read, err := s.newTransport(appID, installationID, privateKey, &github.InstallationTokenOptions{
		Permissions: &github.InstallationPermissions{Actions: github.Ptr("read"), Contents: github.Ptr("read")},
	})
	if err != nil {
		return nil, fmt.Errorf("GitHub App credentials secret %s: %w", s.secret, err)
	}

	writeClient, err := newGoGithubClientWithTransport(write)
	if err != nil {
		return nil, err
	}
	readClient, err := newGoGithubClientWithTransport(read)
	if err != nil {
		return nil, err
	}
	return &repoRoutingClient{applicationRepositories: writeClient, services: readClient}, nil
}

func secretInt(secret *corev1.Secret, key string) (int64, error) {
	raw, ok := secret.Data[key]
	if !ok {
		return 0, fmt.Errorf("GitHub App credentials secret %s/%s has no %q key", secret.Namespace, secret.Name, key)
	}
	n, err := strconv.ParseInt(string(raw), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("GitHub App credentials secret %s/%s: %q is not a number", secret.Namespace, secret.Name, key)
	}
	return n, nil
}

func newInstallationTransport(appID, installationID int64, privateKey []byte, opts *github.InstallationTokenOptions) (http.RoundTripper, error) {
	tr, err := ghinstallation.New(http.DefaultTransport, appID, installationID, privateKey)
	if err != nil {
		return nil, err
	}
	tr.InstallationTokenOptions = opts
	return tr, nil
}

// repoRoutingClient sends calls on application-repositories to one client
// and calls on every other repository to another, so each can carry a token
// scoped to just that use.
type repoRoutingClient struct {
	applicationRepositories githubClient
	services                githubClient
}

func (c *repoRoutingClient) forRepo(repo string) githubClient {
	if repo == applicationRepositoriesRepo {
		return c.applicationRepositories
	}
	return c.services
}

func (c *repoRoutingClient) GetFileContent(ctx context.Context, owner, repo, path, ref string) ([]byte, bool, error) {
	return c.forRepo(repo).GetFileContent(ctx, owner, repo, path, ref)
}

func (c *repoRoutingClient) GetBranchHead(ctx context.Context, owner, repo, branch string) (string, string, error) {
	return c.forRepo(repo).GetBranchHead(ctx, owner, repo, branch)
}

func (c *repoRoutingClient) CommitFiles(ctx context.Context, owner, repo, branch, message string, files map[string][]byte, parentSHA, baseTreeSHA string) (string, error) {
	return c.forRepo(repo).CommitFiles(ctx, owner, repo, branch, message, files, parentSHA, baseTreeSHA)
}

func (c *repoRoutingClient) LatestSuccessfulRun(ctx context.Context, owner, repo, workflowFile, branch string) (*workflowRun, error) {
	return c.forRepo(repo).LatestSuccessfulRun(ctx, owner, repo, workflowFile, branch)
}

func (c *repoRoutingClient) CompareCommits(ctx context.Context, owner, repo, base, head string) (string, error) {
	return c.forRepo(repo).CompareCommits(ctx, owner, repo, base, head)
}
