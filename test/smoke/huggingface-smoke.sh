#!/usr/bin/env bash
# Builds the image, starts commandcenter, then uses the real huggingface_hub
# and transformers (python:3.12-slim, the latest releases with hf_xet as pip
# installs them) with HF_ENDPOINT at a proxy of huggingface.co and HF_TOKEN
# a Command Center API token (anonymous reads are off) to:
#   1. download a tiny model with `hf download` and snapshot_download (two
#      LFS files, one in a subfolder) and check the files' sha256
#   2. resolve files by branch, by commit and by an older commit, in a
#      subfolder and of a dataset; read a byte range (HfFileSystem)
#   3. get EntryNotFoundError, RevisionNotFoundError and
#      RepositoryNotFoundError for what is missing, and no anonymous access
#   4. load a tokenizer and a config with transformers, which probes
#      optional files that the repository does not have
#   5. download again into an empty client cache with the proxy blocked
#      from its remote: everything comes from the proxy's cache, and a
#      file never fetched before does not
#   6. try an upload: refused, the format has proxies only
# The proxy downloads about 2 MB from huggingface.co; pip installs
# huggingface_hub and transformers (no torch; about 42 MB) from pypi.org.
# Finally prints the container's memory usage.
set -euo pipefail
export MSYS_NO_PATHCONV=1
cd "$(dirname "$0")/../.."
root="$(pwd -W 2>/dev/null || pwd)"
work="test/smoke/.work-huggingface-$$"
net=commandcenter-huggingface-smoke-$$
name=commandcenter-huggingface-smoke-$$
. test/postgres.sh

cleanup() {
  docker rm -f -v "$name" "$name-pg" "$name-c" >/dev/null 2>&1 || true
  docker network rm "$net" >/dev/null 2>&1 || true
  rm -rf "$work" 2>/dev/null || true
}
trap cleanup EXIT
cleanup
rm -rf "$work" && mkdir -p "$work"

docker build -q -t commandcenter:smoke-huggingface . >/dev/null
hash="$(printf 'smoke-pass\n' | docker run --rm -i commandcenter:smoke-huggingface hash-password)"

cat > "$work/config.yaml" <<EOF
anonymousRead: false
baseURL: "http://commandcenter:8081"
users:
  - { name: admin, passwordHash: "$hash", roles: [admin] }
  - { name: ci, passwordHash: "$hash", roles: [reader] }
roles:
  reader: { write: [hf-proxy] }
repositories:
  - { name: hf-proxy, format: huggingface, type: proxy, remote: "https://huggingface.co/" }
EOF

cat > "$work/run.py" <<'EOF'
import base64, hashlib, json, os, shutil, subprocess, urllib.request

from huggingface_hub import HfApi, HfFileSystem, hf_hub_download, snapshot_download
from huggingface_hub.errors import EntryNotFoundError, HfHubHTTPError, RepositoryNotFoundError, RevisionNotFoundError
import huggingface_hub

CC = "http://commandcenter:8081"
MODEL = "hf-internal-testing/tiny-random-bert"
SKIP = ["tf_model.h5"]  # 26 MB: left for the blocked proxy to miss
api = HfApi()


def sha256(path):
    with open(path, "rb") as f:
        return hashlib.sha256(f.read()).hexdigest()


def raises(kind, fn):
    try:
        fn()
    except kind as e:
        return e
    except Exception as e:
        raise SystemExit(f"FAIL: {type(e).__name__} instead of {kind.__name__}: {e}")
    raise SystemExit(f"FAIL: no {kind.__name__}")


def block(blocked):
    body = {"name": "hf-proxy", "format": "huggingface", "type": "proxy", "remote": "https://huggingface.co/", "blocked": blocked}
    req = urllib.request.Request(CC + "/-/api/repositories/hf-proxy", data=json.dumps(body).encode(), method="PUT", headers={
        "Content-Type": "application/json", "X-Command-Center-UI": "1",
        "Authorization": "Basic " + base64.b64encode(b"admin:smoke-pass").decode()})
    urllib.request.urlopen(req).close()


print("huggingface_hub", huggingface_hub.__version__)

print("== hf download and snapshot_download of", MODEL)
subprocess.run(["hf", "download", MODEL, "config.json"], check=True, stdout=subprocess.DEVNULL)
snap = snapshot_download(MODEL, ignore_patterns=SKIP)
tree = {f.path: f for f in api.list_repo_tree(MODEL, recursive=True) if hasattr(f, "size")}
lfs = [p for p, f in tree.items() if f.lfs is not None and p not in SKIP]
assert sorted(lfs) == ["onnx/model.onnx", "pytorch_model.bin"], lfs
for path, f in tree.items():
    if path in SKIP:
        continue
    local = os.path.join(snap, path)
    assert os.path.getsize(local) == f.size, path
    if f.lfs is not None:
        assert sha256(local) == f.lfs.sha256, path
assert json.load(open(os.path.join(snap, "config.json")))["model_type"] == "bert"

print("== revisions and files")
commits = api.list_repo_commits(MODEL)
head = commits[0].commit_id
assert os.path.basename(snap) == head and api.model_info(MODEL).sha == head
assert hf_hub_download(MODEL, "config.json", revision="main") == hf_hub_download(MODEL, "config.json", revision=head)
old = next(c.commit_id for c in commits[1:] if any(f.path == "config.json" for f in api.list_repo_tree(MODEL, revision=c.commit_id)))
assert f"/snapshots/{old}/" in hf_hub_download(MODEL, "config.json", revision=old)
assert hf_hub_download(MODEL, "model.onnx", subfolder="onnx").endswith("/onnx/model.onnx")
readme = hf_hub_download("nyu-mll/glue", "README.md", repo_type="dataset")
assert "glue" in open(readme, encoding="utf-8").read().lower()
with HfFileSystem().open(MODEL + "/pytorch_model.bin") as remote, open(os.path.join(snap, "pytorch_model.bin"), "rb") as local:
    remote.seek(1000)
    local.seek(1000)
    assert remote.read(4096) == local.read(4096), "a byte range of the LFS file"

print("== what is missing, and no anonymous access")
raises(EntryNotFoundError, lambda: hf_hub_download(MODEL, "nope.txt"))
raises(RevisionNotFoundError, lambda: hf_hub_download(MODEL, "config.json", revision="no-such-revision"))
raises(RepositoryNotFoundError, lambda: hf_hub_download("hf-internal-testing/no-such-repository", "config.json"))
raises(RepositoryNotFoundError, lambda: api.model_info("hf-internal-testing/no-such-repository"))
assert api.file_exists(MODEL, "config.json") and not api.file_exists(MODEL, "nope.txt")
assert api.repo_exists(MODEL) and not api.repo_exists("hf-internal-testing/no-such-repository")
e = raises(HfHubHTTPError, lambda: HfApi(token=False).model_info(MODEL))
assert e.response.status_code == 401, e

print("== transformers: a config and a tokenizer (it probes optional files the repository does not have)")
from transformers import AutoConfig, AutoTokenizer

assert AutoConfig.from_pretrained(MODEL).model_type == "bert"
assert len(AutoTokenizer.from_pretrained(MODEL)("hello world")["input_ids"]) > 2

print("== again from the proxy's cache: the proxy is blocked from huggingface.co, the client's cache is empty")
block(True)
shutil.rmtree(os.path.expanduser("~/.cache/huggingface"))
again = snapshot_download(MODEL, ignore_patterns=SKIP)
assert again == snap
for path in lfs:
    assert sha256(os.path.join(again, path)) == tree[path].lfs.sha256, path
e = raises(HfHubHTTPError, lambda: hf_hub_download(MODEL, "tf_model.h5"))
assert e.response.status_code == 502, e
block(False)

print("== upload is refused (proxy only)")
e = raises(HfHubHTTPError, lambda: api.upload_file(path_or_fileobj=b"x", path_in_repo="x.txt", repo_id=MODEL))
assert e.response.status_code in (403, 404, 405), e
print("HUGGINGFACE CLIENT OK")
EOF

docker network create "$net" >/dev/null
start_postgres
docker run -d --name "$name" --network "$net" -e COMMANDCENTER_DATABASE_URL="$pg_url" --network-alias commandcenter --memory 128m \
  -v "$root/$work/config.yaml:/etc/commandcenter/config.yaml:ro" commandcenter:smoke-huggingface >/dev/null

ready=0
for i in $(seq 1 30); do
  if docker exec "$name" commandcenter healthcheck >/dev/null 2>&1; then ready=1; break; fi
  sleep 1
done
if [ "$ready" != 1 ]; then
  echo "FAIL: commandcenter did not become ready"; docker logs "$name"; exit 1
fi

token="$(docker exec "$name" commandcenter token create -config /etc/commandcenter/config.yaml -user ci)"
docker run --rm --name "$name-c" --network "$net" --memory 1g -v "$root/$work:/work:ro" \
  -e HF_ENDPOINT=http://commandcenter:8081/repository/hf-proxy -e HF_TOKEN="$token" -e HF_HUB_DISABLE_TELEMETRY=1 -e HF_HUB_DISABLE_PROGRESS_BARS=1 \
  python:3.12-slim sh -c 'pip install -q --disable-pip-version-check --root-user-action=ignore huggingface_hub transformers && python /work/run.py'

echo "== memory"
docker stats --no-stream --format '{{.Name}} {{.MemUsage}}' "$name"
echo "HUGGINGFACE SMOKE OK"
