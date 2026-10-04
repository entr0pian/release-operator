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
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	platformv1alpha1 "github.com/entr0pian/release-operator/api/v1alpha1"
)

var _ = Describe("GitOps files", func() {
	release := &platformv1alpha1.Release{
		Spec: platformv1alpha1.ReleaseSpec{
			ComponentRef: platformv1alpha1.ComponentReference{Name: "payments"},
			Environment:  "dev",
			Version:      "e6fefbea969f8f4a2ce6b4bb87a26dadddb18e7e",
		},
	}

	It("pins the chart to the same commit as the image", func() {
		env, err := buildEnvironmentsFile(release, "dev", "https://github.com/entr0pian/payments.git", release.Spec.Version)
		Expect(err).NotTo(HaveOccurred())
		Expect(string(env)).To(Equal(`component: payments
environment: dev
namespace: dev
source:
    repoURL: https://github.com/entr0pian/payments.git
    targetRevision: e6fefbea969f8f4a2ce6b4bb87a26dadddb18e7e
    chartPath: chart
`))

		values, err := buildValuesFile(release.Spec.Version, resolvedDatabaseBinding{})
		Expect(err).NotTo(HaveOccurred())
		Expect(string(values)).To(Equal(`image:
    tag: e6fefbea969f8f4a2ce6b4bb87a26dadddb18e7e
`))
	})
})
