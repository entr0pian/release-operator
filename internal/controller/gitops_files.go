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
	"fmt"

	"gopkg.in/yaml.v3"

	platformv1alpha1 "github.com/entr0pian/release-operator/api/v1alpha1"
)

// defaultTargetRevision and defaultChartPath are hardcoded for now.
// RUNTIME_DEPENDENCIES.md's design has these come from the Component's own
// chart metadata eventually; today Component only exposes
// status.repository.url, so these two stay fixed until that's extended.
const (
	defaultTargetRevision = "main"
	defaultChartPath      = "chart"
)

// environmentsFile is components/<component>/environments/<env>.yaml.
// Field order matters here — this is marshaled with yaml.v3, which
// (unlike sigs.k8s.io/yaml's JSON round-trip) preserves struct field order
// instead of sorting keys, so the committed file reads the same as every
// hand-written one in application-repositories.
type environmentsFile struct {
	Component   string       `yaml:"component"`
	Environment string       `yaml:"environment"`
	Namespace   string       `yaml:"namespace"`
	Source      sourceFields `yaml:"source"`
}

type sourceFields struct {
	RepoURL        string `yaml:"repoURL"`
	TargetRevision string `yaml:"targetRevision"`
	ChartPath      string `yaml:"chartPath"`
}

// valuesFile is components/<component>/values/<env>.yaml.
type valuesFile struct {
	Image    imageFields     `yaml:"image"`
	Database *databaseValues `yaml:"database,omitempty"`
}

type imageFields struct {
	Tag string `yaml:"tag"`
}

type databaseValues struct {
	Enabled    bool   `yaml:"enabled"`
	SecretName string `yaml:"secretName"`
}

// buildEnvironmentsFile renders components/<component>/environments/<env>.yaml.
// namespace is the Release's own namespace — see release_controller.go's
// doc comment on why Release, its Database, and the deployed workload all
// share one namespace per environment.
func buildEnvironmentsFile(release *platformv1alpha1.Release, namespace, repoURL string) ([]byte, error) {
	return marshalYAML(environmentsFile{
		Component:   release.Spec.ComponentRef.Name,
		Environment: release.Spec.Environment,
		Namespace:   namespace,
		Source: sourceFields{
			RepoURL:        repoURL,
			TargetRevision: defaultTargetRevision,
			ChartPath:      defaultChartPath,
		},
	})
}

// buildValuesFile renders components/<component>/values/<env>.yaml.
// secretName is the empty string when the database binding isn't enabled.
func buildValuesFile(release *platformv1alpha1.Release, secretName string) ([]byte, error) {
	vf := valuesFile{Image: imageFields{Tag: release.Spec.Version}}
	if db := release.Spec.Bindings.Database; db != nil {
		vf.Database = &databaseValues{Enabled: db.Enabled, SecretName: secretName}
	}
	return marshalYAML(vf)
}

func marshalYAML(v any) ([]byte, error) {
	out, err := yaml.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("marshaling yaml: %w", err)
	}
	return out, nil
}

func environmentsFilePath(componentName, environment string) string {
	return fmt.Sprintf("components/%s/environments/%s.yaml", componentName, environment)
}

func valuesFilePath(componentName, environment string) string {
	return fmt.Sprintf("components/%s/values/%s.yaml", componentName, environment)
}
