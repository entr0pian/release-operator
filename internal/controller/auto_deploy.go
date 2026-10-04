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
	"bytes"
	"context"
	"fmt"

	"gopkg.in/yaml.v3"

	platformv1alpha1 "github.com/entr0pian/release-operator/api/v1alpha1"
)

// status.autoDeploy.reason values.
const (
	autoDeployUpToDate             = "UpToDate"
	autoDeployDeploying            = "Deploying"
	autoDeployWaitingForFirstBuild = "WaitingForFirstBuild"
	autoDeployNotFastForward       = "NotFastForward"
	autoDeployReleaseFileNotFound  = "ReleaseFileNotFound"
)

// runAutoDeploy moves an auto-deploy Release's version forward in git: when
// the newest successful CI build on spec.autoDeploy.branch is ahead of the
// version in the Release's own file in application-repositories, it commits
// that build's commit as spec.version there. It never touches the Release in
// the cluster (Argo CD applies the file, as for any other change), and every
// decision is made against the file in git, not the possibly stale cluster
// copy, so a human change that hasn't synced yet always wins. The outcome is
// recorded in release.Status.AutoDeploy; an error is returned only when
// GitHub couldn't be asked or written to.
func (r *ReleaseReconciler) runAutoDeploy(ctx context.Context, release *platformv1alpha1.Release, repoURL string) error {
	branch := release.Spec.AutoDeploy.Branch
	status := &platformv1alpha1.AutoDeployStatus{Branch: branch}
	release.Status.AutoDeploy = status

	owner, repo, err := parseGitHubRepo(repoURL)
	if err != nil {
		return err
	}
	gh, gitopsOwner, err := r.githubClientFor(ctx)
	if err != nil {
		return err
	}
	latest, err := gh.LatestSuccessfulRun(ctx, owner, repo, ciWorkflowFile, branch)
	if err != nil {
		return err
	}
	if latest == nil {
		if release.Spec.Version == "" {
			status.Reason = autoDeployWaitingForFirstBuild
			status.Message = fmt.Sprintf("no successful %s run on %s/%s %s yet", ciWorkflowFile, owner, repo, branch)
		} else {
			// Runs can age out of GitHub's listing; keep what's deployed.
			status.Reason = autoDeployUpToDate
			status.Message = fmt.Sprintf("no successful %s run listed on %s; keeping %s", ciWorkflowFile, branch, shortSHA(release.Spec.Version))
		}
		return nil
	}
	status.LatestDeployable = latest.HeadSHA
	status.RunURL = latest.HTMLURL
	if latest.HeadSHA == release.Spec.Version {
		status.Reason = autoDeployUpToDate
		status.Message = fmt.Sprintf("%s is the newest successful build on %s", shortSHA(latest.HeadSHA), branch)
		return nil
	}

	// Pin the parent commit first and read the file at exactly that commit:
	// if anything lands on application-repositories in between, the commit
	// below is rejected instead of overwriting it.
	headSHA, treeSHA, err := gh.GetBranchHead(ctx, gitopsOwner, applicationRepositoriesRepo, applicationRepositoriesRef)
	if err != nil {
		return fmt.Errorf("reading %s head: %w", applicationRepositoriesRepo, err)
	}
	path := releaseFilePath(release)
	content, found, err := gh.GetFileContent(ctx, gitopsOwner, applicationRepositoriesRepo, path, headSHA)
	if err != nil {
		return fmt.Errorf("reading %s: %w", path, err)
	}
	if !found {
		// Never create it: a missing file means it was removed (e.g. a
		// teardown) or lives elsewhere, and recreating it would undo that.
		status.Reason = autoDeployReleaseFileNotFound
		status.Message = fmt.Sprintf("%s not found in %s; auto-deploy only edits an existing Release file", path, applicationRepositoriesRepo)
		return nil
	}
	file, err := parseReleaseFile(content)
	if err != nil || file.name != release.Name {
		status.Reason = autoDeployReleaseFileNotFound
		status.Message = fmt.Sprintf("%s in %s is not Release %s", path, applicationRepositoriesRepo, release.Name)
		return nil
	}
	if file.branch != branch {
		status.Reason = autoDeployDeploying
		status.Message = fmt.Sprintf("auto-deploy changed in %s; waiting for Argo CD to apply it", path)
		return nil
	}
	if file.version == latest.HeadSHA {
		status.Reason = autoDeployDeploying
		status.Message = fmt.Sprintf("%s committed to %s; waiting for Argo CD to apply it", shortSHA(latest.HeadSHA), path)
		return nil
	}
	if file.version != "" {
		relation, err := gh.CompareCommits(ctx, owner, repo, file.version, latest.HeadSHA)
		if err != nil {
			return err
		}
		if relation != "ahead" {
			status.Reason = autoDeployNotFastForward
			status.Message = fmt.Sprintf("newest build %s is %s %s, not ahead of it; only moving forward", shortSHA(latest.HeadSHA), relation, shortSHA(file.version))
			return nil
		}
	}

	updated, err := file.withVersion(latest.HeadSHA)
	if err != nil {
		return err
	}
	message := fmt.Sprintf("Auto-deploy %s %s to %s\n\nBuilt by %s", release.Spec.ComponentRef.Name, shortSHA(latest.HeadSHA), release.Spec.Environment, latest.HTMLURL)
	if _, err := gh.CommitFiles(ctx, gitopsOwner, applicationRepositoriesRepo, applicationRepositoriesRef, message, map[string][]byte{path: updated}, headSHA, treeSHA); err != nil {
		return fmt.Errorf("committing %s: %w", path, err)
	}
	status.Reason = autoDeployDeploying
	status.Message = fmt.Sprintf("%s committed to %s; waiting for Argo CD to apply it", shortSHA(latest.HeadSHA), path)
	return nil
}

// releaseFilePath is where a Release's manifest lives in
// application-repositories: the directory is the namespace the platform
// ApplicationSet applies it to, the file name the one Backstage's Create
// deployment and Onboard Service templates write.
func releaseFilePath(release *platformv1alpha1.Release) string {
	return fmt.Sprintf("platform/environments/%s/%s-release.yaml", release.Namespace, release.Spec.ComponentRef.Name)
}

// releaseFile is a Release manifest from git, kept as a YAML node tree so
// that setting spec.version leaves everything else (bindings, labels,
// comments, key order) as it was.
type releaseFile struct {
	root    yaml.Node
	spec    *yaml.Node
	name    string
	branch  string
	version string
}

func parseReleaseFile(content []byte) (*releaseFile, error) {
	f := &releaseFile{}
	if err := yaml.Unmarshal(content, &f.root); err != nil {
		return nil, err
	}
	if f.root.Kind != yaml.DocumentNode || len(f.root.Content) != 1 {
		return nil, fmt.Errorf("not a single YAML document")
	}
	top := f.root.Content[0]
	if kind := mappingValue(top, "kind"); kind == nil || kind.Value != "Release" {
		return nil, fmt.Errorf("not a Release")
	}
	f.spec = mappingValue(top, "spec")
	if f.spec == nil || f.spec.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("no spec")
	}
	f.name = scalarValue(mappingValue(mappingValue(top, "metadata"), "name"))
	f.branch = scalarValue(mappingValue(mappingValue(f.spec, "autoDeploy"), "branch"))
	f.version = scalarValue(mappingValue(f.spec, "version"))
	return f, nil
}

// withVersion renders the file with spec.version set to version, quoted
// like the Backstage templates write it. A missing version key is added
// right after spec.environment.
func (f *releaseFile) withVersion(version string) ([]byte, error) {
	if existing := mappingValue(f.spec, "version"); existing != nil {
		existing.Kind, existing.Tag, existing.Value, existing.Style = yaml.ScalarNode, "!!str", version, yaml.DoubleQuotedStyle
	} else {
		key := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "version"}
		value := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: version, Style: yaml.DoubleQuotedStyle}
		at := len(f.spec.Content)
		for i := 0; i+1 < len(f.spec.Content); i += 2 {
			if f.spec.Content[i].Value == "environment" {
				at = i + 2
			}
		}
		f.spec.Content = append(f.spec.Content[:at], append([]*yaml.Node{key, value}, f.spec.Content[at:]...)...)
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&f.root); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func mappingValue(node *yaml.Node, key string) *yaml.Node {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return node.Content[i+1]
		}
	}
	return nil
}

func scalarValue(node *yaml.Node) string {
	if node == nil || node.Kind != yaml.ScalarNode {
		return ""
	}
	return node.Value
}
