"""GitHub push webhook -> CNB build relay, deploys and snapshots.

GitHub can't call the CNB API directly (no custom auth header), so this
receives the push, mirrors the pushed tree into a branch of StarMoe-org/ci
as a single parentless commit, and starts a CNB build on it.

POST /deploy {"image", "tag", "source"} (from infra's hack/bump.sh, with a CNB
token that can read StarMoe-org/infra) points every workload in namespace apps
of every cluster that runs <image> at <image>:<tag>. Workloads Argo CD still
tracks are left to its git bump; the rest are Zeabur's to edit.

Every night, and on POST /snapshot, namespace apps of every cluster is written
to snapshots/<cluster>/ in StarMoe-org/infra (Secrets SOPS-encrypted): a record
of what runs, not applied by anything.
"""
import base64
import hashlib
import hmac
import json
import logging
import os
import queue
import re
import ssl
import subprocess
import tempfile
import threading
import time
import urllib.error
import urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(message)s")
log = logging.getLogger("relay")

SECRET = os.environ["WEBHOOK_SECRET"].encode()
CNB_TOKEN = os.environ["CNB_TOKEN"]
CNB_REPO = os.environ.get("CNB_REPO", "StarMoe-org/ci")
GH_TOKENS = json.loads(os.environ["GH_TOKENS"])  # {"owner": "token"}
REPOS = json.load(open(os.environ.get("REPOS_FILE", "/etc/relay/repos.json")))
PIPELINE = open(os.environ.get("PIPELINE_FILE", "/etc/relay/pipeline.yml")).read()
WORK = os.environ.get("WORK_DIR", "/work")
# {"<cluster>": {"server", "ca" (base64 PEM), "token"}}: platform/ci-deployer's ServiceAccount
CLUSTERS = json.load(open(os.environ.get("CLUSTERS_FILE", "/etc/relay-clusters/clusters.json")))
INFRA = os.environ.get("INFRA_REPO", "StarMoe-org/infra")
SNAPSHOT_HOUR = int(os.environ.get("SNAPSHOT_HOUR_UTC", "19"))  # 03:00 in China
NS = "apps"

jobs = queue.Queue()


def git(*args, cwd):
    subprocess.run(["git", *args], cwd=cwd, check=True, capture_output=True, text=True)


def git_out(*args, cwd):
    return subprocess.run(["git", *args], cwd=cwd, check=True, capture_output=True, text=True).stdout.strip()


def basic(user, token):
    return "Authorization: Basic " + base64.b64encode(f"{user}:{token}".encode()).decode()


def cnb_header():
    return basic("cnb", CNB_TOKEN)


def mirror(full_name, sha, branch):
    owner = full_name.split("/")[0]
    repo_dir = os.path.join(WORK, full_name.replace("/", "__"))
    os.makedirs(repo_dir, exist_ok=True)
    if not os.path.isdir(os.path.join(repo_dir, ".git")):
        git("init", "-q", cwd=repo_dir)
    gh = basic("x-access-token", GH_TOKENS[owner])
    git("-c", f"http.extraHeader={gh}", "fetch", "-q", "--depth=1",
        f"https://github.com/{full_name}.git", sha, cwd=repo_dir)
    tree = git_out("rev-parse", f"{sha}^{{tree}}", cwd=repo_dir)
    env = dict(os.environ, GIT_AUTHOR_NAME="ci-relay", GIT_AUTHOR_EMAIL="mail@exmeaning.com",
               GIT_COMMITTER_NAME="ci-relay", GIT_COMMITTER_EMAIL="mail@exmeaning.com")
    commit = subprocess.run(["git", "commit-tree", tree, "-m", f"{full_name}@{sha}"], cwd=repo_dir,
                            env=env, check=True, capture_output=True, text=True).stdout.strip()
    git("-c", f"http.extraHeader={cnb_header()}", "push", "-q", "--force",
        f"https://cnb.cool/{CNB_REPO}.git", f"{commit}:refs/heads/{branch}", cwd=repo_dir)
    git("gc", "-q", "--prune=now", cwd=repo_dir)
    return commit


def start_build(branch, commit, env):
    body = json.dumps({
        "branch": branch,
        "sha": commit,
        "event": "api_trigger_build",
        "env": env,
        "config": PIPELINE.replace("__BRANCH__", branch),
    }).encode()
    req = urllib.request.Request(f"https://api.cnb.cool/{CNB_REPO}/-/build/start", data=body, method="POST",
                                 headers={"Authorization": f"Bearer {CNB_TOKEN}",
                                          "Content-Type": "application/json", "Accept": "application/json"})
    with urllib.request.urlopen(req, timeout=30) as r:
        return json.loads(r.read() or b"{}")


# --- Kubernetes -------------------------------------------------------------

def k8s(cluster, method, path, body=None, content_type="application/json"):
    c = CLUSTERS[cluster]
    if "_ctx" not in c:
        c["_ctx"] = ssl.create_default_context(cadata=base64.b64decode(c["ca"]).decode())
    req = urllib.request.Request(c["server"] + path, method=method,
                                 data=None if body is None else json.dumps(body).encode(),
                                 headers={"Authorization": f"Bearer {c['token']}", "Accept": "application/json",
                                          "Content-Type": content_type})
    with urllib.request.urlopen(req, timeout=30, context=c["_ctx"]) as r:
        return json.loads(r.read() or b"{}")


WORKLOADS = [("apis/apps/v1", "deployments"), ("apis/apps/v1", "statefulsets"),
             ("apis/apps/v1", "daemonsets"), ("apis/batch/v1", "cronjobs")]
ARGO = "argocd.argoproj.io/tracking-id"


def pod_spec(kind, obj):
    if kind == "cronjobs":
        return "/spec/jobTemplate/spec/template/spec", obj["spec"]["jobTemplate"]["spec"]["template"]["spec"]
    return "/spec/template/spec", obj["spec"]["template"]["spec"]


def runs(ref, image):
    return ref == image or ref.startswith(image + ":") or ref.startswith(image + "@")


def deploy(image, tag, source):
    """Points every container running <image> at <image>:<tag>; returns what it did, per workload."""
    want = f"{image}:{tag}"
    done = []
    for cluster in CLUSTERS:
        for group, kind in WORKLOADS:
            try:
                items = k8s(cluster, "GET", f"/{group}/namespaces/{NS}/{kind}")["items"]
            except Exception as e:
                done.append({"cluster": cluster, "kind": kind, "error": str(e)})
                continue
            for obj in items:
                meta = obj["metadata"]
                prefix, spec = pod_spec(kind, obj)
                ops = []
                for field in ("initContainers", "containers"):
                    for i, ct in enumerate(spec.get(field) or []):
                        if runs(ct["image"], image) and ct["image"] != want:
                            path = f"{prefix}/{field}/{i}/image"
                            ops += [{"op": "test", "path": path, "value": ct["image"]},
                                    {"op": "replace", "path": path, "value": want}]
                if not ops:
                    continue
                where = {"cluster": cluster, "kind": kind, "name": meta["name"]}
                if ARGO in (meta.get("annotations") or {}):
                    done.append({**where, "result": "argo-cd"})
                    continue
                if not meta.get("annotations"):
                    ops.append({"op": "add", "path": "/metadata/annotations", "value": {}})
                ops.append({"op": "add", "path": "/metadata/annotations/kubernetes.io~1change-cause",
                            "value": f"{want} {source}".strip()})
                try:
                    k8s(cluster, "PATCH", f"/{group}/namespaces/{NS}/{kind}/{meta['name']}?fieldManager=ci-relay",
                        ops, "application/json-patch+json")
                    done.append({**where, "result": "deployed"})
                    log.info("deployed %s to %s/%s/%s", want, cluster, kind, meta["name"])
                except urllib.error.HTTPError as e:
                    done.append({**where, "error": f"{e.code} {e.read()[:300].decode(errors='replace')}"})
    return done


# --- Snapshots --------------------------------------------------------------

SNAPSHOT_KINDS = WORKLOADS + [("api/v1", "services"), ("api/v1", "configmaps"), ("api/v1", "secrets"),
                              ("api/v1", "persistentvolumeclaims"), ("apis/networking.k8s.io/v1", "ingresses"),
                              ("apis/traefik.io/v1alpha1", "middlewares")]
DROP_ANNOTATIONS = re.compile(r"^(kubectl\.kubernetes\.io/last-applied-configuration|argocd\.argoproj\.io/.*"
                              r"|deployment\.kubernetes\.io/revision|pv\.kubernetes\.io/.*|volume\..*)$")
SKIP_SECRET_TYPES = {"kubernetes.io/service-account-token", "kubernetes.io/tls", "helm.sh/release.v1"}


def clean(group, kind, obj):
    meta = obj["metadata"]
    if meta.get("ownerReferences"):
        return None
    if kind == "configmaps" and meta["name"] == "kube-root-ca.crt":
        return None
    if kind == "secrets" and obj.get("type") in SKIP_SECRET_TYPES:
        return None
    ann = {k: v for k, v in (meta.get("annotations") or {}).items() if not DROP_ANNOTATIONS.match(k)}
    out = {"apiVersion": group.split("/", 1)[1] if group.startswith("apis/") else "v1",
           "kind": {"persistentvolumeclaims": "PersistentVolumeClaim", "configmaps": "ConfigMap",
                    "statefulsets": "StatefulSet", "daemonsets": "DaemonSet", "cronjobs": "CronJob",
                    "ingresses": "Ingress"}.get(
               kind, kind[:-1].capitalize()),
           "metadata": {"name": meta["name"], "namespace": NS}}
    if meta.get("labels"):
        out["metadata"]["labels"] = meta["labels"]
    if ann:
        out["metadata"]["annotations"] = ann
    for key in ("type", "data", "stringData", "spec"):
        if key in obj:
            out[key] = obj[key]
    if kind == "services":
        for k in ("clusterIP", "clusterIPs", "ipFamilies", "ipFamilyPolicy", "internalTrafficPolicy"):
            out["spec"].pop(k, None)
    return out


def snapshot():
    import yaml
    d = tempfile.mkdtemp(dir=WORK)
    url = f"https://cnb.cool/{INFRA}.git"
    try:
        git("-c", f"http.extraHeader={cnb_header()}", "clone", "-q", "--depth=1", url, d, cwd=WORK)
        for cluster in CLUSTERS:
            docs, secrets = [], []
            for group, kind in SNAPSHOT_KINDS:
                try:
                    items = k8s(cluster, "GET", f"/{group}/namespaces/{NS}/{kind}")["items"]
                except urllib.error.HTTPError as e:
                    if e.code == 404:
                        continue
                    raise
                for obj in sorted(items, key=lambda o: o["metadata"]["name"]):
                    c = clean(group, kind, obj)
                    if c:
                        (secrets if kind == "secrets" else docs).append(c)
            out = os.path.join(d, "snapshots", cluster)
            os.makedirs(out, exist_ok=True)
            with open(os.path.join(out, "apps.yaml"), "w") as f:
                f.write(f"# Namespace {NS} of {cluster}, written by ci-relay. A record, not applied by anything.\n")
                yaml.safe_dump_all(docs, f, sort_keys=False, allow_unicode=True)
            # Re-encrypting unchanged Secrets would still change the file, and only the age public key is
            # here to decrypt with, so a hash of the plaintext says whether they changed
            path = os.path.join(out, "secrets.enc.yaml")
            new = yaml.safe_dump_all(secrets, sort_keys=False, allow_unicode=True)
            digest = hashlib.sha256(new.encode()).hexdigest()
            digest_path = os.path.join(out, "secrets.sha256")
            old = open(digest_path).read().strip() if os.path.exists(digest_path) else None
            if digest != old or not os.path.exists(path):
                with open(path, "w") as f:
                    f.write(new)
                subprocess.run(["sops", "-e", "-i", path], cwd=d, check=True, capture_output=True, text=True)
                with open(digest_path, "w") as f:
                    f.write(digest + "\n")
        git("add", "-A", "snapshots", cwd=d)
        if subprocess.run(["git", "diff", "--cached", "--quiet"], cwd=d).returncode == 0:
            return "unchanged"
        env = dict(os.environ, GIT_AUTHOR_NAME="ci-relay", GIT_AUTHOR_EMAIL="mail@exmeaning.com",
                   GIT_COMMITTER_NAME="ci-relay", GIT_COMMITTER_EMAIL="mail@exmeaning.com")
        subprocess.run(["git", "commit", "-qm", "snapshot: namespace apps"], cwd=d, env=env, check=True,
                       capture_output=True, text=True)
        git("-c", f"http.extraHeader={cnb_header()}", "push", "-q", url, "HEAD:main", cwd=d)
        return "committed"
    finally:
        subprocess.run(["rm", "-rf", d])


def snapshot_loop():
    while True:
        now = time.gmtime()
        wait = ((SNAPSHOT_HOUR - now.tm_hour) % 24) * 3600 - now.tm_min * 60 - now.tm_sec
        time.sleep(wait if wait > 0 else wait + 86400)
        snapshot_jobs.put(None)


snapshot_jobs = queue.Queue()


def snapshot_worker():
    while True:
        snapshot_jobs.get()
        try:
            log.info("snapshot: %s", snapshot())
        except subprocess.CalledProcessError as e:
            log.error("snapshot failed: %s", (e.stderr or "").strip()[-500:])
        except Exception:
            log.exception("snapshot failed")


# Tokens that could read the infra repository, by hash, for ten minutes
_allowed = {}


def may_deploy(token):
    key = hashlib.sha256(token.encode()).hexdigest()
    if _allowed.get(key, 0) > time.time():
        return True
    req = urllib.request.Request(f"https://cnb.cool/{INFRA}.git/info/refs?service=git-upload-pack",
                                 headers={"Authorization": basic("cnb", token)[len("Authorization: "):]})
    try:
        with urllib.request.urlopen(req, timeout=20) as r:
            ok = r.status == 200
    except urllib.error.HTTPError:
        ok = False
    if ok:
        _allowed[key] = time.time() + 600
    return ok


def worker():
    while True:
        full_name, sha, cfg = jobs.get()
        branch = f"src/{cfg['image']}"
        try:
            commit = mirror(full_name, sha, branch)
            env = {"IMAGE_NAME": cfg["image"], "SRC_REPO": full_name, "SRC_SHA": sha,
                   "BUILD_ARGS": cfg.get("build_args", "")}
            res = start_build(branch, commit, env)
            log.info("build started %s@%s: %s", full_name, sha[:12], res.get("buildLogUrl") or res)
        except subprocess.CalledProcessError as e:
            log.error("git failed for %s@%s: %s", full_name, sha[:12], (e.stderr or "").strip()[-500:])
        except Exception:
            log.exception("build failed for %s@%s", full_name, sha[:12])
        finally:
            jobs.task_done()


class Handler(BaseHTTPRequestHandler):
    def _reply(self, code, msg):
        self.send_response(code)
        self.send_header("Content-Type", "text/plain")
        self.end_headers()
        self.wfile.write(msg.encode() + b"\n")

    def do_GET(self):
        self._reply(200, "ok")

    def do_POST(self):
        body = self.rfile.read(int(self.headers.get("Content-Length", 0)))
        if self.path in ("/deploy", "/snapshot"):
            auth = self.headers.get("Authorization", "")
            if not auth.startswith("Bearer ") or not may_deploy(auth[7:].strip()):
                return self._reply(401, "needs a CNB token that can read " + INFRA)
            if self.path == "/snapshot":
                snapshot_jobs.put(None)
                return self._reply(202, "snapshot queued")
            try:
                p = json.loads(body)
                image, tag = p["image"], p["tag"]
            except (ValueError, KeyError):
                return self._reply(400, 'expected {"image", "tag", "source"}')
            if not re.fullmatch(r"[a-z0-9./_-]+", image) or not re.fullmatch(r"[A-Za-z0-9._-]+", tag):
                return self._reply(400, "bad image or tag")
            res = deploy(image, tag, str(p.get("source", ""))[:300])
            code = 500 if any("error" in r for r in res) else 200
            self.send_response(code)
            self.send_header("Content-Type", "application/json")
            self.end_headers()
            self.wfile.write(json.dumps(res).encode() + b"\n")
            return
        sig = self.headers.get("X-Hub-Signature-256", "")
        want = "sha256=" + hmac.new(SECRET, body, hashlib.sha256).hexdigest()
        if not hmac.compare_digest(sig, want):
            return self._reply(401, "bad signature")
        event = self.headers.get("X-GitHub-Event", "")
        if event == "ping":
            return self._reply(200, "pong")
        if event != "push":
            return self._reply(204, "ignored")
        p = json.loads(body)
        full_name = p["repository"]["full_name"]
        cfg = REPOS.get(full_name)
        if not cfg or p.get("deleted") or p.get("ref") != f"refs/heads/{cfg['branch']}":
            return self._reply(202, "skipped")
        jobs.put((full_name, p["after"], cfg))
        log.info("queued %s@%s", full_name, p["after"][:12])
        self._reply(202, "queued")

    def log_message(self, fmt, *args):
        pass


if __name__ == "__main__":
    for _ in range(2):
        threading.Thread(target=worker, daemon=True).start()
    threading.Thread(target=snapshot_worker, daemon=True).start()
    threading.Thread(target=snapshot_loop, daemon=True).start()
    ThreadingHTTPServer(("", 8080), Handler).serve_forever()
