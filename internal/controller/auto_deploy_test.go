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
// (application-repositories), plus the CI runs LatestSuccessfulRun reports.
type fakeGitHub struct {
	files      map[string][]byte
	commits    []fakeCommit
	latestRun  *workflowRun
	runQueries []string
	fileReads  int
}

type fakeCommit struct {
	message string
	files   map[string][]byte
}

func newFakeGitHub() *fakeGitHub {
	return &fakeGitHub{files: map[string][]byte{}}
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

const (
	shaA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	shaB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

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

	createRelease := func(name, env string, spec func(*platformv1alpha1.ReleaseSpec)) types.NamespacedName {
		release := &platformv1alpha1.Release{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: env},
			Spec: platformv1alpha1.ReleaseSpec{
				ComponentRef: platformv1alpha1.ComponentReference{Name: component},
				Environment:  env,
			},
		}
		spec(&release.Spec)
		Expect(k8sClient.Create(ctx, release)).To(Succeed())
		DeferCleanup(func() { Expect(k8sClient.Delete(ctx, release)).To(Succeed()) })
		return types.NamespacedName{Name: name, Namespace: env}
	}
	autoDeploy := func(s *platformv1alpha1.ReleaseSpec) {
		s.AutoDeploy = &platformv1alpha1.AutoDeploySpec{Enabled: true}
	}

	reconcileOnce := func(key types.NamespacedName) (reconcile.Result, *platformv1alpha1.Release) {
		result, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		release := &platformv1alpha1.Release{}
		Expect(k8sClient.Get(ctx, key, release)).To(Succeed())
		return result, release
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

		It("rejects a Release with both version and autoDeploy.enabled", func() {
			release := newRelease("both")
			release.Spec.Version = shaA
			release.Spec.AutoDeploy = &platformv1alpha1.AutoDeploySpec{Enabled: true}
			err := k8sClient.Create(ctx, release)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("set exactly one of version or autoDeploy.enabled"))
		})

		It("rejects a Release with neither version nor autoDeploy.enabled", func() {
			release := newRelease("neither")
			release.Spec.AutoDeploy = &platformv1alpha1.AutoDeploySpec{Enabled: false}
			err := k8sClient.Create(ctx, release)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("set exactly one of version or autoDeploy.enabled"))
		})
	})

	It("refuses auto-deploy outside the allowed environments, without calling GitHub", func() {
		key := createRelease("orders-prod", "prod", autoDeploy)
		result, release := reconcileOnce(key)

		Expect(result.RequeueAfter).To(BeZero())
		cond := findReadyCondition(release)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Reason).To(Equal("AutoDeployNotAllowed"))
		Expect(gh.runQueries).To(BeEmpty())
		Expect(gh.commits).To(BeEmpty())
	})

	It("waits for the first successful build instead of failing", func() {
		key := createRelease("orders-dev-wait", ns, autoDeploy)
		result, release := reconcileOnce(key)

		Expect(result.RequeueAfter).To(Equal(42 * time.Second))
		Expect(findReadyCondition(release).Reason).To(Equal("AwaitingFirstBuild"))
		Expect(gh.runQueries).To(ConsistOf("entr0pian/orders ci.yaml@main"))
		Expect(gh.commits).To(BeEmpty())
	})

	It("deploys the latest successful run directly to application-repositories and records it in status", func() {
		gh.latestRun = &workflowRun{HeadSHA: shaA, RunNumber: 7, HTMLURL: "https://github.com/entr0pian/orders/actions/runs/7"}
		key := createRelease("orders-dev", ns, autoDeploy)
		result, release := reconcileOnce(key)

		Expect(result.RequeueAfter).To(Equal(42 * time.Second))
		Expect(findReadyCondition(release).Reason).To(Equal("Synced"))
		Expect(gh.commits).To(HaveLen(1))
		Expect(gh.commits[0].message).To(ContainSubstring("auto-deploy orders/dev aaaaaaa"))
		Expect(gh.commits[0].message).To(ContainSubstring("actions/runs/7"))
		Expect(string(gh.files["components/orders/values/dev.yaml"])).To(Equal("image:\n    tag: " + shaA + "\n"))
		Expect(string(gh.files["components/orders/environments/dev.yaml"])).To(ContainSubstring("targetRevision: " + shaA))

		Expect(release.Spec.Version).To(BeEmpty(), "the operator must never write the version into spec")
		Expect(release.Status.AutoDeploy).NotTo(BeNil())
		Expect(release.Status.AutoDeploy.DeployedVersion).To(Equal(shaA))
		Expect(release.Status.AutoDeploy.RunNumber).To(Equal(int64(7)))
		Expect(release.Status.AutoDeploy.DeployedAt).NotTo(BeNil())

		By("rolling forward when a newer run succeeds")
		gh.latestRun = &workflowRun{HeadSHA: shaB, RunNumber: 8, HTMLURL: "https://github.com/entr0pian/orders/actions/runs/8"}
		_, release = reconcileOnce(key)
		Expect(gh.commits).To(HaveLen(2))
		Expect(string(gh.files["components/orders/values/dev.yaml"])).To(Equal("image:\n    tag: " + shaB + "\n"))
		Expect(release.Status.AutoDeploy.DeployedVersion).To(Equal(shaB))

		By("never moving back to an older run")
		gh.latestRun = &workflowRun{HeadSHA: shaA, RunNumber: 7, HTMLURL: "https://github.com/entr0pian/orders/actions/runs/7"}
		_, release = reconcileOnce(key)
		Expect(gh.commits).To(HaveLen(2))
		Expect(release.Status.AutoDeploy.DeployedVersion).To(Equal(shaB))
	})

	It("makes no commit, no file reads and no status write when nothing changed", func() {
		gh.latestRun = &workflowRun{HeadSHA: shaA, RunNumber: 3, HTMLURL: "https://github.com/entr0pian/orders/actions/runs/3"}
		key := createRelease("orders-dev-idle", ns, autoDeploy)
		_, first := reconcileOnce(key)

		readsAfterFirst := gh.fileReads
		result, second := reconcileOnce(key)
		Expect(result.RequeueAfter).To(Equal(42 * time.Second))
		Expect(gh.commits).To(HaveLen(1))
		Expect(gh.fileReads).To(Equal(readsAfterFirst),
			"an idle poll must not read application-repositories, only ask GitHub for the latest run")
		Expect(second.ResourceVersion).To(Equal(first.ResourceVersion),
			"a poll that finds nothing new must not bump resourceVersion, or the controller would reconcile itself in a loop")
	})

	It("leaves a pinned Release unchanged: deploys spec.version, never polls, never requeues", func() {
		key := createRelease("orders-dev-pinned", ns, func(s *platformv1alpha1.ReleaseSpec) { s.Version = shaA })
		result, release := reconcileOnce(key)

		Expect(result.RequeueAfter).To(BeZero())
		Expect(gh.runQueries).To(BeEmpty())
		Expect(gh.commits).To(HaveLen(1))
		Expect(gh.commits[0].message).To(Equal("Release orders-dev-pinned: sync orders/dev"))
		Expect(string(gh.files["components/orders/values/dev.yaml"])).To(Equal("image:\n    tag: " + shaA + "\n"))
		Expect(release.Status.AutoDeploy).To(BeNil())
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
