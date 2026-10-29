#!/usr/bin/env python3
"""Deploy complete public release bundles for the current main revision."""
import contextlib
import fcntl
import hashlib
import json
import os
from pathlib import Path
import re
import sqlite3
import subprocess
import sys
import time
import urllib.error
import urllib.request

REPOSITORY = "adam-web3-developer9/aistudio-api"
ROOT = Path("/opt/aistudio-api")
MAINTENANCE = Path("/run/aistudio-api-maintenance")
CONTAINER = "new-api"
DATA = Path("/www/wwwroot/new-api/data")
COMPOSE = DATA.parent / "docker-compose.yml"
SHA_PATTERN = re.compile(r"[0-9a-f]{40}")


def log(message):
    print(time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()), message, flush=True)


def run(args, timeout=60):
    result = subprocess.run(args, capture_output=True, text=True, timeout=timeout)
    if result.returncode:
        # Commands never include credentials; avoid logging container env/logs.
        raise RuntimeError(f"{args[0]} {args[1]} failed ({result.returncode}): {result.stderr[-800:]}")
    return result.stdout.strip()


def write_private(path, value):
    temp = path.with_name(path.name + ".tmp")
    with temp.open("w", encoding="utf-8") as out:
        os.chmod(temp, 0o600)
        out.write(value)
    temp.replace(path)


def current_main():
    refs = run(["git", "ls-remote", f"https://github.com/{REPOSITORY}.git", "refs/heads/main"])
    sha = refs.split()[0] if refs else ""
    if not SHA_PATTERN.fullmatch(sha):
        raise RuntimeError("main does not yet identify a full commit")
    return sha


def download(url, target):
    temp = target.with_name(target.name + ".partial")
    try:
        with urllib.request.urlopen(url, timeout=45) as response, temp.open("wb") as out:
            os.chmod(temp, 0o600)
            while chunk := response.read(1024 * 1024):
                out.write(chunk)
        temp.replace(target)
    finally:
        temp.unlink(missing_ok=True)


def digest(path):
    with path.open("rb") as source:
        return hashlib.file_digest(source, "sha256").hexdigest()


def verify_bundle(folder, sha):
    if not SHA_PATTERN.fullmatch(sha):
        raise RuntimeError("invalid deployment revision")
    checksums = {}
    for line in (folder / "SHA256SUMS").read_text().splitlines():
        parts = line.split()
        if len(parts) != 2 or not re.fullmatch(r"[0-9a-f]{64}", parts[0]):
            raise RuntimeError("invalid release checksums")
        name = parts[1].removeprefix("*")
        if name not in {"release.json", "aistudio-api-image.tar.gz"} or name in checksums:
            raise RuntimeError("unexpected release checksum entry")
        checksums[name] = parts[0]
    if set(checksums) != {"release.json", "aistudio-api-image.tar.gz"}:
        raise RuntimeError("release checksums are incomplete")
    for name, expected in checksums.items():
        if digest(folder / name) != expected:
            raise RuntimeError(f"release checksum mismatch: {name}")
    manifest = json.loads((folder / "release.json").read_text())
    if manifest.get("commit") != sha or manifest.get("image_ref") != "aistudio-api:sha-" + sha:
        raise RuntimeError("release does not match the selected main revision")
    if manifest.get("artifact_checksum") != checksums["aistudio-api-image.tar.gz"]:
        raise RuntimeError("image checksum differs from release metadata")
    if manifest.get("version") != "main-" + sha[:12]:
        raise RuntimeError("unexpected application version")
    if not re.fullmatch(r"sha256:[0-9a-f]{64}", manifest.get("image_id", "")):
        raise RuntimeError("invalid source image identity")
    return manifest


def healthy(expected_version=None, attempts=1):
    for _ in range(attempts):
        try:
            with urllib.request.urlopen("http://127.0.0.1:8080/api/status", timeout=5) as response:
                data = json.load(response)
            if data.get("success") and (expected_version is None or data.get("data", {}).get("version") == expected_version):
                return True
        except (OSError, ValueError):
            pass
        time.sleep(2)
    return False


def active_images():
    database = DATA / "one-api.db"
    if not database.is_file():
        raise RuntimeError("existing production database is missing")
    with sqlite3.connect(database.as_uri() + "?mode=ro", uri=True, timeout=10) as db:
        return db.execute("SELECT count(*) FROM tasks WHERE platform='image' AND status NOT IN ('SUCCESS','FAILURE')").fetchone()[0]


def active_http_connections(pid):
    count = 0
    for name in ["tcp", "tcp6"]:
        with Path(f"/proc/{pid}/net/{name}").open() as sockets:
            next(sockets)
            for line in sockets:
                fields = line.split()
                if fields[1].rsplit(":", 1)[1] == "0BB8" and fields[3] == "01":
                    count += 1
    return count


def deploy(folder, manifest, sha):
    if current_main() != sha:
        log("main advanced; skip superseded deployment")
        return
    old = json.loads(run(["docker", "inspect", CONTAINER]))[0]
    if not old["State"]["Running"]:
        raise RuntimeError("current container is not running; manual recovery required")
    env_file = ROOT / "runtime.env"
    if not env_file.is_file():
        raise RuntimeError("private runtime environment has not been installed")
    rollback = CONTAINER + "-previous-" + old["Id"][:12]
    stamp = time.strftime("%Y%m%dT%H%M%SZ", time.gmtime())
    before = ROOT / "history" / (stamp + "-" + sha[:12])
    before.mkdir(parents=True, mode=0o700)
    write_private(before / "container.json", json.dumps(old, indent=2))
    write_private(before / "docker-compose.yml", COMPOSE.read_text())
    # The operator explicitly opted out of database backups. Preserve the same
    # database mount and never silently replace it with a fresh database.
    write_private(before / "policy.json", json.dumps({"database_backup": False, "data_directory": str(DATA), "previous_image": old["Image"]}))
    marker = MAINTENANCE
    marker.touch(mode=0o600)
    old_stopped = False
    old_renamed = False
    new_started = False
    policy = old.get("HostConfig", {}).get("RestartPolicy", {"Name": "always"})
    restart_policy = policy.get("Name") or "no"
    if restart_policy == "on-failure" and policy.get("MaximumRetryCount"):
        restart_policy += ":" + str(policy["MaximumRetryCount"])
    try:
        # Nginx rejects new requests while existing in-process image workers
        # finish. Their original implementation cannot replay after restart.
        deadline = time.monotonic() + 300
        while active_images() or active_http_connections(old["State"]["Pid"]):
            if time.monotonic() >= deadline:
                raise RuntimeError("HTTP connections or async image tasks did not drain; deployment deferred")
            time.sleep(3)
        old_env = dict(v.split("=", 1) for v in old["Config"].get("Env", []) if "=" in v)
        interval = int(old_env.get("BATCH_UPDATE_INTERVAL", "5"))
        time.sleep(max(5, interval) + 2)
        if current_main() != sha:
            log("main advanced while draining; keep running version")
            return
        run(["docker", "stop", "--time", "150", CONTAINER], timeout=180)
        old_stopped = True
        # Retained rollback containers must never auto-start against the same
        # port and SQLite database after a Docker daemon or machine restart.
        run(["docker", "update", "--restart=no", CONTAINER])
        run(["docker", "rename", CONTAINER, rollback])
        old_renamed = True
        new_started = True
        run(["docker", "run", "-d", "--name", CONTAINER, "--restart", "always",
             "--publish", "127.0.0.1:8080:3000", "--volume", str(DATA) + ":/data",
             "--env-file", str(env_file), "--log-opt", "max-size=20m", "--log-opt", "max-file=3",
             "--label", "org.opencontainers.image.revision=" + sha,
             manifest["local_image_id"]], timeout=90)
        if not healthy(manifest["version"], attempts=150):
            raise RuntimeError("new application did not pass its versioned health check")
        # Keep an operator-readable Compose file matching the actual container;
        # the old file advertised a different image and host port.
        compose = {"services": {"new-api": {"image": manifest["image_ref"], "container_name": CONTAINER,
                   "restart": "always", "stop_grace_period": "150s", "ports": ["127.0.0.1:8080:3000"],
                   "volumes": [str(DATA) + ":/data"], "env_file": [str(env_file)],
                   "logging": {"driver": "json-file", "options": {"max-size": "20m", "max-file": "3"}}}}}
        write_private(COMPOSE, json.dumps(compose, indent=2) + "\n")
        state = {"commit": sha, "version": manifest["version"], "image_ref": manifest["image_ref"],
                 "image_id": json.loads(run(["docker", "inspect", CONTAINER]))[0]["Image"],
                 "previous_container": rollback, "configuration_backup": str(before),
                 "source_image_id": manifest["image_id"],
                 "database_backup": False, "deployed_at": stamp}
        write_private(ROOT / "deployed.json", json.dumps(state, indent=2) + "\n")
        log("deployment healthy: " + sha)
    except Exception:
        # Revert the runtime only. Database migration rollback is intentionally
        # unavailable because the operator chose to skip database copies.
        if new_started:
            with contextlib.suppress(RuntimeError):
                run(["docker", "rm", "--force", CONTAINER], timeout=180)
        if old_renamed:
            run(["docker", "rename", rollback, CONTAINER])
        if old_stopped:
            run(["docker", "update", "--restart=" + restart_policy, CONTAINER])
            run(["docker", "start", CONTAINER])
            write_private(COMPOSE, (before / "docker-compose.yml").read_text())
            log("previous runtime restarted; health=" + str(healthy(attempts=15)))
        raise
    finally:
        if healthy():
            marker.unlink(missing_ok=True)
        else:
            log("runtime health failed; maintenance retained for recovery")


def poll():
    sha = current_main()
    state_path = ROOT / "deployed.json"
    if state_path.exists():
        state = json.loads(state_path.read_text())
        if state.get("commit") == sha and healthy(state.get("version")):
            running = json.loads(run(["docker", "inspect", CONTAINER]))[0]
            labels = running["Config"].get("Labels", {}) or {}
            if running["State"]["Running"] and running["Image"] == state.get("image_id") and labels.get("org.opencontainers.image.revision") == sha:
                return
    folder = ROOT / "releases" / sha
    folder.mkdir(parents=True, exist_ok=True, mode=0o700)
    base = f"https://github.com/{REPOSITORY}/releases/download/deploy-{sha}/"
    try:
        for name in ["SHA256SUMS", "release.json"]:
            download(base + name, folder / name)
    except urllib.error.HTTPError as error:
        if error.code == 404:
            log("waiting for complete main build: " + sha)
            return
        raise
    image = folder / "aistudio-api-image.tar.gz"
    if not image.exists():
        free = os.statvfs(ROOT).f_bavail * os.statvfs(ROOT).f_frsize
        if free < 2 * 1024**3:
            raise RuntimeError("less than 2 GiB free for image download/load; current runtime retained")
        download(base + image.name, image)
    try:
        manifest = verify_bundle(folder, sha)
    except Exception:
        image.unlink(missing_ok=True)
        raise
    if current_main() != sha:
        log("main advanced after download; keep running version")
        return
    run(["docker", "load", "--input", str(image)], timeout=300)
    loaded = json.loads(run(["docker", "image", "inspect", manifest["image_ref"]]))[0]
    labels = loaded["Config"].get("Labels", {}) or {}
    if labels.get("org.opencontainers.image.revision") != sha or labels.get("org.opencontainers.image.source") != "https://github.com/" + REPOSITORY:
        raise RuntimeError("loaded image provenance does not match the selected repository revision")
    # Docker's classic and containerd stores can expose different .Id formats
    # for the same exported image. Archive checksum plus revision/source labels
    # bind the bundle; run the verified local immutable ID, never a mutable tag.
    if not re.fullmatch(r"sha256:[0-9a-f]{64}", loaded.get("Id", "")):
        raise RuntimeError("loaded image has no immutable local identity")
    manifest["local_image_id"] = loaded["Id"]
    deploy(folder, manifest, sha)


def main():
    if sys.argv[1:] != ["poll"]:
        raise SystemExit("usage: deploy.py poll")
    ROOT.mkdir(parents=True, exist_ok=True, mode=0o700)
    os.umask(0o077)
    with (ROOT / "deploy.lock").open("a") as lock:
        try:
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            return
        poll()


if __name__ == "__main__":
    try:
        main()
    except Exception as error:
        log(str(error))
        raise SystemExit(1)
