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
// for this Release, and which Database resource to resolve its connection
// Secret from. See RUNTIME_DEPENDENCIES.md's Database Connection Secret
// Contract for the resolution chain this backs
// (ref -> Database CR -> status.connectionSecretRef -> values secretName).
type DatabaseBinding struct {
	// enabled turns the database binding on for this environment. The
	// scaffolded chart always ships with database support present but
	// disabled — this is what flips it on, per environment.
	// +optional
	Enabled bool `json:"enabled,omitempty"`

	// ref names the Database CR, in the same namespace as this Release, to
	// resolve the connection Secret from. Required when enabled is true;
	// the controller must resolve it via Database.status.connectionSecretRef
	// and never reconstruct the Secret name from ref itself.
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

// ReleaseSpec defines the desired state of Release
type ReleaseSpec struct {
	// componentRef is the authoritative reference to the owning Component.
	// +required
	ComponentRef ComponentReference `json:"componentRef"`

	// environment this Release targets, e.g. "dev" or "prod" — must match
	// one of ArgoCD's registered clusters' environment label.
	// +required
	// +kubebuilder:validation:MinLength=1
	Environment string `json:"environment"`

	// version is the component's image tag to deploy.
	// +required
	// +kubebuilder:validation:MinLength=1
	Version string `json:"version"`

	// bindings declares which runtime dependencies are enabled for this
	// component/environment, and which resource to resolve each from.
	// +optional
	Bindings ReleaseBindings `json:"bindings,omitempty"`
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
