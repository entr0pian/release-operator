# Auto-deploy

A `Release` can follow its component's `main` branch instead of a pinned
version. release-operator then deploys every successful CI build on `main` by
itself, committing straight to `application-repositories`, with no PR per
deploy. This document is the agreed plan, the reasoning behind each decision,
and the tests run against the implementation, with their results.

Changes span four repositories, each on branch
`claude/release-operator-auto-deploy-oyxd5o`:

| Repository | What changes |
|---|---|
| `release-operator` | Release API, controller, operator flags, chart CRD, this document |
| `backstage` | Create deployment toggle, Onboard Service option, committed-Release endpoint, deployment card |
| `application-repositories` | README only (no structural change) |
| `platform-scaffolds` | README only (documents `ci.yaml` as a contract) |

Nothing changes in `component-operator`, `helm-charts` or the scaffold's CI
workflow itself.

## How it works

```
developer pushes to main of <component>
        │
        ▼
<component> ci.yaml: build + test + push image ghcr.io/<owner>/<component>:<sha>
        │  (run succeeds)
        ▼
release-operator, polling the Release every 60s:
  GET /repos/<owner>/<component>/actions/workflows/ci.yaml/runs
      ?branch=main&event=push&status=success&per_page=1
        │  newer run than status.autoDeploy.runNumber?
        ▼
commit to application-repositories main (no PR):
  components/<component>/values/dev.yaml        image.tag      = <sha>
  components/<component>/environments/dev.yaml  targetRevision = <sha>
        │
        ▼
Release status.autoDeploy = { deployedVersion, runNumber, runURL, deployedAt }
        │
        ▼
Argo CD syncs dev
```

## The plan

### 1. Release API (`api/v1alpha1/release_types.go`)

```yaml
spec:
  componentRef: { name: orders }
  environment: dev
  autoDeploy:
    enabled: true        # exactly one of this or `version`
status:
  autoDeploy:
    deployedVersion: <sha>
    runNumber: 42
    runURL: https://github.com/<owner>/orders/actions/runs/...
    deployedAt: <time>
```

- `spec.autoDeploy: { enabled: bool }` is new.
- `spec.version` becomes optional. A CEL rule on the spec enforces
  "set exactly one of version or autoDeploy.enabled". The API server rejects
  both and rejects neither.
- `status.autoDeploy` records what was deployed.
- The CRD is regenerated in `config/crd/bases/` and copied into
  `chart/templates/crd/`.

**Why a struct, not a bool:** it leaves room for `branch` or `workflow` later
without another API change.

**Why version and auto-deploy are mutually exclusive:** a Release with both
would be ambiguous. Making the API reject it means every consumer
(Backstage, the operator, people reading the file) only ever has one
question to ask: pinned or following main.

### 2. The operator never writes the version into `spec`

The Release manifest lives in git
(`platform/environments/<env>/<component>-release.yaml`) and Argo CD applies
it. If the operator wrote each new SHA into `spec.version`, Argo CD would
either revert it (self-heal) or show the Release OutOfSync forever, and git
would stop being the source of truth. So the version the operator picked lives
only in `status.autoDeploy`. The Release file in git doesn't change on a
deploy; only `components/<component>/…` does.

### 3. How a new build is detected: polling, not webhooks

A Release with auto-deploy on requeues every `--auto-deploy-poll-interval`
(default 60s). Each reconcile asks GitHub for the newest successful run of
`ci.yaml` triggered by a push to `main`.

Why polling rather than webhooks:
- A webhook alone can miss deliveries (operator restarting, cluster being
  rebuilt), so something would have to catch up anyway. Polling is that
  catch-up, and it is idempotent: a poll that finds nothing new does nothing.
- Webhooks would also need registering on every service repository, which
  component-operator would have to do for each new Component.
- The management cluster already receives GitHub push webhooks for Argo CD
  (with a shared HMAC secret), so a `workflow_run` webhook that only triggers
  a reconcile is a small follow-up: it cuts deploy latency, with polling still
  the source of truth.

Why this exact query:
- It is the same one Backstage's Create deployment Version picker uses
  (`DeployableVersionReader.ts`), so auto-deploy and a person picking a version
  agree on what "deployable" means.
- In the scaffold's `ci.yaml`, a push run on `main` only succeeds once its
  `push-image` job has pushed the SHA-tagged image, so the image is guaranteed
  to exist.

Cost: one API call per auto-deploy Release per minute. An idle poll stops
there: when the newest run is the one already deployed, the spec hasn't
changed and the last sync succeeded, the operator doesn't read
`application-repositories` at all. The trade-off is that a hand edit to the
component's files there isn't reverted until the next build or Release
change, which is how pinned Releases already behave. That's well inside
GitHub's 5,000/hour limit for roughly 80 auto-deploy Releases sharing one
token. Conditional (ETag) requests would make polls nearly free and are a
possible follow-up.

### 4. Writing to application-repositories: reuse the existing path

The resolved SHA goes through the operator's existing
`buildEnvironmentsFile` / `buildValuesFile` / `syncToGitOps`. That path is
already a single atomic commit straight to `main` (no PR), and it already
skips the commit when both files match. The only differences for an auto-deploy
Release:
- The version comes from the CI run instead of `spec.version`.
- The commit message is `Release <name>: auto-deploy <component>/<env> <sha7>`
  and links to the run.

### 5. Guards

| Guard | Behaviour | Why |
|---|---|---|
| Dev only | `--auto-deploy-environments` (default `dev`). A Release with auto-deploy in any other environment gets `Ready=False, reason=AutoDeployNotAllowed`; nothing is polled or written. | Greying the toggle out in Backstage only stops people using the form. A hand-written PR could still put `autoDeploy` on a prod Release. |
| Forward only | Never deploy a run with a lower `runNumber` than `status.autoDeploy.runNumber`. | A re-run or an out-of-order listing must not roll dev backwards. The guard lives in status, so it starts fresh when status is lost (cluster rebuilt, auto-deploy turned off and on). That's harmless: the newest successful run is always the one picked. |
| No build yet | `Ready=False, reason=AwaitingFirstBuild`, poll again. Not an error. | Right after onboarding, the repository and its first CI run don't exist yet. |
| Runs aged out | If GitHub lists no runs but one was already deployed, keep it. | A quiet repository shouldn't make the Release regress to "waiting". |
| No hot loop | A poll that finds nothing new writes a byte-identical status, which the API server treats as a no-op: no new `resourceVersion`, no watch event. | The controller reconciles on every change to the Release. A timestamp written on every poll would make it reconcile itself continuously. |

### 6. Operator flags (`cmd/main.go`)

| Flag | Default | Purpose |
|---|---|---|
| `--auto-deploy-environments` | `dev` | Comma-separated environments where `spec.autoDeploy` is honoured. |
| `--auto-deploy-poll-interval` | `60s` | How often an auto-deploy Release checks for a newer run. |

Set them through the chart's `manager.args` if the defaults need changing.
The GitHub token in `crossplane-system/crossplane-github-credentials` also
needs `actions:read` on the service repositories. It created those
repositories, so it should already have it.

### 7. Backstage

**Onboard Service**
- New checkbox "Set up auto deployment to dev", off by default.
- When it's ticked, the onboarding PR also adds
  `platform/environments/dev/<name>-release.yaml`: auto-deploy on, no version,
  no bindings.
- The name and path are the same ones Create deployment uses, so Create
  deployment later shows this file as committed and edits it.

**Create deployment**
- **Auto-deploy toggle** (`PlatformAutoDeployToggle`), placed between
  Environment and Version:
  - Greyed out and off outside `platform.autoDeployEnvironments` (default
    `[dev]`, which must match the operator flag).
  - In dev it starts as what's committed in git. It reads that from the new
    `GET /api/platform/committed-releases/:component/:environment`, which
    reads the file from `application-repositories` `main`. Git is used rather
    than the cluster copy because the cluster lags a merge until Argo CD syncs
    and doesn't exist before the first sync.
  - Locked off when the form is opened by Roll back.
- **Version picker:**
  - Greyed out and cleared while auto-deploy is on.
  - Required only when auto-deploy is off. This is a schema `if`/`else`, not
    an unconditional `required`.
- **Rendered Release:** contains `autoDeploy.enabled: true` or
  `version: <sha>`, never both. Turning auto-deploy off therefore pins a
  version, and turning it on clears the pin.
- **PR:** titled `Enable auto-deploy for <component> in dev`, on branch
  `backstage/deploy-<component>-dev-auto`.

**Deployments card**
- An auto-deploy environment shows an **Auto-deploy** badge and **no Roll back
  button**. The next build on `main` would deploy straight over a roll back.
  The ways back are reverting the commit on `main` (fix forward), or turning
  auto-deploy off and pinning a version.
- Everything that read `spec.version` (card version, rollout progress, the
  "running image matches the Release" check) now reads the effective version:
  `spec.version`, or `status.autoDeploy.deployedVersion` for an auto-deploy
  Release.

### Decided against or out of scope

| Idea | Decision | Reason |
|---|---|---|
| Operator edits `spec.version` | Rejected | Fights Argo CD (see §2). |
| Each service's CI pushes to application-repositories itself | Rejected | It would put write credentials for the GitOps repo in every service repo. With this design one credential, held by the operator, does it. |
| Warning in the PR when Create deployment would turn auto-deploy off | Rejected | Replaced by the form showing the committed state and allowing only valid combinations. |
| Roll back on an auto-deploy environment that pins the old version | Rejected | The button is hidden instead, which is simpler. Pinning stays one deliberate step away in Create deployment. |
| Promote (dev → prod) button | Out of scope | Agreed to keep this change focused. |
| Webhook trigger | Out of scope | Polling first; a `workflow_run` webhook is the natural speed-up (see §3). |

## Tests run and results

All of these were run in this branch's environment before pushing.

### release-operator

**Unit and envtest suite.** Run with
`KUBEBUILDER_ASSETS=bin/k8s/1.35.0-linux-amd64 go test ./...`. It uses a real
kube-apiserver and etcd (envtest, Kubernetes 1.35), so CRD validation and
status-update behaviour are the real thing. GitHub is an in-memory fake
(`fakeGitHub` in `internal/controller/auto_deploy_test.go`).

```
Ran 21 of 21 Specs in 7.554 seconds
SUCCESS! -- 21 Passed | 0 Failed | 0 Pending | 0 Skipped
ok  github.com/entr0pian/release-operator/internal/controller
```

New specs (all passed):

| Spec | Checks |
|---|---|
| API validation rejects a Release with both version and autoDeploy.enabled | The CEL rule, enforced by the real API server |
| API validation rejects a Release with neither version nor autoDeploy.enabled | Same, the other way round |
| refuses auto-deploy outside the allowed environments, without calling GitHub | `prod` + auto-deploy → `AutoDeployNotAllowed`, no runs queried, no commit, no requeue |
| waits for the first successful build instead of failing | No run yet → `AwaitingFirstBuild`, requeue after the poll interval, no commit. Queries `entr0pian/orders ci.yaml@main` |
| deploys the latest successful run directly to application-repositories and records it in status | Run #7 → one commit with the SHA in `values/dev.yaml` and `targetRevision`, commit message links the run, `spec.version` stays empty, `status.autoDeploy` set. Then run #8 → rolls forward. Then run #7 reported again → stays on #8 |
| makes no commit, no file reads and no status write when nothing changed | A second poll with the same run → no commit, no read of `application-repositories`, and an **unchanged `resourceVersion`** (no reconcile loop) |
| leaves a pinned Release unchanged: deploys spec.version, never polls, never requeues | Regression check for the existing behaviour |
| parseGitHubRepo splits a Component clone URL / rejects non-GitHub URLs | URL parsing |

The 11 existing specs (Component resolution, database binding, GitOps file
rendering) still pass unchanged.

**Code generation:** `make manifests generate fmt vet` is clean. The CRD and
deepcopy code were regenerated, and the chart CRD was synced from
`config/crd/bases`.

**Lint:** `bin/golangci-lint run` reports 0 findings in code this change adds.
3 `modernize` findings remain; they are on `main` already, in lines this
change doesn't touch (`interface{}` → `any` in `findExport` and in
`release_controller_test.go`).

`make test` itself exits non-zero in this sandbox. The cause is
`go: no such tool "covdata"` when it collects coverage for packages with no
tests. This happens identically on `main`, is a property of the sandbox's Go
toolchain, and the controller package's tests pass in the same run.

### backstage

| Check | Command | Result |
|---|---|---|
| Typecheck | `yarn tsc` | 0 errors (also 0 on `main` beforehand) |
| Frontend tests | `yarn backstage-cli package test src/modules` in `packages/app` | 24 suites, **123 passed**, 0 failed |
| Backend tests | `yarn backstage-cli package test src/modules` in `packages/backend` | 14 suites, **163 passed**, 0 failed |
| Lint | `yarn backstage-cli package lint` in `packages/app` and `packages/backend` | clean |
| Template render, Create deployment | `node --test templates/create-deployment/render.test.mjs` | 5/5 passed |
| Template render, Onboard Service | `node --test templates/onboard-service/render.test.mjs` | 5/5 passed |
| Template render, Add Database (unchanged) | `node --test templates/add-database/render.test.mjs` | 2/2 passed |

New tests:
- `AutoDeployToggle.test.tsx`
  - Greyed out and off in `prod`, and never calls the backend there.
  - Turns itself off when the environment changes to one without
    auto-deploy.
  - Starts **on** in dev when the committed Release has auto-deploy, and
    calls `/committed-releases/orders/dev`.
  - Starts **off** in dev when the committed Release pins a version.
  - Locked off for a roll back.
- `CommittedReleaseMapper.test.ts`
  - No file means no Release.
  - Parses an auto-deploy Release.
  - Parses a pinned Release, keeping only its enabled bindings.
  - A non-Release file or invalid YAML is treated as absent rather than
    crashing the form.
  - The file path matches where Create deployment writes.
  - Only Kubernetes-style names are accepted, so nothing like `../secrets` can
    reach the path.
- `ReleaseVersionMapper.test.ts`: an auto-deploy Release reports
  `status.autoDeploy.deployedVersion` as its version, or an empty version
  before the first deploy.
- `joinDeployments.test.ts`: the auto-deploy flag reaches the deployment card.
- Render tests:
  - Auto-deploy Release, with and without a database binding.
  - Turning auto-deploy off pins the version again.
  - The Onboard Service dev Release renders as expected.
  - Onboard Service and Create deployment use the same file name and Release
    name.

**Form schema check.** The Create deployment parameter schema was compiled
with ajv 8 (the validator the scaffolder form uses):

```
VALID   pinned with version
INVALID pinned, no version            — must have required property 'version'
INVALID autoDeploy false, no version  — must have required property 'version'
VALID   autoDeploy on, no version
```

**Rendering with the real template engine.** Both skeletons, plus the PR
branch, title and table expressions, were rendered with nunjucks 3.2.4
configured with `${{ }}` delimiters, the way Backstage's scaffolder runs it.
This catches nunjucks behaviour the dependency-free render tests can't. All
output was as intended:

```
autoDeploy=true  | backstage/deploy-orders-dev-auto     | Enable auto-deploy for orders in dev | auto-deploy: every successful build on `main`
autoDeploy=false | backstage/deploy-orders-prod-ff5987e | Deploy orders ff5987e to prod        | `ff5987e8395c2fc8f52f3cd078416830244b5019`
onboard PR "Adds" line, setupAutoDeploy=true:  `platform/registry/orders.yaml` and `platform/environments/dev/orders-release.yaml`.
onboard PR "Adds" line, setupAutoDeploy=false: `platform/registry/orders.yaml`.
```

### Not tested here

No real cluster, Argo CD or GitHub was available, so these were **not**
exercised:

- `goGithubClient.LatestSuccessfulRun` against the real GitHub API. It is a
  thin wrapper over go-github's `ListWorkflowRunsByFileName`; the tests use a
  fake.
- A full scaffolder run in a live Backstage: the form, `fetch:template`
  conditionals and the PR being opened.
- Argo CD syncing an auto-deployed commit.

Suggested end-to-end check after deploying:
1. Onboard a test service with "Set up auto deployment to dev" ticked, and
   merge the PR.
2. Watch the Release go `AwaitingFirstBuild` → `Synced` once the first
   `ci.yaml` run on `main` succeeds.
3. Push a commit and confirm a
   `Release <name>-dev: auto-deploy <name>/dev <sha>` commit lands in
   `application-repositories` within about a minute.
4. Open Create deployment for that service in dev: the toggle should start
   on.
5. Switch it off, pick a version, merge, and confirm the operator stops
   following `main`.
