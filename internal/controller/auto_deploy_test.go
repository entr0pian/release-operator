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
	"fmt"
	"maps"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	platformv1alpha1 "github.com/entr0pian/release-operator/api/v1alpha1"
)

// fakeGitHub is an in-memory githubClient: one branch of one repo
// (application-repositories), plus the CI runs LatestSuccessfulRun reports
// and the answer CompareCommits gives.
type fakeGitHub struct {
	files      map[string][]byte
	commits    []fakeCommit
	latestRun  *workflowRun
	runQueries []string
	relation   string
	compares   []string
	fileReads  int
}

type fakeCommit struct {
	message string
	files   map[string][]byte
}

func newFakeGitHub() *fakeGitHub {
	return &fakeGitHub{files: map[string][]byte{}, relation: "ahead"}
}

func (f *fakeGitHub) GetFileContent(_ context.Context, _, _, path, _ string) ([]byte, bool, error) {
	f.fileReads++
	content, ok := f.files[path]
	return content, ok, nil
}

func (f *fakeGitHub) GetBranchHead(_ context.Context, _, _, _ string) (string, string, error) {
	return fmt.Sprintf("head-%d", len(f.commits)), "tree", nil
}

func (f *fakeGitHub) CommitFiles(_ context.Context, _, _, _, message string, files map[string][]byte, _, _ string) (string, error) {
	maps.Copy(f.files, files)
	f.commits = append(f.commits, fakeCommit{message: message, files: files})
	return fmt.Sprintf("commit-%d", len(f.commits)), nil
}

func (f *fakeGitHub) LatestSuccessfulRun(_ context.Context, owner, repo, workflowFile, branch string) (*workflowRun, error) {
	f.runQueries = append(f.runQueries, fmt.Sprintf("%s/%s %s@%s", owner, repo, workflowFile, branch))
	return f.latestRun, nil
}

func (f *fakeGitHub) CompareCommits(_ context.Context, owner, repo, base, head string) (string, error) {
	f.compares = append(f.compares, fmt.Sprintf("%s/%s %s...%s", owner, repo, base, head))
	return f.relation, nil
}

const (
	shaA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	shaB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

	releasePath = "platform/environments/dev/orders-release.yaml"
	valuesPath  = "components/orders/values/dev.yaml"
)

// releaseYAML renders a Release file the way Backstage's templates write
// it, with a comment and a binding that auto-deploy must leave untouched.
func releaseYAML(name, version string, autoDeploy bool) string {
	s := "apiVersion: platform.taskapp.io/v1alpha1\nkind: Release\nmetadata:\n  name: " + name +
		"\n  labels:\n    platform.taskapp.io/component: orders\nspec:\n  componentRef:\n    name: orders\n  environment: dev\n"
	if version != "" {
		s += "  version: \"" + version + "\"\n"
	}
	if autoDeploy {
		s += "  # follow main\n  autoDeploy:\n    branch: main\n"
	}
	return s
}

func runFor(sha string) *workflowRun {
	return &workflowRun{HeadSHA: sha, HTMLURL: "https://github.com/entr0pian/orders/actions/runs/" + sha[:1]}
}

var _ = Describe("Auto-deploy", func() {
	const (
		ns        = "dev"
		component = "orders"
	)
	ctx := context.Background()

	var (
		gh         *fakeGitHub
		reconciler *ReleaseReconciler
	)

	ensureNamespace := func(name string) {
		err := k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}})
		if err != nil && !errors.IsAlreadyExists(err) {
			Expect(err).NotTo(HaveOccurred())
		}
	}

	BeforeEach(func() {
		for _, n := range []string{ns, "prod", componentNamespace, credentialsSecretNamespace} {
			ensureNamespace(n)
		}

		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: credentialsSecretName, Namespace: credentialsSecretNamespace},
			Data:       map[string][]byte{"credentials": []byte(`{"token":"t","owner":"entr0pian"}`)},
		}
		if err := k8sClient.Create(ctx, secret); err != nil && !errors.IsAlreadyExists(err) {
			Expect(err).NotTo(HaveOccurred())
		}

		c := &unstructured.Unstructured{}
		c.SetGroupVersionKind(componentGVK)
		c.SetName(component)
		c.SetNamespace(componentNamespace)
		Expect(unstructured.SetNestedMap(c.Object, map[string]any{"owner": "orders-team", "repository": map[string]any{}}, "spec")).To(Succeed())
		Expect(k8sClient.Create(ctx, c)).To(Succeed())
		Expect(unstructured.SetNestedMap(c.Object, map[string]any{"url": "https://github.com/entr0pian/orders", "ready": true}, "status", "repository")).To(Succeed())
		Expect(k8sClient.Status().Update(ctx, c)).To(Succeed())

		gh = newFakeGitHub()
		reconciler = &ReleaseReconciler{
			Client:                 k8sClient,
			Scheme:                 k8sClient.Scheme(),
			APIReader:              k8sClient,
			NewGitHubClient:        func(string) githubClient { return gh },
			AutoDeployPollInterval: 42 * time.Second,
		}
	})

	AfterEach(func() {
		c := &unstructured.Unstructured{}
		c.SetGroupVersionKind(componentGVK)
		c.SetName(component)
		c.SetNamespace(componentNamespace)
		Expect(k8sClient.Delete(ctx, c)).To(Succeed())
	})

	createRelease := func(name, env, version string) types.NamespacedName {
		release := &platformv1alpha1.Release{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: env},
			Spec: platformv1alpha1.ReleaseSpec{
				ComponentRef: platformv1alpha1.ComponentReference{Name: component},
				Environment:  env,
				Version:      version,
				AutoDeploy:   &platformv1alpha1.AutoDeploySpec{Branch: "main"},
			},
		}
		Expect(k8sClient.Create(ctx, release)).To(Succeed())
		DeferCleanup(func() { Expect(ignoreNotFound(k8sClient.Delete(ctx, release))).To(Succeed()) })
		return types.NamespacedName{Name: name, Namespace: env}
	}

	reconcileOnce := func(key types.NamespacedName) (reconcile.Result, *platformv1alpha1.Release) {
		result, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		release := &platformv1alpha1.Release{}
		Expect(k8sClient.Get(ctx, key, release)).To(Succeed())
		return result, release
	}

	// argoApplies plays Argo CD: applies the version now in the git file to
	// the Release in the cluster.
	argoApplies := func(key types.NamespacedName, version string) {
		release := &platformv1alpha1.Release{}
		Expect(k8sClient.Get(ctx, key, release)).To(Succeed())
		release.Spec.Version = version
		Expect(k8sClient.Update(ctx, release)).To(Succeed())
	}

	Context("API validation", func() {
		newRelease := func(name string) *platformv1alpha1.Release {
			return &platformv1alpha1.Release{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
				Spec: platformv1alpha1.ReleaseSpec{
					ComponentRef: platformv1alpha1.ComponentReference{Name: component},
					Environment:  ns,
				},
			}
		}

		It("rejects a Release with neither version nor autoDeploy", func() {
			err := k8sClient.Create(ctx, newRelease("neither"))
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("set version, autoDeploy, or both"))
		})

		It("rejects autoDeploy without a branch", func() {
			release := newRelease("no-branch")
			release.Spec.AutoDeploy = &platformv1alpha1.AutoDeploySpec{}
			Expect(k8sClient.Create(ctx, release)).NotTo(Succeed())
		})

		It("accepts version and autoDeploy together", func() {
			release := newRelease("both")
			release.Spec.Version = shaA
			release.Spec.AutoDeploy = &platformv1alpha1.AutoDeploySpec{Branch: "main"}
			Expect(k8sClient.Create(ctx, release)).To(Succeed())
			Expect(k8sClient.Delete(ctx, release)).To(Succeed())
		})
	})

	It("refuses auto-deploy outside the allowed environments, without calling GitHub", func() {
		key := createRelease("orders-prod", "prod", "")
		result, release := reconcileOnce(key)

		Expect(result.RequeueAfter).To(BeZero())
		Expect(findReadyCondition(release).Reason).To(Equal("AutoDeployNotAllowed"))
		Expect(gh.runQueries).To(BeEmpty())
		Expect(gh.commits).To(BeEmpty())
	})

	It("waits for the first successful build instead of failing", func() {
		key := createRelease("orders-dev", ns, "")
		gh.files[releasePath] = []byte(releaseYAML("orders-dev", "", true))
		result, release := reconcileOnce(key)

		Expect(result.RequeueAfter).To(Equal(42 * time.Second))
		Expect(findReadyCondition(release).Reason).To(Equal(autoDeployWaitingForFirstBuild))
		Expect(release.Status.AutoDeploy.Reason).To(Equal(autoDeployWaitingForFirstBuild))
		Expect(gh.runQueries).To(ConsistOf("entr0pian/orders ci.yaml@main"))
		Expect(gh.commits).To(BeEmpty())
	})

	It("commits the first build to the Release file only, and deploys once Argo CD applies it", func() {
		key := createRelease("orders-dev", ns, "")
		gh.files[releasePath] = []byte(releaseYAML("orders-dev", "", true))
		gh.latestRun = runFor(shaA)

		_, release := reconcileOnce(key)
		Expect(gh.commits).To(HaveLen(1))
		Expect(gh.commits[0].files).To(HaveKey(releasePath))
		Expect(gh.commits[0].files).To(HaveLen(1), "auto-deploy writes the Release file, never components/ directly")
		Expect(string(gh.files[releasePath])).To(Equal(releaseYAML("orders-dev", shaA, true)))
		Expect(gh.commits[0].message).To(Equal("Auto-deploy orders aaaaaaa to dev\n\nBuilt by https://github.com/entr0pian/orders/actions/runs/a"))
		Expect(gh.compares).To(BeEmpty(), "nothing to compare against before the first version")
		Expect(release.Spec.Version).To(BeEmpty(), "the cluster copy is never patched")
		Expect(release.Status.AutoDeploy.Reason).To(Equal(autoDeployDeploying))

		// Polling again before Argo CD applies it: no second commit.
		reconcileOnce(key)
		Expect(gh.commits).To(HaveLen(1))

		argoApplies(key, shaA)
		_, release = reconcileOnce(key)
		Expect(gh.commits).To(HaveLen(2))
		Expect(gh.commits[1].files).To(HaveKey(valuesPath))
		Expect(string(gh.files[valuesPath])).To(Equal("image:\n    tag: " + shaA + "\n"))
		Expect(findReadyCondition(release).Reason).To(Equal("Synced"))
		Expect(release.Status.AutoDeploy.Reason).To(Equal(autoDeployUpToDate))
	})

	It("moves the version forward only when the new build is ahead of it", func() {
		key := createRelease("orders-dev", ns, shaA)
		gh.files[releasePath] = []byte(releaseYAML("orders-dev", shaA, true))
		gh.latestRun = runFor(shaB)

		gh.relation = "behind"
		_, release := reconcileOnce(key)
		Expect(gh.compares).To(ConsistOf("entr0pian/orders " + shaA + "..." + shaB))
		Expect(release.Status.AutoDeploy.Reason).To(Equal(autoDeployNotFastForward))
		Expect(string(gh.files[releasePath])).To(Equal(releaseYAML("orders-dev", shaA, true)))

		gh.relation = "ahead"
		_, release = reconcileOnce(key)
		Expect(string(gh.files[releasePath])).To(Equal(releaseYAML("orders-dev", shaB, true)))
		Expect(gh.commits[len(gh.commits)-1].message).To(HavePrefix("Auto-deploy orders bbbbbbb to dev"))
		Expect(release.Spec.Version).To(Equal(shaA))
		Expect(release.Status.AutoDeploy.Reason).To(Equal(autoDeployDeploying))
	})

	It("leaves the file alone when git no longer has auto-deploy, even if the cluster still does", func() {
		key := createRelease("orders-dev", ns, shaA)
		gh.files[releasePath] = []byte(releaseYAML("orders-dev", shaA, false))
		gh.latestRun = runFor(shaB)

		_, release := reconcileOnce(key)
		Expect(string(gh.files[releasePath])).To(Equal(releaseYAML("orders-dev", shaA, false)))
		Expect(release.Status.AutoDeploy.Reason).To(Equal(autoDeployDeploying))
		for _, c := range gh.commits {
			Expect(c.files).NotTo(HaveKey(releasePath))
		}
	})

	It("never creates a missing Release file, or edits one holding another Release", func() {
		key := createRelease("orders-dev", ns, shaA)
		gh.latestRun = runFor(shaB)

		_, release := reconcileOnce(key)
		Expect(release.Status.AutoDeploy.Reason).To(Equal(autoDeployReleaseFileNotFound))
		Expect(gh.files).NotTo(HaveKey(releasePath))

		gh.files[releasePath] = []byte(releaseYAML("someone-else", shaA, true))
		_, release = reconcileOnce(key)
		Expect(release.Status.AutoDeploy.Reason).To(Equal(autoDeployReleaseFileNotFound))
		Expect(string(gh.files[releasePath])).To(Equal(releaseYAML("someone-else", shaA, true)))
	})

	It("makes no commit, no file reads and no status write when nothing changed", func() {
		key := createRelease("orders-dev", ns, shaA)
		gh.files[releasePath] = []byte(releaseYAML("orders-dev", shaA, true))
		gh.latestRun = runFor(shaA)
		_, first := reconcileOnce(key)
		commits, reads := len(gh.commits), gh.fileReads

		result, second := reconcileOnce(key)
		Expect(result.RequeueAfter).To(Equal(42 * time.Second))
		Expect(gh.commits).To(HaveLen(commits))
		Expect(gh.fileReads).To(Equal(reads),
			"an idle poll must not read application-repositories, only ask GitHub for the latest run")
		Expect(second.ResourceVersion).To(Equal(first.ResourceVersion),
			"a poll that finds nothing new must not bump resourceVersion, or the controller would reconcile itself in a loop")
	})

	It("writes nothing to git for a Release that is being deleted", func() {
		key := createRelease("orders-dev", ns, shaA)
		gh.files[releasePath] = []byte(releaseYAML("orders-dev", shaA, true))
		gh.latestRun = runFor(shaB)

		release := &platformv1alpha1.Release{}
		Expect(k8sClient.Get(ctx, key, release)).To(Succeed())
		release.Finalizers = []string{"test.platform.taskapp.io/hold"}
		Expect(k8sClient.Update(ctx, release)).To(Succeed())
		Expect(k8sClient.Delete(ctx, release)).To(Succeed())

		reconcileOnce(key)
		Expect(gh.runQueries).To(BeEmpty())
		Expect(gh.commits).To(BeEmpty())

		Expect(k8sClient.Get(ctx, key, release)).To(Succeed())
		release.Finalizers = nil
		Expect(k8sClient.Update(ctx, release)).To(Succeed())
	})

	It("leaves a pinned Release unchanged: deploys spec.version, never polls, never requeues", func() {
		release := &platformv1alpha1.Release{
			ObjectMeta: metav1.ObjectMeta{Name: "orders-dev-pinned", Namespace: ns},
			Spec: platformv1alpha1.ReleaseSpec{
				ComponentRef: platformv1alpha1.ComponentReference{Name: component},
				Environment:  ns,
				Version:      shaA,
			},
		}
		Expect(k8sClient.Create(ctx, release)).To(Succeed())
		DeferCleanup(func() { Expect(k8sClient.Delete(ctx, release)).To(Succeed()) })

		result, got := reconcileOnce(types.NamespacedName{Name: release.Name, Namespace: ns})
		Expect(result.RequeueAfter).To(BeZero())
		Expect(gh.runQueries).To(BeEmpty())
		Expect(gh.commits).To(HaveLen(1))
		Expect(gh.commits[0].message).To(Equal("Release orders-dev-pinned: sync orders/dev"))
		Expect(string(gh.files[valuesPath])).To(Equal("image:\n    tag: " + shaA + "\n"))
		Expect(got.Status.AutoDeploy).To(BeNil())
	})
})

var _ = Describe("releaseFile", func() {
	It("sets spec.version and leaves every other line as it was", func() {
		file, err := parseReleaseFile([]byte(releaseYAML("orders-dev", shaA, true)))
		Expect(err).NotTo(HaveOccurred())
		Expect(file.name).To(Equal("orders-dev"))
		Expect(file.branch).To(Equal("main"))
		Expect(file.version).To(Equal(shaA))
		out, err := file.withVersion(shaB)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(out)).To(Equal(releaseYAML("orders-dev", shaB, true)))
	})

	It("adds a missing version right after environment", func() {
		file, err := parseReleaseFile([]byte(releaseYAML("orders-dev", "", true)))
		Expect(err).NotTo(HaveOccurred())
		out, err := file.withVersion(shaA)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(out)).To(Equal(releaseYAML("orders-dev", shaA, true)))
	})

	It("rejects files that aren't a Release", func() {
		_, err := parseReleaseFile([]byte("kind: Database\nspec: {}\n"))
		Expect(err).To(HaveOccurred())
		_, err = parseReleaseFile([]byte(":\n- ["))
		Expect(err).To(HaveOccurred())
	})
})

var _ = Describe("parseGitHubRepo", func() {
	It("splits a Component clone URL into owner and repo", func() {
		owner, repo, err := parseGitHubRepo("https://github.com/entr0pian/orders.git")
		Expect(err).NotTo(HaveOccurred())
		Expect(owner).To(Equal("entr0pian"))
		Expect(repo).To(Equal("orders"))
	})

	It("rejects anything that isn't a github.com owner/repo URL", func() {
		for _, url := range []string{"https://gitlab.com/a/b.git", "https://github.com/a.git", "https://github.com/a/b/c.git"} {
			_, _, err := parseGitHubRepo(url)
			Expect(err).To(HaveOccurred(), url)
		}
	})
})

func ignoreNotFound(err error) error {
	if errors.IsNotFound(err) {
		return nil
	}
	return err
}
