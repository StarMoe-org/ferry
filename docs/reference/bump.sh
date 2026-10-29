#!/bin/sh
# Deploy <image>:<tag>, from CI (GitHub Actions and CNB builds clone this repo and run it).
# - Workloads in namespace apps, which Zeabur manages: ci-relay points them at the new image
#   (POST https://ci.star.moe/deploy, as platform/ci-deployer in each cluster).
# - Manifests still in this repository (what Argo CD applies, such as ci-relay itself): bumped,
#   committed and pushed.
# Needs CNB_TOKEN (a token that can read this repository). Usage: hack/bump.sh <image-without-tag> <tag> [source]
set -eu
img=$1 tag=$2 src=${3:-}
: "${CNB_TOKEN:?CNB_TOKEN is not set}"

body=$(printf '{"image":"%s","tag":"%s","source":"%s"}' "$img" "$tag" "$src")
for i in 1 2 3; do
  code=$(curl -sS -o /tmp/deploy.out -w '%{http_code}' -m 120 -X POST https://ci.star.moe/deploy \
    -H "Authorization: Bearer $CNB_TOKEN" -H 'Content-Type: application/json' -d "$body") || code=000
  [ "$code" = 200 ] && break
  echo "ci-relay answered $code: $(cat /tmp/deploy.out 2>/dev/null)" >&2
  [ "$i" = 3 ] && exit 1
  sleep $((i * 10))
done
echo "ci-relay: $(cat /tmp/deploy.out)"

re=$(printf '%s' "$img" | sed 's/[.[\*^$/]/\\&/g')
git config user.name Exmeaning
git config user.email mail@exmeaning.com
for i in 1 2 3 4 5; do
  git fetch -q origin main
  git reset -q --hard origin/main
  files=$(grep -rlE --include='*.yaml' "$re:[A-Za-z0-9._-]+" clusters || true)
  if [ -z "$files" ]; then echo "no manifest in this repository uses $img"; exit 0; fi
  for f in $files; do sed -i.bak -E "s/($re):[A-Za-z0-9._-]+/\1:$tag/g" "$f" && rm -f "$f.bak"; done
  if git diff --quiet; then echo "manifests already at $tag"; exit 0; fi
  git commit -qam "deploy: ${img##*/} $tag${src:+

$src}"
  if git push -q origin HEAD:main; then git --no-pager log -1 --stat --format='%h %s'; exit 0; fi
  sleep $((i * 3))
done
echo "push failed" >&2
exit 1
