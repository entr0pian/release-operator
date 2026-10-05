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
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"net/http"

	"github.com/google/go-github/v88/github"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// tokenRequest records how a stand-in installation transport was built; the
// tests never contact GitHub.
type tokenRequest struct {
	appID, installationID int64
	opts                  *github.InstallationTokenOptions
}

var _ = Describe("GitHub App authentication", func() {
	const appNS = "release-operator-system"
	secretName := types.NamespacedName{Namespace: appNS, Name: "release-operator-github-app"}

	var (
		requests []tokenRequest
		source   *appSource
	)

	privateKey := func() []byte {
		key, err := rsa.GenerateKey(rand.Reader, 2048)
		Expect(err).NotTo(HaveOccurred())
		return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	}

	writeSecret := func(data map[string][]byte) {
		secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: secretName.Name, Namespace: secretName.Namespace}}
		err := k8sClient.Get(ctx, secretName, secret)
		if errors.IsNotFound(err) {
			secret.Data = data
			Expect(k8sClient.Create(ctx, secret)).To(Succeed())
			return
		}
		Expect(err).NotTo(HaveOccurred())
		secret.Data = data
		Expect(k8sClient.Update(ctx, secret)).To(Succeed())
	}

	validSecret := func() map[string][]byte {
		return map[string][]byte{appIDKey: []byte("123"), installationIDKey: []byte("456"), privateKeyKey: privateKey()}
	}

	BeforeEach(func() {
		err := k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: appNS}})
		if err != nil && !errors.IsAlreadyExists(err) {
			Expect(err).NotTo(HaveOccurred())
		}
		_ = k8sClient.Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: secretName.Name, Namespace: secretName.Namespace}})

		requests = nil
		source = &appSource{
			reader: k8sClient,
			secret: secretName,
			owner:  "entr0pian",
			newTransport: func(appID, installationID int64, _ []byte, opts *github.InstallationTokenOptions) (http.RoundTripper, error) {
				requests = append(requests, tokenRequest{appID, installationID, opts})
				return http.DefaultTransport, nil
			},
		}
	})

	It("scopes the write token to application-repositories and the other to reading", func() {
		writeSecret(validSecret())

		_, owner, err := source.clientFor(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(owner).To(Equal("entr0pian"))

		Expect(requests).To(HaveLen(2))
		for _, r := range requests {
			Expect(r.appID).To(Equal(int64(123)))
			Expect(r.installationID).To(Equal(int64(456)))
		}
		Expect(requests[0].opts.Repositories).To(Equal([]string{applicationRepositoriesRepo}))
		Expect(requests[0].opts.Permissions).To(Equal(&github.InstallationPermissions{Contents: github.Ptr("write")}))
		Expect(requests[1].opts.Repositories).To(BeEmpty())
		Expect(requests[1].opts.Permissions).To(Equal(&github.InstallationPermissions{Actions: github.Ptr("read"), Contents: github.Ptr("read")}))
	})

	It("reuses its clients until the Secret changes", func() {
		writeSecret(validSecret())
		first, _, err := source.clientFor(ctx)
		Expect(err).NotTo(HaveOccurred())
		again, _, err := source.clientFor(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(again).To(BeIdenticalTo(first))
		Expect(requests).To(HaveLen(2), "no new transports, so no new tokens, for an unchanged Secret")

		writeSecret(validSecret())
		rotated, _, err := source.clientFor(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(rotated).NotTo(BeIdenticalTo(first))
		Expect(requests).To(HaveLen(4))
	})

	It("fails, without falling back to the PAT, when the Secret is missing", func() {
		_, _, err := source.clientFor(ctx)
		Expect(err).To(MatchError(ContainSubstring("reading GitHub App credentials secret release-operator-system/release-operator-github-app")))
		Expect(requests).To(BeEmpty())
	})

	It("rejects a Secret with a missing or malformed key", func() {
		data := validSecret()
		delete(data, privateKeyKey)
		writeSecret(data)
		_, _, err := source.clientFor(ctx)
		Expect(err).To(MatchError(ContainSubstring(`no "privateKey" key`)))

		data = validSecret()
		data[appIDKey] = []byte("abc")
		writeSecret(data)
		_, _, err = source.clientFor(ctx)
		Expect(err).To(MatchError(ContainSubstring(`"appId" is not a number`)))
	})

	It("needs the owner the App is installed on", func() {
		writeSecret(validSecret())
		source.owner = ""
		_, _, err := source.clientFor(ctx)
		Expect(err).To(MatchError(ContainSubstring("--github-owner")))
	})

	It("is the only source a reconciler without credentials would use", func() {
		_, _, err := (&ReleaseReconciler{}).githubClientFor(ctx)
		Expect(err).To(MatchError("no GitHub credentials configured"))
	})
})

var _ = Describe("repoRoutingClient", func() {
	It("sends application-repositories calls to the write client and the rest to the read client", func() {
		appRepos, services := newFakeGitHub(), newFakeGitHub()
		c := &repoRoutingClient{applicationRepositories: appRepos, services: services}

		_, err := c.CommitFiles(context.Background(), "entr0pian", applicationRepositoriesRepo, "main", "m", map[string][]byte{"f": []byte("x")}, "p", "t")
		Expect(err).NotTo(HaveOccurred())
		_, err = c.LatestSuccessfulRun(context.Background(), "entr0pian", "orders", "ci.yaml", "main")
		Expect(err).NotTo(HaveOccurred())

		Expect(appRepos.commits).To(HaveLen(1))
		Expect(services.commits).To(BeEmpty())
		Expect(services.runQueries).To(ConsistOf("entr0pian/orders ci.yaml@main"))
		Expect(appRepos.runQueries).To(BeEmpty())
	})
})
