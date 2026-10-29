# Ferry: design

Ferry ships container images into Kubernetes workloads that a dashboard owns, and stays out of
the way of GitOps.

It targets a split that is becoming common now that [Zeabur](https://github.com/zeabur-oss) is open
source:

- **Argo CD (git)** owns the platform layer: ingress controller, cert-manager, monitoring, the
  dashboard itself, and Ferry.
- **A dashboard (Zeabur)** owns the applications. People change their env vars, replicas,
  resources and domains by hand, so these objects cannot also be reconciled from git.
- **Ferry** is the missing CD piece for that second layer. CI builds an image and tells Ferry
  `image:tag`. Ferry points every workload running that image at the new tag, in every cluster,
  and leaves anything Argo CD tracks alone. Ferry also has a web UI to manage build sources,
  see what runs where, deploy a specific tag, roll back, and pause auto-deploys.

Ferry replaces an existing ~380-line Python service, "ci-relay", which already does the
backend part in production. `docs/reference/` contains that service and the files around it.
Ferry **must stay compatible** with every external contract listed in
[Compatibility](#compatibility), because CI pipelines and GitHub webhooks already call it.

## Goals and non-goals

Goals:

- One static Go binary with the React SPA embedded (`go:embed`). No database. State is stored in a
  ConfigMap, or in a JSON file outside Kubernetes.
- Multi-cluster, using a ServiceAccount token per cluster. Only `get`, `list` and `patch` are
  needed. See [RBAC](#rbac-ferry-runs-with).
- Every feature of ci-relay, plus the UI and the API behind it.
- Generic enough to open source: nothing StarMoe-specific hard-coded. Everything specific goes in
  the config file.

Non-goals. Zeabur or Argo CD already does these; do not build them:

- Editing env vars, resources, domains or volumes; logs; terminals; metrics; restarts.
- Creating or deleting workloads. Services are created in the dashboard.
- Running builds. Ferry starts builds on an external builder (CNB) or lets external CI
  (GitHub Actions) build and call Ferry.

## Architecture

```
GitHub push ──webhook──▶ Ferry ──mirror + start build──▶ CNB build ──┐
                                                                     │ POST /deploy
GitHub Actions / any CI ─────────────────────────────────────────────┤ {image, tag, source}
                                                                     ▼
Browser (SPA, OIDC login) ──/api──▶ Ferry ──JSON patch──▶ clusters 1..n (namespaces you hand to the dashboard)
                                       │
                                       ├── state: ConfigMap (sources, deploy tokens, activity)
                                       └── nightly snapshot ──git push (SOPS)──▶ backup repo
```

Repository layout. This is a suggestion; keep it flat and boring.

```
cmd/ferry/main.go            flags: -config path; subcommand `version`
internal/kube/               minimal REST client (already written: kube.go)
internal/kube/fake/          in-memory fake API server for tests and demo mode
internal/state/              data model + file/ConfigMap backends
internal/ferry/              config, deploy/rollout/pause, builds (CNB), GitHub, snapshot
internal/server/             HTTP routes, auth (OIDC + sessions + deploy tokens), SPA serving
web/                         Vite + React + TypeScript SPA; web/embed.go embeds web/dist
deploy/                      example Kubernetes manifests (Deployment, RBAC, config)
docs/                        this file, configuration reference
```

`internal/kube/kube.go` already exists. It is a minimal client: bearer token, custom CA,
in-cluster mode, and `StatusError`. Use it and extend it as needed. Do not pull in `client-go`.

Allowed Go dependencies: the standard library, `sigs.k8s.io/yaml`, `github.com/coreos/go-oidc/v3`
and `golang.org/x/oauth2`. Ask before adding more. Shell out to the `git` and `sops` binaries,
which the image includes. Do not use go-git or the sops library.

## Configuration

The config is a YAML file (`-config /etc/ferry/config.yaml`). Before parsing, expand `${VAR}` from
the environment, so secrets come from env vars that a Secret fills. Defaults are in brackets.

```yaml
listen: ":8080"
publicURL: https://ci.star.moe          # used for OIDC redirect and webhook URLs
namespaces: [apps]                      # namespaces Ferry acts in, on every cluster
annotationPrefix: ferry.star.moe        # [ferry.star.moe] -> <prefix>/auto-deploy

clusters:                               # and/or clustersFile
  - name: local
    inCluster: true
  - name: tl
    server: https://203.0.113.7:6443
    caData: <base64 PEM>
    token: ${TL_TOKEN}
clustersFile: /etc/ferry-clusters/clusters.json   # ci-relay format, see Compatibility

state:                                  # exactly one
  kubernetes: { namespace: ferry, name: ferry-state }   # ConfigMap, via in-cluster ServiceAccount
  # file: /var/lib/ferry/state.json

auth:
  sessionKey: ${SESSION_KEY}            # HMAC key for cookies; random per start if empty (warn)
  sessionHours: 12                      # how long a browser sign-in lasts [12]
  oidc:
    issuer: https://login.star.moe
    clientID: ferry
    clientSecret: ${OIDC_CLIENT_SECRET}
    scopes: [openid, profile, groups]   # [these]
    groupsClaim: groups                 # [groups]
    allowedGroups: [admins]             # any of these (as the ID token spells them), or
    allowedUsers: []                    # preferred_username or sub
  # disabled: true                      # local development only: everyone is "dev" (log a loud warning)

deployAuth:
  # Besides deploy tokens made in the UI, accept any token that can read this git repository
  # (HTTP basic auth against <url>/info/refs?service=git-upload-pack, cached 10 minutes by SHA-256).
  gitRead: { url: https://cnb.cool/StarMoe-org/infra.git, username: cnb }

github:
  webhookSecret: ${WEBHOOK_SECRET}
  tokens: ${GH_TOKENS}                  # JSON object {"<owner>": "<token>"}, valid YAML after expansion
  hookTokens: ${GH_HOOK_TOKENS}         # optional, same shape: used for webhooks instead of tokens

cnb:                                    # optional builder
  repo: StarMoe-org/ci                  # where sources are mirrored and built
  token: ${CNB_TOKEN}
  pipelineFile: /etc/ferry/pipeline.yml
  imagePrefix: docker.cnb.cool/starmoe-org/ci/
  branchPrefix: src/                    # [src/]
  api: https://api.cnb.cool             # [this]
  git: https://cnb.cool                 # [this]

snapshot:                               # optional
  repo: https://cnb.cool/StarMoe-org/infra.git
  username: cnb
  token: ${CNB_TOKEN}
  branch: main                          # [main]
  dir: snapshots                        # [snapshots]
  hourUTC: 19                           # [19]
  sops: true                            # run `sops -e -i` on Secret files (the repo's .sops.yaml decides keys)
  author: { name: ferry, email: ferry@localhost }

seedSources: /etc/ferry/repos.json      # imported once, when the state has no sources yet
workDir: /work                          # [/work] git scratch space
```

Validate at startup and fail with clear messages. Log which features are on: builder,
snapshots, OIDC, and clusters.

## Data model (state)

```go
type Source struct {
    ID        string    // short random id
    Repo      string    // GitHub "owner/name"
    Branch    string    // builds on pushes to this branch
    Builder   string    // "cnb" | "external"
    Image     string    // cnb: name under cnb.imagePrefix; external: full repository, e.g. ghcr.io/org/app
    BuildArgs string    // cnb only, passed through as BUILD_ARGS
    Created   time.Time
}
// ImageRef(): cnb -> cnb.imagePrefix + Image; external -> Image. Workloads are matched to sources by ImageRef.

type Token struct {          // deploy tokens for CI
    ID, Name string
    Hash     string          // sha256 hex of the token; the token itself is shown once
    Hint     string          // last 4 characters
    Created  time.Time
    LastUsed *time.Time
}

type Event struct {          // the activity feed; newest first, keep the last 300
    ID       string          // sortable: time-based + random
    Time     time.Time
    Kind     string          // build | deploy | rollout | rollback | pause | resume | snapshot | source | token
    Actor    string          // user's preferred_username, "token:<name>", "ci" (gitRead), "github", "schedule"
    Status   string          // queued | started | ok | partial | error | skipped
    Image    string          // repository without tag
    Tag      string
    Repo     string          // GitHub owner/name, for builds
    Commit   string
    Link     string          // the "source" sent with a deploy (usually a commit URL)
    LogURL   string          // CNB build log
    Message  string
    Results  []Result        // deploy/rollout/rollback
}

type Result struct {
    Cluster, Namespace, Kind, Name, Container string
    From, To string           // full image refs
    Result   string           // deployed | current | argo-cd | paused | would-deploy | error
    Error    string
}
```

Backends:

- **ConfigMap**: keys `state.json` (sources and tokens) and `events.json`. Create it if it is
  missing. Update with `resourceVersion`; on a 409 conflict, re-read and retry.
- **File**: one JSON file, written atomically (temp file plus rename).

All mutations go through one mutex-guarded store that persists on every change.

Build events link to deploys. When a deploy arrives whose image matches a `started` build event
and whose tag equals the first 12 characters of the build's commit, set that build's status to
`ok` and its Message to "deployed".

## Behaviour

### Deploy (CI → Ferry)

`Deploy(image, tag, source, dryRun)`:

- Image matching and patching:
  - `want = image + ":" + tag`.
  - Do every cluster in parallel. Within a cluster, go through every configured namespace and
    every kind (Deployment, StatefulSet, DaemonSet, CronJob), and list the objects.
  - A container "runs" the image when its `image` is exactly `image`, or starts with `image + ":"`
    or `image + "@"`. Check both `initContainers` and `containers`.
  - Pod spec path: `/spec/template/spec`, or for CronJob `/spec/jobTemplate/spec/template/spec`.
- A workload that runs the image gets one Result:
  - `current`: every matching container is already `want`.
  - `argo-cd`: the object has the annotation `argocd.argoproj.io/tracking-id`. Do not touch it;
    the git bump in `hack/bump.sh` handles it.
  - `paused`: the annotation `<prefix>/auto-deploy` is `"false"`.
  - `would-deploy`: dry run.
  - `deployed`: otherwise. Send one JSON Patch (`application/json-patch+json`) with
    `?fieldManager=ferry`:
    - per changed container: `{"op":"test","path":<p>,"value":<old>}` and
      `{"op":"replace","path":<p>,"value":want}`;
    - an `add` of `/metadata/annotations` (`{}`) if there are none;
    - an `add` of `/metadata/annotations/kubernetes.io~1change-cause` = `"<want> <source>"`, trimmed.
  - `error`: listing or patching failed. List errors become a Result with only Cluster, Namespace,
    Kind and Error set.
- Record one Event (kind `deploy`) with all Results. Its status is:
  - `ok` when nothing failed;
  - `partial` when some workloads failed and some deployed;
  - `error` when the deploy only failed;
  - `skipped` when nothing ran the image.

  Then link the build (see above).

### Manual rollout, rollback, pause (UI → Ferry)

- **Rollout**: `{cluster, namespace, kind, name, container, image}`. GET the object.
  - Refuse if it is Argo CD tracked: 409 "managed by Argo CD; change it in git".
  - Patch that one container with test+replace and set change-cause to `"<image> rolled out by <user>"`.
  - Ignores `paused`. Event kind `rollout`.
- **Rollback**: given a deploy, rollout or rollback event, for every Result with
  `Result=="deployed"`, set the container back from `To` to `From`.
  - The `test` op checks the container is still at `To`. If it moved on, that Result is an error
    ("changed since").
  - Event kind `rollback`. Its Message references the original event.
- **Pause/resume**: merge-patch `metadata.annotations["<prefix>/auto-deploy"]` to `"false"`, or to
  `null` to remove it. Refuse Argo CD tracked objects. Event kind `pause` or `resume`.

### Workload inventory

`GET /api/workloads` lists every workload in the configured namespaces of every cluster, in
parallel. Cache it for 10 seconds; `?fresh=1` bypasses the cache. Per workload:

```json
{"cluster":"de","namespace":"apps","kind":"Deployment","name":"starmoe-api",
 "containers":[{"name":"app","image":"docker.cnb.cool/starmoe-org/ci/starmoe-api:1a2b3c4d5e6f","init":false}],
 "desired":1,"ready":1,"argocd":false,"paused":false,
 "changeCause":"... https://github.com/...","created":"2026-10-01T00:00:00Z","schedule":""}
```

- `ready` / `desired`: from `status.readyReplicas` / `spec.replicas` for Deployments and
  StatefulSets, and from `numberReady` / `desiredNumberScheduled` for DaemonSets. CronJobs show the
  schedule, `suspend` and `lastScheduleTime` instead.
- Also return the cluster status: name, reachable, error, k8s version (`GET /version`) and
  workload count. A cluster that fails must not fail the whole request.

### Builds (CNB)

Trigger: a GitHub push webhook for a source with `builder: cnb`. The branch must match and
`deleted` must be false. A build can also be started from the UI with "Build now", which resolves
the branch head with `GET /repos/{repo}/commits/{branch}` using the owner's GitHub token.

Queue builds and run them on 2 workers. Steps, exactly as ci-relay's `mirror` and `start_build` do:

1. In `workDir/<owner>__<name>`, run `git init` if needed. Fetch with `--depth=1` from
   `https://github.com/<repo>.git <sha>`. If `github.tokens[owner]` exists, send
   `http.extraHeader=Authorization: Basic base64("x-access-token:<token>")`.
2. `git commit-tree <sha>^{tree} -m "<repo>@<sha>"`, with author and committer "ferry" and the
   snapshot author email or `ferry@localhost`. This gives a single parentless commit.
3. Force-push it to `<cnb.git>/<cnb.repo>.git` as `refs/heads/<branchPrefix><image>`, with header
   basic `cnb:<token>`. Then run `git gc --prune=now`.
4. `POST <cnb.api>/<cnb.repo>/-/build/start` with bearer token and JSON
   `{"branch","sha":<commit>,"event":"api_trigger_build","env":{"IMAGE_NAME","SRC_REPO","SRC_SHA","BUILD_ARGS"},"config":<pipelineFile with __BRANCH__ replaced by the branch>}`.
   - The response has `buildLogUrl` and `sn`.
   - The event goes `queued` → `started`, with LogURL, or → `error` with a short message.
     Never put tokens in messages; git's stderr is safe.

Then poll `GET <cnb.api>/<cnb.repo>/-/build/status/<sn>` (bearer token, `Accept: application/json`)
every 15 s for up to 45 minutes; the reply is
`{"status":"success|error|...","pipelinesStatus":{"<sn>-001":{"stages":[{"id","name","status"}]}}}`.
Store `sn` on the build event (`buildSn`) and resume polling after a restart.

- `success`: unless `cnb.deployOnSuccess` is false, `Deploy(<imageRef>, commit[:12],
  "https://github.com/<repo>/commit/<sha>")` as actor `cnb`. The build event becomes `ok` with
  "deployed" or "built; nothing runs this image", or takes the deploy's `error`/`partial`.
- `error`, `cancel`, `timeout`: the build event becomes `error`, naming the failed stage.
- A build event that something else already settled (a deploy from the pipeline) is left alone.

The pipeline cannot deploy by itself: the `CNB_TOKEN` inside a build is accepted by CNB only from
CNB's runners, so `gitRead` rejects it when the pipeline calls `POST /deploy`. Its last stage only
bumps manifests kept in git (`BUMP_GIT_ONLY=1 hack/bump.sh`).

Sources with `builder: external` ignore webhooks. The UI shows the CI snippet for them instead.

### GitHub

- **Webhooks**: `POST /webhook/github` and `POST /` (legacy). Verify `X-Hub-Signature-256` with
  HMAC-SHA256 and a constant-time compare (401 "bad signature").
  - `ping` → 200 "pong".
  - Not `push` → 204.
  - Push → 202 "queued" or 202 "skipped".
  - Limit bodies to 25 MB.
- **Webhook setup** from the UI:
  - `GET /api/sources/{id}/webhook` lists the repo's hooks with the owner's token. It reports
    `ok` (a hook whose `config.url` is `publicURL + "/webhook/github"` or `publicURL + "/"`),
    `missing`, or `no-permission` (403/404: the token lacks the Webhooks permission).
  - `POST` creates one: `{"config":{"url":publicURL+"/webhook/github","content_type":"json","secret":...},"events":["push"],"active":true}`.
  - When there is no permission, the UI shows manual steps: the payload URL, content type json,
    "use Ferry's webhook secret", and push events only. **Never** send the secret to the browser.

### Snapshots

Port `snapshot()` from `docs/reference/relay.py`. Every day at `hourUTC`, and on demand:

- Shallow-clone the repo with auth by header, not by a token in the URL.
- For each cluster and namespace, write:
  - `<dir>/<cluster>/<namespace>.yaml`: everything except Secrets;
  - `<dir>/<cluster>/<namespace>.secrets.enc.yaml`: the Secrets, then run `sops -e -i` on it;
  - `<dir>/<cluster>/<namespace>.secrets.sha256`: a digest of the plaintext, so unchanged Secrets
    are not re-encrypted (sops output differs every run).
- Layout migration:
  - Remove the legacy `secrets.enc.yaml` and `secrets.sha256` from each cluster directory.
  - `apps.yaml` keeps its name, because it is `<namespace>.yaml` for `apps`.
- The kinds, the cleaning rules (dropped annotations and server fields, skipped secret types, skip
  objects with ownerReferences, and `kube-root-ca.crt`) and the 404-means-skip behaviour are as in
  relay.py.
- Also write `<dir>/ferry.json` with the sources. Never write the tokens.
- Commit only if `git diff --cached` shows changes, with message
  `snapshot: <namespaces joined by ", ">`, then push. Event kind `snapshot`, status `ok`, with
  Message `committed` or `unchanged`.
- One snapshot at a time. An extra request while one is running joins it.

## HTTP API

The SPA is served from `/` with a history-API fallback to `index.html`, for any GET that is not
under `/api`, `/auth`, `/webhook` or `/healthz`. Assets get long cache headers and `index.html`
gets `no-cache`.

| Method and path | Auth | Notes |
|---|---|---|
| `GET /healthz` | none | `ok` |
| `POST /` , `POST /webhook/github` | HMAC | GitHub webhook |
| `POST /deploy` | CI | legacy; same as below |
| `POST /api/deploy` | CI or session | `{image, tag, source}` → 200 JSON array of Results, or 500 if any error |
| `POST /api/deploy/preview` | session | `{image, tag}` → dry-run Results |
| `POST /snapshot`, `POST /api/snapshot` | CI or session | 202 |
| `GET /auth/login` `GET /auth/callback` `POST /auth/logout` | – | OIDC |
| `GET /api/me` | session | `{user, name, groups}`; 401 when signed out |
| `GET /api/overview` | session | clusters, the last 20 events, counts (sources, workloads, deploys in 24h) |
| `GET /api/workloads` | session | `{clusters:[…], workloads:[…]}` |
| `POST /api/workloads/rollout` | session | see above |
| `POST /api/workloads/pause` | session | `{cluster,namespace,kind,name,paused}` |
| `GET/POST /api/sources`, `PUT/DELETE /api/sources/{id}` | session | validate repo `^[\w.-]+/[\w.-]+$`, image, and builder availability; repo+branch is unique |
| `POST /api/sources/{id}/build` | session | 202 with the event |
| `GET/POST /api/sources/{id}/webhook` | session | |
| `GET /api/events?limit=&kind=&image=` | session | |
| `POST /api/events/{id}/rollback` | session | |
| `GET/POST /api/tokens`, `DELETE /api/tokens/{id}` | session | POST returns the token once: `fy_` + 32 random bytes in base62 or hex |
| `GET /api/settings` | session | public summary: publicURL, namespaces, builders on, snapshot on (repo, hour), deployAuth modes, version |

General API rules:

- Errors are JSON, `{"error": "…"}`, except the legacy text replies listed in Compatibility.
- Validation:
  - image `^[a-z0-9./_-]+$` (and must not end with `:`);
  - tag `^[A-Za-z0-9._-]{1,128}$`;
  - names follow the Kubernetes rules.
- Request bodies are capped at 1 MB, except webhooks.

## Authentication

**Browser:** OpenID Connect authorization code flow with PKCE (S256), `state` and `nonce`.

- The issuer is Zeabur Identity (`https://login.star.moe`).
  - Discovery and JWKS are standard. ID tokens are ES256 by default.
  - The `groups` scope yields a `groups` claim.
  - Admins carry the group `zeabur:admins`.
  - The client is confidential, with redirect URI `<publicURL>/auth/callback`.
- Keep the in-flight state, nonce and verifier in a short-lived, signed, HttpOnly cookie.
- After verifying the ID token (issuer, audience = clientID, expiry, nonce), check
  allowedGroups or allowedUsers, then set a session cookie:
  - HMAC-signed JSON `{sub, user, name, groups, exp}`, valid for 12 h;
  - `HttpOnly`, `Secure` when publicURL is https, `SameSite=Lax`.
- Show users who are not allowed a friendly 403 page that names the account.
- **CSRF:** mutating `/api` requests must carry `Content-Type: application/json`. This is in
  addition to SameSite=Lax.
- Do not store access or refresh tokens. Ferry acts with its own ServiceAccount tokens; the
  session is only who may use the UI.

**CI:** `Authorization: Bearer <token>`. A request is accepted when either:

- the token's SHA-256 matches a deploy token (update its LastUsed; the actor is `token:<name>`); or
- `deployAuth.gitRead` accepts it (the actor is `ci`).

Otherwise reply 401 with the text `needs a deploy token or a token that can read <url>`.

## Compatibility

These are already in use and must keep working unchanged:

1. **`POST /deploy`** from `hack/bump.sh` (see `docs/reference/bump.sh`):
   - Request: Bearer CNB token (gitRead) and JSON `{"image","tag","source"}`.
   - Success: 200, and the body is printed by CI.
   - Errors: 400 text `expected {"image", "tag", "source"}` / `bad image or tag`, 401 text, and
     500 when any Result failed. bump.sh retries 3 times on anything but 200.
   - `source` is truncated to 300 characters.
2. **`POST /`**: GitHub webhooks of about 10 private repositories already point at
   `https://ci.star.moe/` with the existing secret.
3. **`POST /snapshot`** with a CI token returns 202.
4. **`clustersFile`**: JSON `{"<name>": {"server": "...", "ca": "<base64 PEM>", "token": "..."}}`.
5. **`seedSources`**: ci-relay's `repos.json`, `{"owner/name": {"branch": "main", "image": "name", "build_args": "..."}}`.
   - Import each entry as a `cnb` source.
6. **`pipelineFile`**: `docs/reference/pipeline.yml`.
   - Its `__BRANCH__` placeholder and the env names IMAGE_NAME, SRC_REPO, SRC_SHA and BUILD_ARGS
     are fixed.
7. **Patch semantics**:
   - test+replace JSON patches;
   - the change-cause annotation;
   - Argo CD tracked objects are skipped.

   Zeabur reads change-cause in its deploy history. Zeabur refuses to edit objects with Argo CD
   markers, or with managedFields managers starting with `argocd`, `flux` or `helm`, so the field
   manager must be `ferry`.

## RBAC Ferry runs with

In each cluster, Ferry acts as a ServiceAccount with a Role in each configured namespace:

- `get` and `list` on everything;
- `patch` on `deployments`, `statefulsets`, `daemonsets` and `cronjobs`.

There is no `watch`, `create`, `delete` or pod access. Every feature must work inside that. For
the ConfigMap state store, Ferry's own namespace on its home cluster also needs `get`, `create`
and `update` on `configmaps`; put that in `deploy/`.

## UI

**Stack:**

- Vite, React 19 and TypeScript, built to `web/dist`.
- `react-router` for routing.
- `lucide-react` for icons.
- Plain CSS with custom properties, no Tailwind.
- Ask before adding other runtime dependencies.
- Data fetching is a tiny `api()` helper with plain `fetch`. Any 401 redirects to `/auth/login`.

**Look: Claude.ai.** Warm, quiet, editorial. Use the visual language only, not Anthropic's logo or
fonts; Ferry gets its own simple mark, for example a small boat or wave.

- Light colours:
  - background `#FAF9F5`;
  - sidebar `#F5F4EE`;
  - cards `#FFFFFF`;
  - text `#141413`;
  - secondary text `#5E5D59`;
  - muted text `#8A8880`;
  - borders `rgba(31,30,29,.12)`;
  - accent `#C96442` (hover `#B5573A`);
  - success `#5A7D4F`, warning `#B07A1E`, danger `#B33A3A`.
- Dark colours (`prefers-color-scheme` plus a manual toggle saved in localStorage):
  - background `#262624`;
  - sidebar `#1F1E1D`;
  - cards `#30302E`;
  - text `#F5F4EE`;
  - secondary `#C2C0B6`;
  - borders `rgba(222,220,209,.14)`;
  - the same accent.
- Type:
  - headings in a serif stack: `ui-serif, "Tiempos Text", Georgia, "Noto Serif SC", "Songti SC", serif`;
  - body in a sans stack: `ui-sans-serif, system-ui, -apple-system, "Segoe UI", "PingFang SC", "Noto Sans SC", sans-serif`;
  - monospace for images and tags.
  - Body is 15px, line-height 1.6.
- Shapes and motion:
  - corner radius 12px for cards, 10px for inputs, 8px for buttons; pills are fully rounded;
  - thin 1px borders, almost no shadows except the composer;
  - 150ms transitions;
  - generous white space;
  - the content column is at most about 880px wide and centred.
- **Layout:**
  - **Left sidebar** (260px, collapsible to icons; a drawer on mobile):
    - Ferry mark and name;
    - a prominent "+ New source" button, like "New chat";
    - nav: Home, Services, Sources, Activity, Settings;
    - at the bottom, the user avatar (initial on an accent circle) with name, a menu for theme,
      language and sign out.
- **Home**:
  - A large serif greeting with the time of day and the user's name, for example "Good evening,
    exmeaning", next to the Ferry mark in accent colour.
  - Below it, a **composer**: a rounded, elevated input like Claude's prompt box. The placeholder
    is "Deploy an image… e.g. ghcr.io/org/app:1a2b3c".
    - Enter runs a preview: a list of which workloads would change (cluster · namespace/name ·
      container · from → to).
    - A "Deploy" button then confirms.
    - Suggestions under the composer are chips with the 3 most recent deployed images.
  - Then the cluster status chips (name, version, workload count, a red dot if unreachable).
  - Then "Recent activity": the last 10 events, compact.
- **Services**:
  - Workloads grouped by image repository.
    - A group header shows the image, a linked source (repo@branch) if any, and the tags in use.
    - Rows show:
      - cluster and namespace/name;
      - kind;
      - container;
      - the tag (monospace pill);
      - ready/desired, with a coloured dot;
      - badges for "Argo CD" and "Paused";
      - the change-cause link, if any.
  - Row menu:
    - "Deploy tag…": a modal with a text input and recent tags for that image from events.
    - "Pause/Resume auto-deploy".
    - Both are disabled, with a tooltip, for Argo CD rows.
  - A search box filters by name, image or cluster, plus a cluster filter.
  - Workloads not linked to any source still show; they are just not linked.
- **Sources**:
  - A list of cards: repo (link to GitHub), branch, builder badge, image ref, webhook status
    (loaded lazily), the last build event with a log link, and which services run it (count, which
    links to Services filtered).
  - Actions: Build now (cnb), Set up webhook, Edit, Delete (confirm).
  - "New source" is a modal:
    - fields: repo, branch (default main), builder (only those configured), image name, build args;
    - for `external`, a copyable CI snippet calling `POST <publicURL>/deploy`, with a GitHub Actions
      example.
- **Activity**:
  - A timeline of events with an icon per kind, status colour, actor and relative time, and an
    expandable list of Results.
  - Filters for kind and image.
  - "Roll back" on deploy, rollout and rollback events that have deployed results, behind a
    confirm dialog listing what will change.
  - Polls every 15 s while visible.
- **Settings**:
  - Deploy tokens: list, create (the token is shown once with a copy button and a warning),
    revoke.
  - Clusters: read-only status.
  - Snapshots: config summary, "Snapshot now", and the last snapshot event.
  - "Connect CI": copyable examples for curl and GitHub Actions.
- Empty states, loading skeletons, toasts for results, and confirms for destructive actions.
- Keyboard: `/` focuses the search or composer; Esc closes modals.
- **i18n**: English and 简体中文, auto-detected from `navigator.language`, switchable, using a small
  dictionary module (no i18n library). Show times relative ("3 min ago" / "3 分钟前") with the
  absolute time in a tooltip.
- **Accessibility**: real buttons and labels, focus rings in accent, contrast AA.

## Development without a cluster

The implementer cannot reach the real clusters, so this must exist and the tests must use it:

- **`internal/kube/fake`**: an `httptest.Server` that serves
  - `GET /version`;
  - list and get of the four workload kinds, Services, ConfigMaps, Secrets, PVCs, Ingresses and
    Traefik Middlewares (404 for unknown kinds);
  - `PATCH` with `application/json-patch+json` (`test`, `replace` and `add` are enough; a failed
    `test` → 422) and `application/merge-patch+json`;
  - a ConfigMap get, create and update with resourceVersion conflicts;
  - bearer token checks, and an RBAC-ish switch to return 403 for a kind.
- **`ferry -demo`**:
  - starts 2–3 fake clusters seeded with realistic workloads, including one Argo CD tracked,
    one paused, a CronJob and a DaemonSet;
  - uses a file state in a temp dir and `auth.disabled`;
  - does not start the CNB builder, or uses a fake one.

  `npm run dev` proxies `/api` to it. This is how the UI is built and shown in screenshots.
- **Tests (`go test ./...`)**, at least:
  - image matching (`runs`), including `@sha256` and registry ports such as `host:5000/img:tag`;
  - every Result kind;
  - the test+replace patch body;
  - CronJob paths;
  - initContainers;
  - change-cause when annotations is nil;
  - rollback, including "changed since";
  - pause and resume;
  - webhook HMAC;
  - gitRead auth with a fake git server and the cache;
  - deploy tokens;
  - repos.json seed;
  - clustersFile parsing;
  - snapshot cleaning, golden-file style;
  - ConfigMap store conflicts;
  - the OIDC callback, against a fake issuer that serves discovery and JWKS and signs an ES256
    ID token;
  - SPA fallback routing.

## Packaging

- `Dockerfile`, multi-stage:
  - Node builds the SPA.
  - Go builds a static binary (`CGO_ENABLED=0`, `-trimpath -ldflags "-s -w -X main.version=…"`).
  - The final stage is `alpine` with `git`, `ca-certificates`, `tini` and `sops` (pinned version
    and SHA-256, see `docs/reference/relay.Dockerfile`).
  - Run as a non-root user with a `/work` volume. `ENTRYPOINT ["tini","--","ferry"]`.
  - Use `--platform=$BUILDPLATFORM` for the build stages, so multi-arch builds cross-compile
    instead of emulating.
- A `Makefile` with `web`, `build`, `test`, `demo` and `image`.
- `.github/workflows/ci.yml`: `go vet`, `go test -race`, `npm ci && npm run build && npm run lint`
  (if there is a lint script), and `docker build`.
- `.github/workflows/release.yml`: on `v*` tags, multi-arch (amd64, arm64) images to
  `ghcr.io/<owner>/ferry`.
- `deploy/`: Namespace, ServiceAccount, the Role and RoleBinding for the state ConfigMap, a
  Deployment (readiness `/healthz`, about 64Mi memory), a Service, an example config ConfigMap, a
  Secret template, and the remote-cluster RBAC from `docs/reference/ci-deployer.yaml`, generalised.
- `README.md` for the open-source audience:
  - what it is, and the Argo CD + dashboard split;
  - a screenshot from demo mode;
  - quick start (demo, Kubernetes);
  - configuration reference;
  - the CI contract (curl and Actions);
  - security notes.

  Add a `LICENSE` (Apache-2.0 unless the owner says otherwise).

## Security checklist

- Never log or return:
  - tokens, the webhook secret or the client secret;
  - Secret contents. Snapshots only write them to files that sops encrypts.
- Constant-time compares for HMAC and token hashes.
- Git credentials go in `http.extraHeader` via `-c`, never in URLs or argv that might be logged in
  errors. Scrub error strings before storing them in events.
- All Kubernetes paths are built from validated names.
- The session cookie is signed. The OIDC state cookie is signed and expires in 10 minutes.
- Return 404 instead of 403 for unknown API routes.

## Definition of done

- `go vet ./...` and `go test -race ./...` pass.
- `cd web && npm ci && npm run build` passes with no TypeScript errors.
- `docker build .` passes.
- `ferry -demo` runs and every page works against fake clusters, in both languages and both
  themes.
- The README has screenshots, and all of Compatibility is covered by tests.
- No secrets, internal IPs or real tokens in the repository.
  - `docs/reference/` stays until review. The owner deletes it before the repository goes public.
