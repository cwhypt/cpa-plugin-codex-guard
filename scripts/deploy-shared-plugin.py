#!/usr/bin/env python3
"""Install a verified Guard artifact and atomically hot-load its routing policy."""
import argparse
import copy
from datetime import datetime, timezone
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import tempfile
import time
import yaml


def digest(path):
    h = hashlib.sha256()
    with path.open("rb") as source:
        for chunk in iter(lambda: source.read(1024*1024), b""):
            h.update(chunk)
    return h.hexdigest()


def atomic_write(path, data, mode):
    descriptor, temporary = tempfile.mkstemp(prefix=path.name+".", suffix=".tmp", dir=path.parent)
    try:
        os.fchmod(descriptor, mode)
        with os.fdopen(descriptor, "wb") as output:
            output.write(data)
            output.flush()
            os.fsync(output.fileno())
        os.replace(temporary, path)
    finally:
        if os.path.exists(temporary):
            os.unlink(temporary)


def write_watched_config(path, expected, data):
    # CPA watches the file inode, not its parent directory. Replacing the inode
    # loses that watch; write the validated bytes in place as the original
    # one-shot release script does. The caller keeps a separate rollback copy.
    with path.open("r+b") as output:
        assert output.read()==expected, "config changed concurrently"
        output.seek(0)
        output.write(data)
        output.truncate()
        output.flush()
        os.fsync(output.fileno())


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--root", type=Path, required=True)
    parser.add_argument("--artifact", type=Path, required=True)
    parser.add_argument("--policy", type=Path, required=True)
    parser.add_argument("--version", required=True)
    parser.add_argument("--source-commit", required=True)
    parser.add_argument("--dry-run", action="store_true")
    args = parser.parse_args()
    if not re.fullmatch(r"\d+\.\d+\.\d+", args.version):
        parser.error("numeric version required")
    root = args.root.resolve()
    config_path = root/"config.yaml"
    destination = root/"plugins/linux/arm64"/f"cpa-codex-guard-v{args.version}.so"
    assert destination.parent.is_dir() and (root/".rollback").is_dir()
    assert not destination.exists() and not destination.with_suffix(".h").exists(), "release artifact already exists"
    assert args.artifact.with_suffix(".h").is_file()
    description = subprocess.check_output(["file", str(args.artifact)], text=True)
    assert all(part in description for part in ("ELF 64-bit", "aarch64", "shared object")), description
    symbols = subprocess.check_output(["nm", "-D", str(args.artifact)], text=True)
    assert re.search(r"(?m)\bT cliproxy_plugin_init$", symbols)
    before_bytes = config_path.read_bytes()
    before = before_bytes.decode()
    config = yaml.safe_load(before)
    guard = config["plugins"]["configs"]["cpa-codex-guard"]
    previous_version = guard["store"]["version"]
    assert tuple(map(int,args.version.split("."))) > tuple(map(int,previous_version.split(".")))
    assert guard.get("enabled") and guard.get("priority")==5
    assert config["plugins"]["configs"]["cpa-key-policy-x"].get("routing_mode")=="host"
    policy = yaml.safe_load(args.policy.read_text())
    assert set(policy)=={"session_routing"} and policy["session_routing"]["enabled"]
    assert "session_routing" not in guard, "existing routing config requires explicit review"
    block_re = re.compile(r"(?m)^    cpa-codex-guard:[ \t]*\n(?:(?!^    \S)[^\n]*(?:\n|$))+")
    blocks = list(block_re.finditer(before))
    assert len(blocks)==1, "ambiguous Guard configuration block"
    block = blocks[0]
    updated, count = re.subn(r"(?m)^(        version:[ \t]*)\d+\.\d+\.\d+([ \t]*)$",
                            lambda match: match[1]+args.version+match[2], block.group())
    assert count==1
    rendered = yaml.safe_dump(policy, allow_unicode=True, sort_keys=False, width=120)
    updated = updated.rstrip()+"\n"+"".join("      "+line+"\n" for line in rendered.splitlines())+"\n"
    after = before[:block.start()]+updated+before[block.end():]
    expected = copy.deepcopy(config)
    expected["plugins"]["configs"]["cpa-codex-guard"]["store"]["version"] = args.version
    expected["plugins"]["configs"]["cpa-codex-guard"]["session_routing"] = policy["session_routing"]
    assert yaml.safe_load(after)==expected, "configuration edit changed unrelated values"
    old_artifact = destination.parent/f"cpa-codex-guard-v{previous_version}.so"
    assert old_artifact.is_file()
    if args.dry_run:
        print(json.dumps(dict(version=args.version,previous_version=previous_version,
                              source_commit=args.source_commit,artifact_sha256=digest(args.artifact),
                              policy_sha256=digest(args.policy),status="validated-without-writes"),indent=2))
        return
    pid = subprocess.check_output(["systemctl", "--user", "show", "cliproxyapi.service", "-p", "MainPID", "--value"], text=True).strip()
    assert pid.isdigit() and pid!="0"
    backup = root/".rollback"/("codex-guard-"+args.version+"-"+datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ"))
    backup.mkdir(mode=0o700)
    atomic_write(backup/"config.before.yaml",before_bytes,0o600)
    atomic_write(backup/"guard.before.yaml",yaml.safe_dump({"guard":guard},allow_unicode=True,sort_keys=False).encode(),0o600)
    shutil.copy2(old_artifact,backup/old_artifact.name)
    for state_path in (guard.get("state_file"),guard.get("probe_state_file"),policy["session_routing"]["state_file"]):
        if state_path:
            path = root/state_path
            if path.is_file():
                shutil.copy2(path,backup/path.name)
                os.chmod(backup/path.name,0o600)
    artifact_hash = digest(args.artifact)
    manifest = dict(version=args.version, previous_version=previous_version, source_commit=args.source_commit,
                    pid_before=int(pid), backup=str(backup), artifact=str(destination), sha256=artifact_hash,
                    previous_sha256=digest(old_artifact), policy_sha256=digest(args.policy),
                    config_before_sha256=hashlib.sha256(before_bytes).hexdigest(),
                    config_after_sha256=hashlib.sha256(after.encode()).hexdigest(), status="prepared")
    atomic_write(backup/"manifest.json",json.dumps(manifest,indent=2).encode(),0o600)
    atomic_write(destination.with_suffix(".h"),args.artifact.with_suffix(".h").read_bytes(),0o644)
    atomic_write(destination,args.artifact.read_bytes(),0o644)
    assert digest(destination)==artifact_hash
    assert config_path.read_bytes()==before_bytes, "config changed concurrently; installed artifact retained, config not changed"
    log = Path.home()/".cli-proxy-api/logs/main.log"
    stat = log.stat() if log.exists() else None
    write_watched_config(config_path,before_bytes,after.encode())
    manifest["status"]="config-written"
    atomic_write(backup/"manifest.json",json.dumps(manifest,indent=2).encode(),0o600)
    deadline=time.monotonic()+30
    while time.monotonic()<deadline:
        if log.exists():
            current=log.stat()
            with log.open(errors="replace") as source:
                if stat and current.st_ino==stat.st_ino and current.st_size>=stat.st_size:
                    source.seek(stat.st_size)
                if any("plugin hot reloaded" in line and "plugin_id=cpa-codex-guard" in line and
                       f"active_version={args.version}" in line for line in source.read().splitlines()):
                    break
        time.sleep(1)
    else:
        raise RuntimeError(f"hot reload not confirmed; inspect {backup}/manifest.json before retrying")
    pid_after=subprocess.check_output(["systemctl","--user","show","cliproxyapi.service","-p","MainPID","--value"],text=True).strip()
    assert pid_after==pid, "CPA PID changed"
    assert f"cliproxy-plugin-cpa-codex-guard-{artifact_hash[:32]}" in Path(f"/proc/{pid}/maps").read_text()
    manifest.update(status="hot-reload-confirmed",pid_after=int(pid_after),verified_at=datetime.now(timezone.utc).isoformat())
    atomic_write(backup/"manifest.json",json.dumps(manifest,indent=2).encode(),0o600)
    print(json.dumps(manifest,indent=2))


if __name__=="__main__":
    main()
