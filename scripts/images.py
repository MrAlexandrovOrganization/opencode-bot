"""Build Compose application images once and activate their published digests."""

import base64
import fcntl
import json
import os
from pathlib import Path
import re
import shlex
import subprocess
import sys
import tempfile
import time

ROOT = Path(__file__).resolve().parents[1]
STATE = ROOT / ".images"
PLATFORM = "linux/amd64"


def run(*args, capture=False):
    return subprocess.run(
        args,
        cwd=ROOT,
        check=True,
        text=True,
        stdout=subprocess.PIPE if capture else None,
    ).stdout


def compose(runtime=False):
    files = os.environ.get("COMPOSE_FILE", os.environ["IMAGE_COMPOSE_FILE"]).split(
        os.pathsep
    )
    if runtime and "COMPOSE_FILE" not in os.environ:
        override = ROOT / "docker-compose.override.yml"
        if override.exists():
            files.append(override.name)
    command = ["docker", "compose", "--project-directory", str(ROOT)]
    if os.environ.get("IMAGE_PROJECT_NAME"):
        command += ["-p", os.environ["IMAGE_PROJECT_NAME"]]
    for file in files:
        command += ["-f", file]
    return command


def model():
    # Read only build definitions. No .env, runtime secrets or env_file contents.
    return json.loads(
        run(
            *compose(),
            "--profile",
            "*",
            "--env-file",
            "/dev/null",
            "config",
            "--no-interpolate",
            "--no-env-resolution",
            "--no-path-resolution",
            "--format",
            "json",
            capture=True,
        )
    )


def expand(value):
    def replace(match):
        key, operator, default = match.groups()
        value = os.environ.get(key, "")
        if not value and operator == ":?":
            raise ValueError(f"Missing build variable: {key}; run through Make")
        return value or (default if operator == ":-" else "")

    return re.sub(r"\$\{([A-Z_][A-Z_0-9]*)(:\?|:-)?([^}]*)\}", replace, str(value))


def specs():
    result = {
        name: service["build"]
        for name, service in model()["services"].items()
        if service.get("build")
    }
    if not result:
        raise ValueError("No application images")
    return result


def write(path, value):
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(value, indent=2) + "\n")


def revision():
    return run("git", "rev-parse", "HEAD", capture=True).strip()


def build():
    repo = os.environ.get("IMAGE_REPOSITORY", f"local/{ROOT.name}").lower()
    sha = revision()
    run_id = os.environ.get("GITHUB_RUN_ID", "local")
    attempt = os.environ.get("GITHUB_RUN_ATTEMPT", "1")
    tag = f"{sha}-{run_id}-{attempt}"
    images, built = {}, {}
    for name, spec in specs().items():
        key = json.dumps(spec, sort_keys=True)
        if key not in built:
            image = f"{repo}/{name}:{tag}"
            command = [
                "docker",
                "build",
                "--platform",
                PLATFORM,
                "-t",
                image,
                "--label",
                f"org.opencontainers.image.revision={sha}",
            ]
            if os.environ.get("GITHUB_REPOSITORY"):
                command += [
                    "--label",
                    "org.opencontainers.image.source=https://github.com/"
                    + os.environ["GITHUB_REPOSITORY"],
                ]
            context = ROOT / expand(spec["context"])
            command += ["-f", str(context / spec.get("dockerfile", "Dockerfile"))]
            if spec.get("target"):
                command += ["--target", spec["target"]]
            for arg, value in spec.get("args", {}).items():
                command += [
                    "--build-arg",
                    f"{arg}={expand(value) if value is not None else os.environ[arg]}",
                ]
            run(*command, str(context))
            built[key] = image
        images[name] = built[key]
    for alias in os.environ.get("IMAGE_ALIASES", "").split():
        target, source = alias.split("=")
        images[target] = images[source]
    write(
        STATE / "build.json", {"revision": sha, "platform": PLATFORM, "images": images}
    )


def publish():
    manifest = json.loads((STATE / "build.json").read_text())
    if manifest["revision"] != revision():
        raise ValueError("Build revision differs from checkout")
    resolved = {}
    for tag in set(manifest["images"].values()):
        if not tag.startswith("ghcr.io/"):
            raise ValueError("Publication requires IMAGE_REPOSITORY=ghcr.io/owner/repo")
        run("docker", "push", tag)
        digests = json.loads(
            run(
                "docker",
                "image",
                "inspect",
                "--format",
                "{{json .RepoDigests}}",
                tag,
                capture=True,
            )
        )
        repository = tag.rsplit(":", 1)[0]
        resolved[tag] = next(
            ref for ref in digests if ref.startswith(repository + "@sha256:")
        )
    manifest["images"] = {
        name: resolved[tag] for name, tag in manifest["images"].items()
    }
    write(STATE / "release.json", manifest)
    if os.environ.get("GITHUB_OUTPUT"):
        encoded = base64.b64encode(json.dumps(manifest).encode()).decode()
        with open(os.environ["GITHUB_OUTPUT"], "a") as output:
            output.write(f"manifest={encoded}\n")


def validate(manifest, *, require_revision=True):
    if (require_revision and manifest["revision"] != revision()) or manifest[
        "platform"
    ] != PLATFORM:
        raise ValueError("Release revision/platform differs from checkout")
    expected = set(specs())
    expected.update(
        alias.split("=")[0] for alias in os.environ.get("IMAGE_ALIASES", "").split()
    )
    if set(manifest["images"]) != expected:
        raise ValueError("Release service list differs from Compose")
    for name, ref in manifest["images"].items():
        if not re.fullmatch(r"[a-z0-9][a-z0-9_-]*", name):
            raise ValueError("Invalid service name")
        if not re.fullmatch(r"ghcr\.io/[a-z0-9_.\-/]+@sha256:[0-9a-f]{64}", ref):
            raise ValueError("Release must contain GHCR digests")
    for alias in os.environ.get("IMAGE_ALIASES", "").split():
        target, source = alias.split("=")
        if manifest["images"][target] != manifest["images"][source]:
            raise ValueError("Migration alias must use the application digest")


def locked_deploy():
    if os.environ.get("IMAGE_DEPLOY_LOCKED") == "1":
        return deploy()
    lockfile = Path(os.path.expandvars(os.environ["IMAGE_DEPLOY_LOCK"]))
    with lockfile.open("a") as lock:
        deadline = time.monotonic() + 1800
        while True:
            try:
                fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
                break
            except BlockingIOError:
                if time.monotonic() >= deadline:
                    raise ValueError("Timed out waiting for deployment lock") from None
                time.sleep(1)
        # The router's existing activation shares this same host lock.
        os.environ["DEPLOY_LOCKED"] = "1"
        deploy()


def deploy():
    if os.environ.get("IMAGE_MANIFEST_B64"):
        manifest = json.loads(
            base64.b64decode(os.environ["IMAGE_MANIFEST_B64"], validate=True)
        )
    else:
        manifest = json.loads(
            Path(os.environ.get("IMAGE_MANIFEST", STATE / "release.json")).read_text()
        )
    validate(manifest)
    STATE.mkdir(exist_ok=True)
    write(STATE / "requested.json", manifest)
    # Fetch every application image before touching running services or databases.
    for ref in set(manifest["images"].values()):
        run("docker", "pull", "--platform", PLATFORM, ref)
    override = {
        "services": {
            name: {"image": ref, "platform": PLATFORM}
            for name, ref in manifest["images"].items()
        }
    }
    with tempfile.TemporaryDirectory(dir=STATE) as directory:
        candidate = Path(directory) / "compose.json"
        write(candidate, override)
        command = compose(runtime=True)
        if (ROOT / "compose.images.yaml").exists():
            command += ["-f", "compose.images.yaml"]
        command += ["-f", str(candidate)]
        # Pull third-party dependencies too; failure leaves the old services intact.
        run(*command, "pull")
        run("make", "image-activate", "IMAGE_COMPOSE=" + shlex.join(command))
    current = STATE / "current.json"
    if current.exists():
        (STATE / "previous.json").write_bytes(current.read_bytes())
    write(current, manifest)
    write(STATE / "compose.json", override)
    print(f"Activated images for {manifest['revision']}")


def operate():
    """Use the active release for diagnostics and explicit one-shot commands."""
    manifest = json.loads((STATE / "current.json").read_text())
    validate(manifest, require_revision=False)
    args = shlex.split(os.environ.get("IMAGE_ARGS", "ps"))
    if not args or args[0] not in {"ps", "logs", "exec", "run", "stop", "restart"}:
        raise ValueError(
            "Use image-deploy for activation; image-compose supports ps/logs/exec/run/stop/restart"
        )
    if args[0] == "run":
        args[1:1] = ["--no-build", "--pull", "never"]
    command = compose(runtime=True)
    if (ROOT / "compose.images.yaml").exists():
        command += ["-f", "compose.images.yaml"]
    run(*command, "-f", str(STATE / "compose.json"), *args)


if __name__ == "__main__":
    try:
        {
            "build": build,
            "publish": publish,
            "deploy": locked_deploy,
            "compose": operate,
        }[sys.argv[1]]()
    except (
        ValueError,
        KeyError,
        OSError,
        subprocess.CalledProcessError,
        StopIteration,
    ):
        # Avoid printing environment variables, runtime Compose models or credentials.
        print(
            "Image operation failed; inspect the preceding build/pull/check result.",
            file=sys.stderr,
        )
        sys.exit(1)
