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

// AutoDeploySpec makes a Release follow a branch of its component's
// repository: whenever a newer commit on it has a successful CI build, the
// operator commits that commit as spec.version to this Release's file in
// application-repositories, and Argo CD applies it like any other change.
type AutoDeploySpec struct {
	// branch is the branch to follow, normally main. Only honoured in the
	// environments the operator is started with (--auto-deploy-environments,
	// "dev" by default).
	// +required
	// +kubebuilder:validation:MinLength=1
	Branch string `json:"branch"`
}

// ReleaseSpec defines the desired state of Release
// +kubebuilder:validation:XValidation:rule="has(self.version) || has(self.autoDeploy)",message="set version, autoDeploy, or both"
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
	// commit SHA). Optional only with autoDeploy, which sets it (in git)
	// once the branch has its first successful build, and moves it forward
	// after that.
	// +optional
	// +kubebuilder:validation:MinLength=1
	Version string `json:"version,omitempty"`

	// autoDeploy, when set, keeps version at the newest successfully built
	// commit on a branch — see AutoDeploySpec.
	// +optional
	AutoDeploy *AutoDeploySpec `json:"autoDeploy,omitempty"`

	// bindings declares which runtime dependencies are enabled for this
	// component/environment, and which resource to resolve each from.
	// +optional
	Bindings ReleaseBindings `json:"bindings,omitempty"`
}

// AutoDeployStatus reports what auto-deploy last found, for Backstage to
// show. Deliberately no timestamp: every status write triggers another
// reconcile, so a "last checked" time would make an idle Release reconcile
// itself in a loop.
type AutoDeployStatus struct {
	// branch is the branch being followed (spec.autoDeploy.branch).
	// +optional
	Branch string `json:"branch,omitempty"`

	// latestDeployable is the newest commit on branch with a successful CI
	// build, or empty before the first one.
	// +optional
	LatestDeployable string `json:"latestDeployable,omitempty"`

	// runURL links to the CI run that built latestDeployable.
	// +optional
	RunURL string `json:"runURL,omitempty"`

	// reason is one of UpToDate, Deploying (version committed, waiting for
	// Argo CD to apply it), WaitingForFirstBuild, NotFastForward,
	// ReleaseFileNotFound.
	// +optional
	Reason string `json:"reason,omitempty"`

	// message explains reason.
	// +optional
	Message string `json:"message,omitempty"`
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

	// autoDeploy is set while spec.autoDeploy is — see AutoDeployStatus.
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
