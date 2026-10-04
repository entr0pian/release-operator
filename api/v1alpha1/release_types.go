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

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// ComponentReference is the authoritative link from a related resource back
// to its owning Component, per PLATFORM_API_ARCHITECTURE.md's componentRef
// pattern — reused here exactly as GitHubRepository and Database already do.
type ComponentReference struct {
	// name of the Component this resource belongs to.
	// +required
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
}

// DatabaseBinding declares whether the database runtime binding is enabled
// for this Release, and which Database resource it belongs to.
type DatabaseBinding struct {
	// enabled turns the database binding on for this environment. The
	// scaffolded chart always ships with database support present but
	// disabled — this is what flips it on, per environment.
	// +optional
	Enabled bool `json:"enabled,omitempty"`

	// ref names the Database CR this binding's connection Secret belongs
	// to. Required when enabled is true.
	// +optional
	Ref string `json:"ref,omitempty"`
}

// ReleaseBindings declares this Release's runtime dependency bindings. Each
// binding type is its own typed field, not a generic map — deliberately, so
// each binding's resolution logic and Secret key contract stays explicit and
// reviewable per binding rather than dispatched from an arbitrary string.
type ReleaseBindings struct {
	// database binding — see DatabaseBinding.
	// +optional
	Database *DatabaseBinding `json:"database,omitempty"`
}

// AutoDeploySpec makes a Release follow its component's main branch instead
// of a pinned version: every successful CI run on main is deployed as it
// lands, with no PR per deploy.
type AutoDeploySpec struct {
	// enabled turns auto-deploy on. Mutually exclusive with spec.version, and
	// only honoured in the environments the operator is started with
	// (--auto-deploy-environments, "dev" by default).
	// +optional
	Enabled bool `json:"enabled,omitempty"`
}

// ReleaseSpec defines the desired state of Release
// +kubebuilder:validation:XValidation:rule="(has(self.version) && size(self.version) > 0) != (has(self.autoDeploy) && has(self.autoDeploy.enabled) && self.autoDeploy.enabled)",message="set exactly one of version or autoDeploy.enabled"
type ReleaseSpec struct {
	// componentRef is the authoritative reference to the owning Component.
	// +required
	ComponentRef ComponentReference `json:"componentRef"`

	// environment this Release targets, e.g. "dev" or "prod" — must match
	// one of ArgoCD's registered clusters' environment label.
	// +required
	// +kubebuilder:validation:MinLength=1
	Environment string `json:"environment"`

	// version is the commit of the component's repository to deploy: used
	// both as the image tag and as the revision the chart is read from, so
	// it must be a git ref whose image exists (CI tags images with the full
	// commit SHA). Required unless autoDeploy.enabled is true, in which case
	// it must be left unset: the operator picks the version itself.
	// +optional
	// +kubebuilder:validation:MinLength=1
	Version string `json:"version,omitempty"`

	// autoDeploy, when enabled, deploys the latest successful CI build on the
	// component's main branch instead of a pinned version — see AutoDeploySpec.
	// +optional
	AutoDeploy *AutoDeploySpec `json:"autoDeploy,omitempty"`

	// bindings declares which runtime dependencies are enabled for this
	// component/environment, and which resource to resolve each from.
	// +optional
	Bindings ReleaseBindings `json:"bindings,omitempty"`
}

// AutoDeployStatus records what auto-deploy last deployed. It lives in status,
// never spec: the Release manifest is applied from git by Argo CD, so writing
// the version back into spec would be reverted (or left OutOfSync).
type AutoDeployStatus struct {
	// deployedVersion is the commit SHA last written to application-repositories.
	// +optional
	DeployedVersion string `json:"deployedVersion,omitempty"`

	// runNumber is the CI workflow run that built deployedVersion. Auto-deploy
	// only ever moves to a higher run number, never back.
	// +optional
	RunNumber int64 `json:"runNumber,omitempty"`

	// runURL links to that CI run.
	// +optional
	RunURL string `json:"runURL,omitempty"`

	// deployedAt is when deployedVersion was written.
	// +optional
	DeployedAt *metav1.Time `json:"deployedAt,omitempty"`
}

// ReleaseStatus defines the observed state of Release.
type ReleaseStatus struct {
	// observedGeneration is the most recent spec generation the controller
	// has reconciled.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// conditions represent the current state of the Release resource.
	// Each condition has a unique type and reflects the status of a specific aspect of the resource.
	//
	// Standard condition types include:
	// - "Available": the resource is fully functional
	// - "Progressing": the resource is being created or updated
	// - "Degraded": the resource failed to reach or maintain its desired state
	//
	// The status of each condition is one of True, False, or Unknown.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// autoDeploy is set while spec.autoDeploy is enabled — see AutoDeployStatus.
	// +optional
	AutoDeploy *AutoDeployStatus `json:"autoDeploy,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// Release is the Schema for the releases API
type Release struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of Release
	// +required
	Spec ReleaseSpec `json:"spec"`

	// status defines the observed state of Release
	// +optional
	Status ReleaseStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// ReleaseList contains a list of Release
type ReleaseList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []Release `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Release{}, &ReleaseList{})
}
