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
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	platformv1alpha1 "github.com/entr0pian/release-operator/api/v1alpha1"
)

const (
	// credentialsSecretName/Namespace mirror scaffold-operator's own
	// githubClientFor: the same shared Secret Crossplane's GitHub provider
	// already reads, rather than a second GitHub credential this operator
	// would have to be issued and rotated separately.
	credentialsSecretName      = "crossplane-github-credentials"
	credentialsSecretNamespace = "crossplane-system"

	// applicationRepositoriesRepo is the one repository this controller
	// ever writes to — unlike scaffold-operator, which writes into
	// whichever repo a ScaffoldRequest names.
	applicationRepositoriesRepo = "application-repositories"
	applicationRepositoriesRef  = "main"

	// componentNamespace is the single, fixed namespace every Component
	// lives in, regardless of which namespace the Releases referencing it
	// are in. Component is a global identity (one GitHub repo, one scaffold
	// status) shared across every environment, so — unlike Database, which
	// is genuinely per-environment and therefore looked up in
	// release.Namespace — it must never be resolved relative to the
	// Release doing the looking up, or the same Component would need to be
	// duplicated into every environment namespace.
	componentNamespace = "platform"

	readyConditionType = "Ready"

	// exportNameConnection is the one Database export name this controller
	// resolves — Database v1 always publishes its connection bundle under
	// this fixed export name.
	exportNameConnection = "connection"
	exportTypeSecret     = "Secret"

	// awsSecretsManagerExportProvider is the only status.exports[].location.provider
	// value this controller currently understands how to translate into a
	// generic workload binding — see genericProviderFor.
	awsSecretsManagerExportProvider = "AWSSecretsManager"

	// genericBindingTypeSecret/genericProviderAWSSecretsManager are the
	// values.yaml-facing vocabulary the workload chart consumes, never the
	// Database-specific "AWSSecretsManager" export enum above.
	genericBindingTypeSecret         = "secret"
	genericProviderAWSSecretsManager = "aws-secrets-manager"

	// ciWorkflowFile/ciBranch are what auto-deploy watches on a component's
	// repository: the platform-scaffolds golang-service CI workflow, which
	// builds and pushes the commit-SHA-tagged image on every push to main.
	// Backstage's Version picker reads the same workflow.
	ciWorkflowFile = "ci.yaml"
	ciBranch       = "main"

	// DefaultAutoDeployPollInterval is how often an auto-deploy Release
	// checks for a newer successful CI run when none is configured.
	DefaultAutoDeployPollInterval = 60 * time.Second
)

// DefaultAutoDeployEnvironments is where auto-deploy is allowed when the
// reconciler isn't configured otherwise.
var DefaultAutoDeployEnvironments = []string{"dev"}

// componentGVK is read as unstructured, not as a typed Go struct: Component
// (component-operator) is owned by a different repository, and importing
// its types here would recreate exactly the cross-project coupling
// componentRef is meant to avoid — the same reason backend-operator reads
// AtlasSchema (also foreign, db.atlasgo.io) as unstructured rather than
// vendoring ariga's types.
var componentGVK = schema.GroupVersionKind{Group: "platform.taskapp.io", Version: "v1alpha1", Kind: "Component"}

// databaseGVK is read as unstructured for the same reason as componentGVK:
// Database is a Crossplane composite resource owned by crossplane-compositions
// (apis/database), not a type this repository should vendor.
var databaseGVK = schema.GroupVersionKind{Group: "database.taskapp.io", Version: "v1alpha1", Kind: "Database"}

// ReleaseReconciler reconciles a Release object
type ReleaseReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// APIReader is a direct, uncached read from the API server
	// (mgr.GetAPIReader()) — used only for the crossplane-github-credentials
	// Secret. The cached client.Client lazily starts a cluster-wide
	// list/watch informer the first time any type is Get'd through it,
	// which this operator's RBAC (deliberately narrow: get on one named
	// Secret) doesn't grant. APIReader needs only "get".
	APIReader client.Reader

	// NewGitHubClient overrides how a githubClient is constructed from a
	// token, for tests. Defaults to newGoGithubClient.
	NewGitHubClient func(token string) githubClient

	// AutoDeployEnvironments lists the environments a Release may enable
	// spec.autoDeploy in. Defaults to DefaultAutoDeployEnvironments. This is
	// enforced here, not only in Backstage's form, so a hand-written PR
	// can't turn on auto-deploy for prod.
	AutoDeployEnvironments []string

	// AutoDeployPollInterval is how often an auto-deploy Release is
	// re-reconciled to look for a newer CI run. Defaults to
	// DefaultAutoDeployPollInterval.
	AutoDeployPollInterval time.Duration
}

// +kubebuilder:rbac:groups=platform.taskapp.io,resources=releases,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=platform.taskapp.io,resources=releases/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=platform.taskapp.io,resources=releases/finalizers,verbs=update
// +kubebuilder:rbac:groups=platform.taskapp.io,resources=components,verbs=get;list;watch
// +kubebuilder:rbac:groups=database.taskapp.io,resources=databases,verbs=get;list;watch

// Reconcile resolves a Release's componentRef and bindings into
// components/<component>/{environments,values}/<environment>.yaml in
// application-repositories, and commits both atomically — see
// RUNTIME_DEPENDENCIES.md's "Release CR" / "GitOps file layout" sections.
//
// Release, the Database CRs it references, and the workload's own namespace
// are all assumed to be the same namespace (release.Namespace) — the same
// convention Database already follows (its connection Secret lands in "the
// workload's own namespace"), and typically one namespace per environment
// (e.g. "dev", "prod"). Component is the one exception: it's a single
// cross-environment identity, not a per-environment resource, so it's
// always resolved from the fixed componentNamespace instead — see
// resolveComponent.
func (r *ReleaseReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	release := &platformv1alpha1.Release{}
	if err := r.Get(ctx, req.NamespacedName, release); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	autoDeploy := autoDeployEnabled(release)
	// waiting is how soon to look again when something isn't ready yet; an
	// auto-deploy Release keeps polling for new builds even once synced.
	waiting := ctrl.Result{RequeueAfter: 15 * time.Second}
	synced := ctrl.Result{}
	if autoDeploy {
		synced = ctrl.Result{RequeueAfter: r.pollInterval()}
		if !slices.Contains(r.autoDeployEnvironments(), release.Spec.Environment) {
			// A spec change is what fixes this, and that triggers a reconcile
			// on its own — nothing to requeue for.
			r.setReady(release, metav1.ConditionFalse, "AutoDeployNotAllowed",
				fmt.Sprintf("auto-deploy is not allowed in environment %q (allowed: %s)", release.Spec.Environment, strings.Join(r.autoDeployEnvironments(), ", ")))
			release.Status.AutoDeploy = nil
			return ctrl.Result{}, r.Status().Update(ctx, release)
		}
	} else {
		release.Status.AutoDeploy = nil
	}

	repoURL, ready, err := r.resolveComponent(ctx, release)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !ready {
		return waiting, r.Status().Update(ctx, release)
	}

	dbBinding, ready, err := r.resolveDatabaseBinding(ctx, release)
	if err != nil {
		r.setReady(release, metav1.ConditionFalse, "DatabaseBindingInvalid", err.Error())
		_ = r.Status().Update(ctx, release)
		return ctrl.Result{}, err
	}
	if !ready {
		return waiting, r.Status().Update(ctx, release)
	}

	version, run, ready, err := r.resolveVersion(ctx, release, repoURL)
	if err != nil {
		r.setReady(release, metav1.ConditionFalse, "VersionResolutionFailed", err.Error())
		_ = r.Status().Update(ctx, release)
		return ctrl.Result{}, err
	}
	if !ready {
		return synced, r.Status().Update(ctx, release)
	}
	if alreadyDeployed(release, version) {
		// The common auto-deploy poll: no new build and no spec change since
		// the last successful sync, so skip reading application-repositories
		// and keep an idle poll to the one GitHub call above.
		return synced, nil
	}

	envContent, err := buildEnvironmentsFile(release, release.Namespace, repoURL, version)
	if err != nil {
		return ctrl.Result{}, err
	}
	valuesContent, err := buildValuesFile(version, dbBinding)
	if err != nil {
		return ctrl.Result{}, err
	}

	if err := r.syncToGitOps(ctx, release, envContent, valuesContent, run); err != nil {
		r.setReady(release, metav1.ConditionFalse, "SyncFailed", err.Error())
		_ = r.Status().Update(ctx, release)
		return ctrl.Result{}, err
	}

	if run != nil && (release.Status.AutoDeploy == nil || release.Status.AutoDeploy.DeployedVersion != run.HeadSHA) {
		log.Info("auto-deployed new version", "component", release.Spec.ComponentRef.Name, "environment", release.Spec.Environment, "version", run.HeadSHA, "run", run.HTMLURL)
		now := metav1.Now()
		release.Status.AutoDeploy = &platformv1alpha1.AutoDeployStatus{
			DeployedVersion: run.HeadSHA,
			RunNumber:       run.RunNumber,
			RunURL:          run.HTMLURL,
			DeployedAt:      &now,
		}
	}

	log.Info("release synced", "component", release.Spec.ComponentRef.Name, "environment", release.Spec.Environment)
	message := "wrote environments/values to application-repositories"
	if autoDeploy {
		message = fmt.Sprintf("auto-deploy: %s, from %s", shortSHA(version), release.Status.AutoDeploy.RunURL)
	}
	r.setReady(release, metav1.ConditionTrue, "Synced", message)
	release.Status.ObservedGeneration = release.Generation
	// When nothing changed (the common auto-deploy poll), this status is
	// byte-identical to what's stored, so the API server treats the update
	// as a no-op: no new resourceVersion, no watch event, no reconcile loop.
	return synced, r.Status().Update(ctx, release)
}

// resolveVersion returns the commit to deploy. A pinned Release just uses
// spec.version (run is nil). An auto-deploy Release uses the newest
// successful CI run on the component's main branch — but never one older
// than what it already deployed, so a re-run or out-of-order listing can't
// roll it backwards. ready=false means there's no successful run yet; the
// condition was set and the caller should poll again, not treat this as an
// error.
func (r *ReleaseReconciler) resolveVersion(ctx context.Context, release *platformv1alpha1.Release, repoURL string) (string, *workflowRun, bool, error) {
	if !autoDeployEnabled(release) {
		return release.Spec.Version, nil, true, nil
	}

	owner, repo, err := parseGitHubRepo(repoURL)
	if err != nil {
		return "", nil, false, err
	}
	gh, _, err := r.githubClientFor(ctx)
	if err != nil {
		return "", nil, false, err
	}
	latest, err := gh.LatestSuccessfulRun(ctx, owner, repo, ciWorkflowFile, ciBranch)
	if err != nil {
		return "", nil, false, err
	}

	deployed := release.Status.AutoDeploy
	if latest == nil {
		if deployed != nil && deployed.DeployedVersion != "" {
			// Runs can age out of GitHub's listing; keep what's deployed.
			latest = &workflowRun{HeadSHA: deployed.DeployedVersion, RunNumber: deployed.RunNumber, HTMLURL: deployed.RunURL}
		} else {
			r.setReady(release, metav1.ConditionFalse, "AwaitingFirstBuild",
				fmt.Sprintf("no successful %s run on %s/%s %s yet", ciWorkflowFile, owner, repo, ciBranch))
			return "", nil, false, nil
		}
	}
	if deployed != nil && deployed.DeployedVersion != "" && latest.RunNumber < deployed.RunNumber {
		latest = &workflowRun{HeadSHA: deployed.DeployedVersion, RunNumber: deployed.RunNumber, HTMLURL: deployed.RunURL}
	}
	return latest.HeadSHA, latest, true, nil
}

// alreadyDeployed reports whether an auto-deploy Release last synced this
// exact version for its current spec, with nothing failing since.
func alreadyDeployed(release *platformv1alpha1.Release, version string) bool {
	deployed := release.Status.AutoDeploy
	ready := apimeta.FindStatusCondition(release.Status.Conditions, readyConditionType)
	return autoDeployEnabled(release) &&
		deployed != nil && deployed.DeployedVersion == version &&
		release.Status.ObservedGeneration == release.Generation &&
		ready != nil && ready.Status == metav1.ConditionTrue && ready.Reason == "Synced"
}

func autoDeployEnabled(release *platformv1alpha1.Release) bool {
	return release.Spec.AutoDeploy != nil && release.Spec.AutoDeploy.Enabled
}

func (r *ReleaseReconciler) autoDeployEnvironments() []string {
	if r.AutoDeployEnvironments == nil {
		return DefaultAutoDeployEnvironments
	}
	return r.AutoDeployEnvironments
}

func (r *ReleaseReconciler) pollInterval() time.Duration {
	if r.AutoDeployPollInterval <= 0 {
		return DefaultAutoDeployPollInterval
	}
	return r.AutoDeployPollInterval
}

// parseGitHubRepo splits a Component's clone URL
// (https://github.com/<owner>/<repo>.git, see resolveComponent) into owner
// and repo.
func parseGitHubRepo(repoURL string) (string, string, error) {
	path := strings.TrimSuffix(strings.TrimPrefix(repoURL, "https://github.com/"), ".git")
	parts := strings.Split(path, "/")
	if path == repoURL || len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("component repository %q is not a https://github.com/<owner>/<repo> URL", repoURL)
	}
	return parts[0], parts[1], nil
}

func shortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

// resolveComponent GETs the referenced Component — always from the fixed
// componentNamespace, never release.Namespace, since Component is a single
// identity shared across every environment rather than a per-environment
// resource — and returns the git-clone repoURL derived from its
// status.repository.url (the HTML URL, "+.git" — see
// PLATFORM_API_ARCHITECTURE.md / component_types.go's RepositoryStatus).
// ready=false means the condition was set and the caller should requeue,
// not treat this as an error.
func (r *ReleaseReconciler) resolveComponent(ctx context.Context, release *platformv1alpha1.Release) (repoURL string, ready bool, err error) {
	component := &unstructured.Unstructured{}
	component.SetGroupVersionKind(componentGVK)
	name := release.Spec.ComponentRef.Name
	if getErr := r.Get(ctx, types.NamespacedName{Name: name, Namespace: componentNamespace}, component); getErr != nil {
		if client.IgnoreNotFound(getErr) != nil {
			return "", false, fmt.Errorf("getting Component/%s: %w", name, getErr)
		}
		r.setReady(release, metav1.ConditionFalse, "ComponentNotFound", fmt.Sprintf("Component/%s not found in namespace %s", name, componentNamespace))
		return "", false, nil
	}

	htmlURL, found, _ := unstructured.NestedString(component.Object, "status", "repository", "url")
	if !found || htmlURL == "" {
		r.setReady(release, metav1.ConditionFalse, "ComponentRepositoryNotReady", fmt.Sprintf("Component/%s status.repository.url not set yet", name))
		return "", false, nil
	}

	return htmlURL + ".git", true, nil
}

// resolvedDatabaseBinding is the generic secret-binding info the Release
// controller resolves from a Database's status.exports entry. Zero value
// means "no binding declared/enabled".
type resolvedDatabaseBinding struct {
	Type      string
	Provider  string
	RemoteRef string
}

// resolveDatabaseBinding resolves spec.bindings.database, if enabled, into
// the generic secret-binding info components/<component>/values/<env>.yaml
// should carry. Returns the zero value when the binding isn't
// declared/enabled.
//
// This reads the Database's public export contract (status.exports), never
// status.connectionSecretRef (an internal, same-cluster implementation
// detail) — the export contract is what lets Release and Database live on
// different clusters. ready=false means the condition was set and the
// caller should requeue, not treat this as an error: Database not
// existing, its export missing, not yet ready, or naming an unsupported
// provider are all normal reconciling states, not failures of this
// controller.
func (r *ReleaseReconciler) resolveDatabaseBinding(ctx context.Context, release *platformv1alpha1.Release) (resolvedDatabaseBinding, bool, error) {
	db := release.Spec.Bindings.Database
	if db == nil || !db.Enabled {
		return resolvedDatabaseBinding{}, true, nil
	}
	if db.Ref == "" {
		r.setReady(release, metav1.ConditionFalse, "InvalidSpec", "bindings.database.enabled is true but ref is empty")
		return resolvedDatabaseBinding{}, false, nil
	}

	database := &unstructured.Unstructured{}
	database.SetGroupVersionKind(databaseGVK)
	if getErr := r.Get(ctx, types.NamespacedName{Name: db.Ref, Namespace: release.Namespace}, database); getErr != nil {
		if client.IgnoreNotFound(getErr) != nil {
			return resolvedDatabaseBinding{}, false, fmt.Errorf("getting Database/%s: %w", db.Ref, getErr)
		}
		r.setReady(release, metav1.ConditionFalse, "DatabaseNotFound", fmt.Sprintf("Database/%s not found in namespace %s", db.Ref, release.Namespace))
		return resolvedDatabaseBinding{}, false, nil
	}

	export, found := findExport(database, exportNameConnection)
	if !found {
		r.setReady(release, metav1.ConditionFalse, "ExportMissing", fmt.Sprintf("Database/%s has no %q export in status.exports", db.Ref, exportNameConnection))
		return resolvedDatabaseBinding{}, false, nil
	}

	if export.exportType != exportTypeSecret {
		r.setReady(release, metav1.ConditionFalse, "ExportTypeUnsupported", fmt.Sprintf("Database/%s connection export has type %q, expected %q", db.Ref, export.exportType, exportTypeSecret))
		return resolvedDatabaseBinding{}, false, nil
	}

	if !export.ready {
		r.setReady(release, metav1.ConditionFalse, "ExportNotReady", fmt.Sprintf("Database/%s connection export is not ready yet", db.Ref))
		return resolvedDatabaseBinding{}, false, nil
	}

	provider, ok := genericProviderFor(export.provider)
	if !ok {
		r.setReady(release, metav1.ConditionFalse, "ExportProviderUnsupported", fmt.Sprintf("Database/%s connection export provider %q is not supported", db.Ref, export.provider))
		return resolvedDatabaseBinding{}, false, nil
	}

	if export.key == "" {
		r.setReady(release, metav1.ConditionFalse, "ExportNotReady", fmt.Sprintf("Database/%s connection export has no location.key set", db.Ref))
		return resolvedDatabaseBinding{}, false, nil
	}

	return resolvedDatabaseBinding{
		Type:      genericBindingTypeSecret,
		Provider:  provider,
		RemoteRef: export.key,
	}, true, nil
}

// databaseExport is one status.exports[] entry, read as plain data — see
// findExport.
type databaseExport struct {
	exportType string
	ready      bool
	provider   string
	key        string
}

// findExport returns the status.exports[] entry with the given name.
// status.exports is a list, not a map, so this is a linear scan rather
// than a direct field lookup.
func findExport(database *unstructured.Unstructured, name string) (databaseExport, bool) {
	exports, found, _ := unstructured.NestedSlice(database.Object, "status", "exports")
	if !found {
		return databaseExport{}, false
	}
	for _, raw := range exports {
		entry, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if entryName, _, _ := unstructured.NestedString(entry, "name"); entryName != name {
			continue
		}
		exportType, _, _ := unstructured.NestedString(entry, "type")
		ready, _, _ := unstructured.NestedBool(entry, "ready")
		provider, _, _ := unstructured.NestedString(entry, "location", "provider")
		key, _, _ := unstructured.NestedString(entry, "location", "key")
		return databaseExport{exportType: exportType, ready: ready, provider: provider, key: key}, true
	}
	return databaseExport{}, false
}

// genericProviderFor translates a Database export's location.provider enum
// into the generic values.yaml provider slug the workload chart
// understands. Unsupported providers are reported by the caller as an
// explicit condition rather than passed through, since an unrecognized
// provider means the chart has no template that could ever consume it.
func genericProviderFor(exportProvider string) (string, bool) {
	switch exportProvider {
	case awsSecretsManagerExportProvider:
		return genericProviderAWSSecretsManager, true
	default:
		return "", false
	}
}

// syncToGitOps commits envContent/valuesContent into application-repositories
// as one atomic commit, skipping entirely if both files already match.
// run is the CI run being auto-deployed, or nil for a pinned version; it
// only changes the commit message.
func (r *ReleaseReconciler) syncToGitOps(ctx context.Context, release *platformv1alpha1.Release, envContent, valuesContent []byte, run *workflowRun) error {
	gh, owner, err := r.githubClientFor(ctx)
	if err != nil {
		return err
	}

	envPath := environmentsFilePath(release.Spec.ComponentRef.Name, release.Spec.Environment)
	valuesPath := valuesFilePath(release.Spec.ComponentRef.Name, release.Spec.Environment)

	files := map[string][]byte{}
	for path, desired := range map[string][]byte{envPath: envContent, valuesPath: valuesContent} {
		current, found, err := gh.GetFileContent(ctx, owner, applicationRepositoriesRepo, path, applicationRepositoriesRef)
		if err != nil {
			return fmt.Errorf("reading current %s: %w", path, err)
		}
		if !found || !bytes.Equal(current, desired) {
			files[path] = desired
		}
	}
	if len(files) == 0 {
		return nil
	}

	headSHA, treeSHA, err := gh.GetBranchHead(ctx, owner, applicationRepositoriesRepo, applicationRepositoriesRef)
	if err != nil {
		return fmt.Errorf("reading %s head: %w", applicationRepositoriesRepo, err)
	}

	message := fmt.Sprintf("Release %s: sync %s/%s", release.Name, release.Spec.ComponentRef.Name, release.Spec.Environment)
	if run != nil {
		message = fmt.Sprintf("Release %s: auto-deploy %s/%s %s\n\nBuilt by %s", release.Name, release.Spec.ComponentRef.Name, release.Spec.Environment, shortSHA(run.HeadSHA), run.HTMLURL)
	}
	if _, err := gh.CommitFiles(ctx, owner, applicationRepositoriesRepo, applicationRepositoriesRef, message, files, headSHA, treeSHA); err != nil {
		return fmt.Errorf("committing to %s: %w", applicationRepositoriesRepo, err)
	}
	return nil
}

// githubCredentials mirrors the single "credentials" key on the
// crossplane-github-credentials Secret — a JSON blob, not separate Secret
// keys (same shape scaffold-operator already reads).
type githubCredentials struct {
	Token string `json:"token"`
	Owner string `json:"owner"`
}

// githubClientFor reads the shared crossplane-github-credentials Secret via
// a direct client.Get (Secrets don't mount cross-namespace) and returns a
// GitHub client plus the account/org this cluster's automation writes as.
func (r *ReleaseReconciler) githubClientFor(ctx context.Context) (githubClient, string, error) {
	secret := &corev1.Secret{}
	if err := r.APIReader.Get(ctx, types.NamespacedName{Name: credentialsSecretName, Namespace: credentialsSecretNamespace}, secret); err != nil {
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

	newClient := r.NewGitHubClient
	if newClient == nil {
		newClient = newGoGithubClient
	}
	return newClient(creds.Token), creds.Owner, nil
}

func (r *ReleaseReconciler) setReady(release *platformv1alpha1.Release, status metav1.ConditionStatus, reason, message string) {
	apimeta.SetStatusCondition(&release.Status.Conditions, metav1.Condition{
		Type:               readyConditionType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: release.Generation,
	})
}

// SetupWithManager sets up the controller with the Manager.
func (r *ReleaseReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&platformv1alpha1.Release{}).
		Named("release").
		Complete(r)
}
