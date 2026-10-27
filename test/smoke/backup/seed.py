"""Seeds and checks artifacts of every format with the standard library only.

Used by backup-smoke.sh (seed, upload loop, check). It runs in a
container on the same Docker network as the server, so BASE is http://<container>:8081.

  seed.py seed   BASE USER PASS TAG MANIFEST   upload one artifact per format, write "path sha256" lines
  seed.py check  BASE USER PASS MANIFEST       download every manifest path and compare the sha256
  seed.py loop   BASE USER PASS                upload Maven files until killed; one "UP i path sha256" line per
                                               success, one "BAD i ..." line per failure (any non-2xx or error)
  seed.py bulk   BASE USER PASS COUNT          upload COUNT small Maven files (makes the backup take longer)
  seed.py verify-loop BASE USER PASS LOG BEFORE  check the loop's uploads on a restored server: the first BEFORE
                                               (completed before the backup began) must be present, the rest
                                               present or absent, but present ones form a prefix
"""
import base64, hashlib, io, json, os, struct, sys, tarfile, time, urllib.error, urllib.request, uuid, zipfile


class Client:
    def __init__(self, base, user, password):
        self.base = base.rstrip("/")
        self.auth = "Basic " + base64.b64encode(f"{user}:{password}".encode()).decode()

    def req(self, method, path, body=None, headers=None):
        r = urllib.request.Request(self.base + path, data=body, method=method)
        r.add_header("Authorization", self.auth)
        for k, v in (headers or {}).items():
            r.add_header(k, v)
        try:
            with urllib.request.urlopen(r, timeout=60) as resp:
                return resp.status, resp.read()
        except urllib.error.HTTPError as e:
            return e.code, e.read()

    def must(self, want, method, path, body=None, headers=None):
        st, data = self.req(method, path, body, headers)
        if st != want:
            raise SystemExit(f"FAIL: {method} {path}: want {want}, got {st}: {data[:300]!r}")
        return data


def payload(n=64 * 1024):
    return os.urandom(n)


def tgz(files):
    buf = io.BytesIO()
    with tarfile.open(fileobj=buf, mode="w:gz") as t:
        for name, data in files.items():
            ti = tarfile.TarInfo(name)
            ti.size = len(data)
            t.addfile(ti, io.BytesIO(data))
    return buf.getvalue()


def multipart(fields, filename, content):
    b = uuid.uuid4().hex
    out = io.BytesIO()
    for k, v in fields.items():
        out.write(f'--{b}\r\nContent-Disposition: form-data; name="{k}"\r\n\r\n{v}\r\n'.encode())
    out.write(f'--{b}\r\nContent-Disposition: form-data; name="content"; filename="{filename}"\r\n'
              f'Content-Type: application/octet-stream\r\n\r\n'.encode())
    out.write(content)
    out.write(f"\r\n--{b}--\r\n".encode())
    return out.getvalue(), {"Content-Type": f"multipart/form-data; boundary={b}"}


# Each uploader returns (download path, bytes, component coordinates).

def up_maven(c, tag):
    g, a, v = "smoke.backup", "m-" + tag, "1.0.0"
    data = payload()
    path = f"/repository/maven-hosted/{g.replace('.', '/')}/{a}/{v}/{a}-{v}.jar"
    c.must(201, "PUT", path, data)
    return path, data, {"group": g, "name": a, "version": v}


def up_npm(c, tag):
    name, v = "n-" + tag, "1.0.0"
    tar = tgz({"package/package.json": json.dumps({"name": name, "version": v}).encode(), "package/blob.bin": payload()})
    fname = f"{name}-{v}.tgz"
    doc = {
        "_id": name, "name": name, "dist-tags": {"latest": v},
        "versions": {v: {"name": name, "version": v, "dist": {
            "shasum": hashlib.sha1(tar).hexdigest(),
            "integrity": "sha512-" + base64.b64encode(hashlib.sha512(tar).digest()).decode(),
            "tarball": f"http://localhost/{name}/-/{fname}"}}},
        "_attachments": {fname: {"content_type": "application/octet-stream", "length": len(tar),
                                 "data": base64.b64encode(tar).decode()}},
    }
    c.must(201, "PUT", f"/repository/npm-hosted/{name}", json.dumps(doc).encode(), {"Content-Type": "application/json"})
    return f"/repository/npm-hosted/{name}/-/{fname}", tar, {"name": name, "version": v}


def up_pypi(c, tag):
    name, v = "p" + tag.replace("-", ""), "1.0"
    fname = f"{name}-{v}.tar.gz"
    tar = tgz({f"{name}-{v}/PKG-INFO": f"Metadata-Version: 2.1\nName: {name}\nVersion: {v}\n".encode(),
               f"{name}-{v}/blob.bin": payload()})
    body, hdr = multipart({":action": "file_upload", "name": name, "version": v, "sha256_digest": hashlib.sha256(tar).hexdigest()},
                          fname, tar)
    c.must(200, "POST", "/repository/pypi-hosted/", body, hdr)
    return f"/repository/pypi-hosted/packages/{name}/{fname}", tar, {"name": name, "version": v}


def up_go(c, tag):
    mod, v = f"example.com/smoke/g{tag.replace('-', '')}", "v1.0.0"
    buf = io.BytesIO()
    with zipfile.ZipFile(buf, "w") as z:
        z.writestr(f"{mod}@{v}/go.mod", f"module {mod}\n\ngo 1.21\n")
        z.writestr(f"{mod}@{v}/blob.bin", payload())
    data = buf.getvalue()
    base = f"/repository/go-hosted/{mod}/@v/{v}"
    c.must(201, "PUT", base + ".zip", data)
    return base + ".zip", data, {"name": mod, "version": v}


def up_cargo(c, tag):
    name, v = "c" + tag.replace("-", "_"), "1.0.0"
    crate = tgz({f"{name}-{v}/Cargo.toml": f'[package]\nname = "{name}"\nversion = "{v}"\n'.encode(),
                 f"{name}-{v}/blob.bin": payload()})
    meta = json.dumps({"name": name, "vers": v, "deps": [], "features": {}, "authors": [], "description": "smoke",
                       "license": "MIT"}).encode()
    body = struct.pack("<I", len(meta)) + meta + struct.pack("<I", len(crate)) + crate
    c.must(200, "PUT", "/repository/cargo-hosted/api/v1/crates/new", body)
    return f"/repository/cargo-hosted/crates/{name}/{v}/download", crate, {"name": name, "version": v}


UPLOADERS = [("maven-hosted", up_maven), ("npm-hosted", up_npm), ("pypi-hosted", up_pypi), ("go-hosted", up_go),
             ("cargo-hosted", up_cargo)]


def sha(b):
    return hashlib.sha256(b).hexdigest()


def cmd_seed(c, tag, manifest):
    lines = []
    for _, up in UPLOADERS:
        path, data, _comp = up(c, tag)
        lines.append(f"{path} {sha(data)}")
    with open(manifest, "w") as f:
        f.write("\n".join(lines) + "\n")
    print(f"seeded {len(lines)} artifacts")


def cmd_check(c, manifest):
    n = 0
    for line in open(manifest):
        path, want = line.split()
        got = sha(c.must(200, "GET", path))
        if got != want:
            raise SystemExit(f"FAIL: sha256 differs for {path}: want {want}, got {got}")
        n += 1
        print(f"ok {path}")
    print(f"verified {n} artifacts")


def cmd_loop(c):
    i = 0
    while True:
        i += 1
        path = f"/repository/maven-hosted/smoke/loop/l/1.0.{i}/l-1.0.{i}.jar"
        data = payload(32 * 1024)
        try:
            st, _ = c.req("PUT", path, data)
        except Exception as e:  # a dropped connection is a failure too
            print(f"BAD {i} ERR {type(e).__name__}: {e}", flush=True)
        else:
            if 200 <= st < 300:
                print(f"UP {i} {path} {sha(data)}", flush=True)
            else:
                print(f"BAD {i} HTTP {st}", flush=True)
        time.sleep(0.01)


def cmd_bulk(c, count):
    for i in range(int(count)):
        c.must(201, "PUT", f"/repository/maven-hosted/smoke/bulk/b/1.0.{i}/b-1.0.{i}.jar", payload(64 * 1024))
    print(f"bulk uploaded {count} files")


def cmd_verify_loop(c, log, before):
    before = int(before)
    ups = [l.split() for l in open(log) if l.startswith("UP ")]
    if len(ups) < before:
        raise SystemExit(f"FAIL: the log has {len(ups)} uploads, fewer than the {before} completed before the backup")
    present = absent = 0
    for n, (_, i, path, want) in enumerate(ups, 1):
        st, data = c.req("GET", path)
        if st == 200:
            if sha(data) != want:
                raise SystemExit(f"FAIL: loop upload {i} was restored with different content")
            if absent:
                raise SystemExit(f"FAIL: loop upload {i} is present after an earlier one was missing: not a consistent snapshot")
            present += 1
        elif st == 404:
            if n <= before:
                raise SystemExit(f"FAIL: loop upload {i} completed before the backup began but is missing after the restore")
            absent += 1
        else:
            raise SystemExit(f"FAIL: loop upload {i}: HTTP {st} after the restore")
    print(f"loop uploads after the restore: {present} present, {absent} absent ({before} completed before the backup)")



def main():
    cmd, base, user, pw, *rest = sys.argv[1:]
    c = Client(base, user, pw)
    {"seed": cmd_seed, "check": cmd_check, "loop": cmd_loop, "bulk": cmd_bulk,
     "verify-loop": cmd_verify_loop}[cmd](c, *rest)


main()
