# Auto-deploy

A `Release` can follow a branch of its component's repository instead of a
pinned version. Whenever a newer commit on that branch has a successful CI
build, release-operator commits that commit as `spec.version` to the
Release's own file in `application-repositories`. Argo CD applies the file
like any other change, and the Release then deploys the usual way. There is
no PR per deploy, and git stays the record of what runs where.

This implements the platform-component half of the design in
`platform-architecture/AUTO_DEPLOY.md`, with the GitHub App deferred (see
[Authentication](#authentication)).

Changes span four repositories, each on branch
`claude/release-operator-auto-deploy-oyxd5o`:

| Repository | What changes |
|---|---|
| `release-operator` | Release API, controller, operator flags, chart CRD, this document |
| `backstage` | Create deployment toggle, Onboard Service option, committed-Release endpoint, deployment card |
| `application-repositories` | README only (no structural change) |
| `platform-scaffolds` | README only (documents `ci.yaml` as a contract) |

## How it works

```
developer pushes to main of <component>
        │
        ▼
<component> ci.yaml: build + test + push image ghcr.io/<owner>/<component>:<sha>
        │  (run succeeds)
        ▼
release-operator, polling the Release every 60s:
  newest successful push run of ci.yaml on spec.autoDeploy.branch
        │  newer than spec.version, and ahead of it on the branch?
        ▼
commit 1 to application-repositories main (no PR), the Release file only:
  platform/environments/dev/<component>-release.yaml   spec.version = <sha>
        │
        ▼  push webhook → Argo CD applies the Release (generation bumps)
        ▼
release-operator reconciles it the normal way (no auto-deploy write: up to date)
commit 2, exactly as for a hand-pinned Release:
  components/<component>/environments/dev.yaml  targetRevision = <sha>
  components/<component>/values/dev.yaml        image.tag      = <sha>
        │
        ▼
Argo CD syncs dev
```

Auto-deploy only ever writes the Release file. The `components/` files are
still written by the one existing path, from the Release Argo CD applied.

## The API

```yaml
spec:
  componentRef: { name: orders }
  environment: dev
  version: "1a2b3c4…"     # set by release-operator in git; absent only before the first build
  autoDeploy:
    branch: main
status:
  autoDeploy:
    branch: main
    latestDeployable: 1a2b3c4…
    runURL: https://github.com/<owner>/orders/actions/runs/…
    reason: UpToDate | Deploying | WaitingForFirstBuild | NotFastForward | ReleaseFileNotFound
    message: …
```

- `spec.autoDeploy.branch` (required inside `autoDeploy`) turns it on.
- `spec.version` stays the single source of truth for what runs. It may be
  absent only when `autoDeploy` is set (CEL rule:
  `has(self.version) || has(self.autoDeploy)`), so a service can be onboarded
  before its first build. Both together is the normal state.
- Turning auto-deploy off (a PR removing `autoDeploy`) leaves the Release
  pinned at whatever `version` it has. That is also the way to pause it.
- `status.autoDeploy` has **no timestamp**. Every status write triggers another
  reconcile, so a "last checked" time would make an idle Release reconcile
  itself in a loop.

## The decision, step by step

On every reconcile of a Release with `autoDeploy`:

1. **Environment allowed?** Not in `--auto-deploy-environments` (default
   `dev`) → `Ready=False, reason=AutoDeployNotAllowed`; nothing is polled or
   written. This is enforced here, not only by the Backstage form, so a
   hand-written PR can't turn auto-deploy on for prod.
2. **Newest successful build** on the branch: the newest successful `push`
   run of `ci.yaml` (same query as Backstage's Version picker,
   `DeployableVersionReader.ts`). In the scaffold's `ci.yaml` such a run only
   succeeds once `push-image` has pushed the SHA-tagged image.
   - None, and no `version` yet → `WaitingForFirstBuild`, poll again.
   - None, but a `version` (runs aged out of the listing) → keep it.
   - Same as `spec.version` → `UpToDate`.
3. **Read the Release file from git, at a pinned head.** The operator reads
   `application-repositories`' head commit first and the file at exactly that
   commit, then commits with that commit as parent. If anything lands in
   between, GitHub rejects the commit and the reconcile retries.
4. **Git wins over the cluster.** Every decision uses the file, not the
   cluster copy, which can lag a merge until Argo CD syncs:
   - File missing, or holding another Release → `ReleaseFileNotFound`; the
     file is **never created** (a teardown may have just removed it).
   - File no longer has `autoDeploy.branch` (someone turned it off and Argo CD
     hasn't applied it yet) → `Deploying`, no write.
   - File already has the newest build → `Deploying`, waiting for Argo CD.
5. **Forward only.** If the file has a version, GitHub's compare API must say
   the new build is `ahead` of it on the branch. `behind` or `diverged` →
   `NotFastForward`, no write. Because the reference point is the version in
   git, this survives a cluster rebuild.
6. **Commit** `spec.version` into the file: `Auto-deploy <component> <sha7> to
   <env>`, with the CI run linked in the body. The file is edited as a YAML
   node tree, so bindings, labels, comments and key order are untouched; a
   missing `version` is added right after `environment`.

Then, as for any Release: without a `version` there's nothing to deploy yet;
with one, the existing path writes the `components/` files.

### Guards in one table

| Guard | Behaviour | Why |
|---|---|---|
| Dev only | `--auto-deploy-environments`, default `dev` | Backstage greying the toggle out doesn't stop a hand-written PR. |
| Forward only | compare API against the version in git | A re-run or slow older build must not roll dev backwards, also after a cluster rebuild. |
| Git wins | decide from the file at a pinned head; commit on that parent | A human change that hasn't synced yet must never be overwritten. |
| Never create the file | missing file → `ReleaseFileNotFound` | Teardown removes Release files first; recreating them is the write-back race that left dangling files before. |
| Deleting Releases | a Release with a `deletionTimestamp` writes nothing | Same teardown race, for the `components/` files. |
| No hot loop | idle polls write a byte-identical status, which the API server treats as a no-op | The controller reconciles on every change to the Release. |
| Cheap idle polls | nothing newer and the spec already synced → stop after the runs query | One GitHub call per Release per poll; `application-repositories` isn't read. |

## Polling, not (yet) webhooks

A Release with `autoDeploy` requeues every `--auto-deploy-poll-interval`
(default 60s). Polling is the source of truth because a webhook alone can miss
deliveries (operator restarting, cluster rebuilt) and would need catching up
anyway. The management cluster already receives GitHub push webhooks for Argo
CD (shared HMAC secret), so a `workflow_run` webhook that only enqueues the
matching Release is a small follow-up that cuts latency; polling stays as the
fallback.

Cost: one GitHub API call per auto-deploy Release per poll while idle; a new
build adds three or four (head, file, compare, commit). Well inside the 5,000/h
limit for dozens of Releases sharing one token.

## Operator flags (`cmd/main.go`)

| Flag | Default | Purpose |
|---|---|---|
| `--auto-deploy-environments` | `dev` | Comma-separated environments where `spec.autoDeploy` is honoured. |
| `--auto-deploy-poll-interval` | `60s` | How often an auto-deploy Release checks for a newer build. |

Set them through the chart's `manager.args` if the defaults need changing.

## Authentication

The operator authenticates as the `taskapp-platform-deployer` GitHub App
(Contents read/write, Actions read, installed on all `entr0pian`
repositories); its commits show as `taskapp-platform-deployer[bot]`.

- **Credentials**: App ID, installation ID and private key live in Secrets
  Manager (`taskapp/platform/github-app`). The chart's ExternalSecret writes
  them into `release-operator-github-app` in the operator's namespace, and a
  Role grants `get` on that one Secret only.
- **Tokens**: `ghinstallation` mints 1-hour installation tokens and renews
  them itself. The operator holds two, each narrowed below what the App has:
  `contents: write` on `application-repositories` only (every commit), and
  `actions: read` + `contents: read` for the service repos (CI runs and the
  forward-only compare). Clients are rebuilt only when the Secret changes, so
  a rotated key is picked up without a restart.
- **No PAT fallback.** `--github-auth=pat` (chart: `github.auth: pat`) still
  switches the operator to the shared `crossplane-system/crossplane-github-credentials`
  token for clusters without the App, but the two modes never mix: with
  `app`, a missing or broken App Secret fails the reconcile instead of
  quietly writing with the PAT, and the chart doesn't grant access to the
  PAT's Secret at all.

| Flag | Chart value | Default |
|---|---|---|
| `--github-auth` | `github.auth` | `app` |
| `--github-owner` | `github.owner` | `entr0pian` (chart) |
| `--github-app-secret` | derived: `<namespace>/<release>-github-app` | — |

## Backstage

**Onboard Service**
- "Set up auto deployment to `<env>`" (`PlatformAutoDeploySetup`), **on by
  default**. `<env>` is the first entry of `platform.autoDeployEnvironments`,
  not a hardcoded `dev` (`platform.environments` starts with `management`, so
  it can't be used for this).
- When on, the onboarding PR also adds
  `platform/environments/<env>/<name>-release.yaml`: `autoDeploy: {branch:
  main}`, no version, no bindings, at the path and name Create deployment uses.

**Create deployment**
- **Auto-deploy toggle** (`PlatformAutoDeployToggle`) between Environment and
  Version:
  - Greyed out and off outside `platform.autoDeployEnvironments`.
  - In an allowed environment it starts as what's committed in git
    (`GET /api/platform/committed-releases/:component/:environment`).
  - If that can't be read, the toggle is locked and the form refuses to
    submit, so a failed lookup can't silently turn auto-deploy off.
  - Locked off when the form is opened by Roll back.
- **Version picker:** greyed out and cleared while auto-deploy is on; required
  only when it's off (schema `if`/`else`).
- **Rendered Release:** `version` when one was picked, `autoDeploy: {branch:
  main}` when the toggle is on. Turning it on drops the version; the operator
  sets it to the newest build within a poll.

**Deployments card**
- An auto-deploy environment shows an **Auto-deploy** badge and no Roll back:
  the next build would move straight past it. The ways back are reverting on
  `main` (fix forward) or turning auto-deploy off and pinning a version.
- Versions are read from `spec.version` as for any Release; empty means
  waiting for the first build.

## Decided against or out of scope

| Idea | Decision | Reason |
|---|---|---|
| Version only in `status`, operator writes `components/` directly | Rejected | Git would stop saying what runs; a cluster rebuild would lose the version; Promote couldn't copy dev's version from the Release file. |
| Operator patches `spec.version` on the cluster object | Rejected | Argo CD would revert it (self-heal). The change goes to git instead. |
| Release file + `components/` in one commit | Rejected | It would mix git state with a possibly stale cluster copy (bindings); two commits keep one writer per file kind, and webhooks keep the delay to seconds. |
| Each service's CI pushes to `application-repositories` | Rejected | Write credentials for the GitOps repo in every service repo. |
| `status.autoDeploy.lastChecked` (from the design doc) | Rejected | Self-reconcile loop; see [The API](#the-api). |
| Promote (dev → prod), prod Rollback | Out of scope | Next step; `spec.version` in git is what Promote copies. |
| GitHub App, `workflow_run` webhook | Out of scope | Next steps, above. |

## Tests run and results

### release-operator

`KUBEBUILDER_ASSETS=bin/k8s/1.35.0-linux-amd64 go test ./internal/...`, with
a real kube-apiserver and etcd (envtest, Kubernetes 1.35), so CRD validation
and status-update behaviour are real. GitHub is an in-memory fake
(`fakeGitHub` in `internal/controller/auto_deploy_test.go`).

```
Ran 29 of 29 Specs
SUCCESS! -- 29 Passed | 0 Failed | 0 Pending | 0 Skipped
```

Auto-deploy specs:

| Spec | Checks |
|---|---|
| rejects neither version nor autoDeploy / autoDeploy without a branch; accepts both | The CEL rule and `branch` validation, enforced by the real API server |
| refuses auto-deploy outside the allowed environments | `prod` → `AutoDeployNotAllowed`, no GitHub calls, no commit |
| waits for the first successful build | `WaitingForFirstBuild`, requeue at the poll interval, no commit |
| commits the first build to the Release file only, and deploys once Argo CD applies it | First commit touches **only** the Release file (exact rendered YAML, message, run link), cluster copy unpatched; a second poll doesn't recommit; after "Argo CD" applies the version, the normal path writes `components/` |
| moves the version forward only when the new build is ahead | Compare `behind` → `NotFastForward`, file unchanged; `ahead` → file moves to the new SHA |
| leaves the file alone when git no longer has auto-deploy | Cluster says on, git says off → no write |
| never creates a missing Release file, or edits another Release's | `ReleaseFileNotFound`, nothing written |
| makes no commit, no file reads and no status write when nothing changed | Idle poll: no `application-repositories` read, **unchanged `resourceVersion`** |
| writes nothing to git for a Release being deleted | No GitHub calls at all |
| leaves a pinned Release unchanged | Regression check for the existing behaviour |
| releaseFile: sets/adds version, rejects non-Releases | YAML round-trip keeps every other line, including a comment |

A mutation check confirmed the "git wins" spec fails when its guard is
removed.

`make manifests generate fmt vet` is clean; the chart CRD was synced from
`config/crd/bases`. `bin/golangci-lint run`: **0 issues** (the three old
`interface{}` findings on `main` are fixed on this branch).

### backstage

| Check | Result |
|---|---|
| `yarn tsc` | 0 errors |
| App tests (`CI=true yarn backstage-cli package test src/modules`) | 25 suites, **131 passed** |
| Backend tests (same, `packages/backend`) | 14 suites, **164 passed** |
| `backstage-cli package lint`, app and backend | clean |
| Template render tests: create-deployment / onboard-service / add-database | 5/5, 6/6, 2/2 |

`backstage-cli package test` runs in watch mode, and never exits, unless
`CI=true` is set.

### Not tested here

- The real GitHub API (`LatestSuccessfulRun`, `CompareCommits`): thin
  wrappers over go-github; the tests use a fake.
- A live scaffolder run, and Argo CD applying an auto-deployed commit.

Suggested end-to-end check after deploying:
1. Onboard a test service (auto deployment is on by default) and merge the PR.
2. Watch its Release go `WaitingForFirstBuild` → an `Auto-deploy <name> <sha7>
   to dev` commit on the Release file → `Synced`.
3. Push a commit; within about a minute a new `Auto-deploy` commit moves
   `version:`, followed by the `components/` commit.
4. Open Create deployment for it in dev: the toggle starts on.
5. Turn it off, pick a version, merge: the operator stops moving `version:`.
