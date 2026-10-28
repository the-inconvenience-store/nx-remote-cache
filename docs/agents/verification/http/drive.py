#!/usr/bin/env python3
"""Drive the shipping server against disposable Garage; retain redacted evidence."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import secrets
import shutil
import signal
import socket
import ssl
import subprocess
import tempfile
import time
import urllib.error
import urllib.request

ROOT = Path(__file__).resolve().parents[4]
parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument("--evidence", type=Path, required=True)
parser.add_argument("--tls", action="store_true", help="Use repository test certificates")
parser.add_argument("--fail-closed", action="store_true")
args = parser.parse_args()
evidence = args.evidence.resolve()
evidence.mkdir(parents=True, exist_ok=False)
scratch = Path(tempfile.mkdtemp(prefix="nx-cache-verify-"))
container = "nx-cache-verify-" + secrets.token_hex(6)
process = None
log = None
records = []
verdict = "INCONCLUSIVE"


def record(message):
    records.append(message)
    print(message, flush=True)


def command(*argv, **kwargs):
    return subprocess.check_output(argv, cwd=ROOT, stderr=subprocess.STDOUT, **kwargs).decode().strip()


def expect(method, path, status, token=None, body=None, expected=None):
    headers = {} if token is None else {"Authorization": "Bearer " + token}
    request = urllib.request.Request(base + path, data=body, headers=headers, method=method)
    try:
        response = urllib.request.urlopen(request, timeout=15, context=context)
    except urllib.error.HTTPError as error:
        response = error
    with response:
        payload = response.read()
        actual = response.status
    digest = hashlib.sha256(payload).hexdigest()
    record(f"{method} {path} credential={'none' if token is None else 'redacted'} -> {actual}; bytes={len(payload)} sha256={digest}")
    if actual != status or (expected is not None and payload != expected):
        raise AssertionError(f"{method} {path}: expected status {status} and specified body, got {actual}")
    return payload


def scrape(prefix):
    with urllib.request.urlopen(f"http://127.0.0.1:{metrics_port}/metrics", timeout=15) as response:
        if response.status != 200:
            raise AssertionError(f"GET /metrics on the metrics port: expected 200, got {response.status}")
        text = response.read().decode()
    return sorted(line for line in text.splitlines() if line.startswith(prefix))


try:
    command("mise", "exec", "--", "go", "build", "-o", str(scratch / "server"), "./cmd/nx-remote-cache")
    record("Built ./cmd/nx-remote-cache from the current checkout")
    env = os.environ.copy()
    # Do not inherit production credentials or optional server configuration.
    for key in list(env):
        if key.startswith(("AWS_", "NX_CACHE_", "TLS_")) or key in ("PORT", "METRICS_PORT", "S3_ENDPOINT_URL",
                                                                     "S3_BUCKET_NAME"):
            env.pop(key)
    env.update(GARAGE_DEFAULT_ACCESS_KEY="GK" + secrets.token_hex(12),
               GARAGE_DEFAULT_SECRET_KEY=secrets.token_hex(32), GARAGE_DEFAULT_BUCKET="nx-cloud")
    command("docker", "run", "--detach", "--name", container,
            "--publish", "127.0.0.1::3900", "--mount",
            f"type=bind,src={ROOT / 'dev/garage.toml'},dst=/etc/garage.toml,readonly",
            "--env", "GARAGE_DEFAULT_ACCESS_KEY", "--env", "GARAGE_DEFAULT_SECRET_KEY",
            "--env", "GARAGE_DEFAULT_BUCKET", "dxflrs/garage:v2.5.0", "/garage", "server",
            "--single-node", "--default-bucket", env=env)
    endpoint = command("docker", "port", container, "3900/tcp").splitlines()[0]
    trusted, reader = secrets.token_hex(24), secrets.token_hex(24)
    with socket.socket() as probe:
        probe.bind(("127.0.0.1", 0))
        metrics_port = probe.getsockname()[1]
    env.update(PORT="0", METRICS_PORT=str(metrics_port), AWS_REGION="garage",
               AWS_ACCESS_KEY_ID=env["GARAGE_DEFAULT_ACCESS_KEY"],
               AWS_SECRET_ACCESS_KEY=env["GARAGE_DEFAULT_SECRET_KEY"], S3_BUCKET_NAME="nx-cloud",
               S3_ENDPOINT_URL="http://" + endpoint, NX_CACHE_ACCESS_TOKEN=trusted,
               NX_CACHE_READ_ONLY_TOKEN=reader, NX_CACHE_FAIL_OPEN="false" if args.fail_closed else "true")
    # Invalid startup must fail before serving; capture only the non-secret diagnostic.
    invalid_env = env.copy()
    invalid_env.pop("NX_CACHE_ACCESS_TOKEN")
    invalid = subprocess.run([str(scratch / "server")], env=invalid_env, capture_output=True, timeout=10)
    if invalid.returncode != 1 or b"NX_CACHE_ACCESS_TOKEN" not in invalid.stderr:
        raise AssertionError("Missing-token startup did not exit 1 with its configuration diagnostic")
    record("Startup without access token -> exit 1 with NX_CACHE_ACCESS_TOKEN diagnostic")
    context = None
    scheme = "http"
    if args.tls:
        certs = ROOT / "cmd/nx-remote-cache/testdata"
        env.update(TLS_CERT_PATH=str(certs / "cert.pem"), TLS_KEY_PATH=str(certs / "key.pem"))
        context = ssl.create_default_context(cafile=str(certs / "ca.pem"))
        scheme = "https"
    log = (scratch / "server.log").open("w")
    process = subprocess.Popen([str(scratch / "server")], env=env, stdout=log, stderr=log, cwd=ROOT)
    deadline = time.monotonic() + 60
    while True:
        text = (scratch / "server.log").read_text()
        match = re.search(r'msg="Server running" addr="?\[::\]:(\d+)', text)
        if match:
            break
        if process.poll() is not None or time.monotonic() > deadline:
            raise RuntimeError("Server did not emit its listening address")
        time.sleep(0.1)
    base = f"{scheme}://localhost:{match[1]}"
    record(f"Launch: fresh server pid={process.pid}; {base}; isolated Garage")
    while True:
        try:
            request = urllib.request.Request(base + "/ready")
            with urllib.request.urlopen(request, timeout=3, context=context) as response:
                if response.status == 200 and response.read() == b"OK":
                    break
        except (OSError, urllib.error.URLError):
            if time.monotonic() > deadline:
                raise RuntimeError("Doctor: bucket did not become ready") from None
            time.sleep(0.2)
    record("Doctor: owned process alive, fresh-build listening address, GET /ready -> 200 OK")
    expect("GET", "/health", 200, expected=b"OK")
    expect("GET", "/ready", 200, expected=b"OK")
    path = "/v1/cache/" + secrets.token_hex(16)
    payload = b"nx-cache real-surface verification\x00\xff\n"
    expect("GET", path, 404, token=trusted)
    expect("PUT", path, 200, token=trusted, body=payload)
    expect("GET", path, 200, token=trusted, expected=payload)
    expect("PUT", path, 409, token=trusted, body=b"replacement")
    expect("GET", path, 200, token=reader, expected=payload)
    record("Cache proof: stored bytes survive a rejected overwrite and are readable by the read-only consumer")
    counts = scrape("nx_cache_requests_total{")
    record("Metrics port 127.0.0.1:" + str(metrics_port) + " /metrics: " + "; ".join(counts))
    if counts != ['nx_cache_requests_total{method="GET",outcome="hit"} 2',
                  'nx_cache_requests_total{method="GET",outcome="miss"} 1',
                  'nx_cache_requests_total{method="PUT",outcome="conflict"} 1',
                  'nx_cache_requests_total{method="PUT",outcome="stored"} 1']:
        raise AssertionError("Request counts do not match the driven hits, miss, store and conflict")
    # One stored upload was read; the conflicting one was refused before its body.
    transferred = scrape("nx_cache_received_bytes_total ") + scrape("nx_cache_sent_bytes_total ")
    record("Bytes: " + "; ".join(transferred))
    if transferred != [f"nx_cache_received_bytes_total {len(payload)}",
                       f"nx_cache_sent_bytes_total {2 * len(payload)}"]:
        raise AssertionError("Byte counters do not match one upload and two downloads of the payload")
    durations = scrape("nx_cache_s3_request_duration_seconds_count{")
    record("S3 call counts: " + "; ".join(durations))
    operations = {re.search(r'operation="(\w+)"', line)[1] for line in durations}
    if not {"HeadObject", "PutObject", "GetObject", "HeadBucket"} <= operations:
        raise AssertionError("S3 duration histogram lacks a series for a driven operation")
    expect("GET", "/metrics", 404)
    record("Metrics proof: request, byte and S3 duration metrics on the metrics port; cache port 404s /metrics")
    expect("GET", path, 401)
    expect("GET", path, 401, token="invalid")
    denied = path + "-denied"
    expect("PUT", denied, 403, token=reader, body=payload)
    expect("GET", denied, 404, token=trusted)
    # Restart only the server, not storage, to rule out process-local artifact state.
    process.send_signal(signal.SIGTERM)
    if process.wait(timeout=30) != 0:
        raise AssertionError("Server did not shut down cleanly")
    env["METRICS_PORT"] = "0"
    process = subprocess.Popen([str(scratch / "server")], env=env, stdout=log, stderr=log, cwd=ROOT)
    deadline = time.monotonic() + 30
    while True:
        addresses = re.findall(r'msg="Server running" addr="?\[::\]:(\d+)', (scratch / "server.log").read_text())
        if len(addresses) >= 2:
            base = f"{scheme}://localhost:{addresses[-1]}"
            break
        if process.poll() is not None or time.monotonic() > deadline:
            raise RuntimeError("Restart did not listen")
        time.sleep(0.1)
    expect("GET", path, 200, token=reader, expected=payload)
    record("Durability: same artifact bytes retrieved after cache-server restart")
    try:
        socket.create_connection(("127.0.0.1", metrics_port), timeout=3).close()
    except ConnectionRefusedError:
        record(f"METRICS_PORT=0 restart: connection to former metrics port {metrics_port} refused")
    else:
        raise AssertionError("Metrics port still listens with METRICS_PORT=0")
    command("docker", "stop", "--time", "5", container)
    expect("GET", "/health", 200, expected=b"OK")
    expect("GET", "/ready", 503, expected=b"Storage unavailable")
    expect("GET", path, 503 if args.fail_closed else 404, token=trusted)
    expect("PUT", path + "-outage", 503 if args.fail_closed else 403, token=trusted, body=payload)
    expect("GET", path, 401, token="invalid")
    verdict = "VERIFIED"
except Exception as error:
    verdict = "NOT VERIFIED" if isinstance(error, AssertionError) else "INCONCLUSIVE"
    record(f"{type(error).__name__}: {error}")
finally:
    try:
        if process is not None:
            if process.poll() is None:
                process.send_signal(signal.SIGTERM)
            if process.wait(timeout=30) != 0:
                raise RuntimeError("Server exited unsuccessfully")
        if log is not None:
            log.close()
        if (scratch / "server.log").exists():
            # Server logs contain routes/statuses, never request headers or environment.
            shutil.copyfile(scratch / "server.log", evidence / "server.log")
        existing = command("docker", "ps", "--all", "--quiet", "--filter", "name=^/" + container + "$")
        if existing:
            command("docker", "rm", "--force", "--volumes", container)
        if command("docker", "ps", "--all", "--quiet", "--filter", "name=^/" + container + "$"):
            raise RuntimeError("Owned container remains")
        shutil.rmtree(scratch)
        if scratch.exists() or (process is not None and process.poll() is None):
            raise RuntimeError("Owned process or scratch state remains")
        record("Cleanup: owned server exited; owned container/volumes and scratch directory removed")
    except Exception as error:
        verdict = "INCONCLUSIVE"
        record(f"Cleanup failed: {error}; scratch={scratch}; container={container}")
    record(verdict)
    (evidence / "transcript.txt").write_text("\n".join(records) + "\n")
    (evidence / "result.json").write_text(json.dumps({"verdict": verdict, "tls": args.tls,
                                                   "fail_closed": args.fail_closed}, indent=2) + "\n")
raise SystemExit(0 if verdict == "VERIFIED" else 1)
