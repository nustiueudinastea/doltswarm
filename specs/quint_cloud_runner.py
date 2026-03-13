#!/usr/bin/env python3
"""
Parallel Quint invariant test runner on cloud VMs (Scaleway or Hetzner).

Provisions N VMs, installs Docker, pulls the pre-built quint image,
uploads specs, and distributes invariant checks across VMs using all
available CPUs.

Usage:
    python3 quint_cloud_runner.py --vms 3 --samples 10000
    python3 quint_cloud_runner.py --vms 2 --provider hetzner --samples 10000
    python3 quint_cloud_runner.py --vms 2 --vm-type POP2-HC-16C-32G --samples 50000

Requirements:
    pip install paramiko
    pip install scaleway          # for Scaleway (default)
    pip install hcloud            # for Hetzner
"""

from __future__ import annotations

import abc
import argparse
import json
import os
import re
import socket
import sys
import textwrap
import threading
import time
from concurrent.futures import ThreadPoolExecutor, as_completed
from dataclasses import dataclass, field
from pathlib import Path

try:
    import paramiko
except ImportError:
    sys.exit("ERROR: paramiko is required. Install with: pip install paramiko")


DOCKER_IMAGE = "nustiueudinastea/quint-runner:latest"

# ---------------------------------------------------------------------------
# Data types
# ---------------------------------------------------------------------------

INV_RE = re.compile(r"^\s*val\s+(inv_[A-Za-z0-9_]+)\s*=")
DEFAULT_STEPS_RE = re.compile(r"^\s*///\s*@default_steps\s+(\d+)\s*$")


@dataclass(frozen=True)
class Invariant:
    name: str
    default_steps: int | None


@dataclass
class VMInstance:
    provider_handle: object  # opaque handle for the provider to destroy
    ip: str
    ssh: paramiko.SSHClient | None = None
    cpus: int = 0
    name: str = ""
    created_at: float = field(default_factory=time.monotonic)
    destroyed_at: float | None = None


@dataclass
class InvariantResult:
    invariant: str
    vm_name: str
    passed: bool
    elapsed_seconds: float
    output: str
    error: str = ""


@dataclass
class RunProgress:
    total: int = 0
    completed: int = 0
    passed: int = 0
    failed: int = 0
    lock: threading.Lock = field(default_factory=threading.Lock)

    def record(self, passed: bool) -> tuple[int, int]:
        with self.lock:
            self.completed += 1
            if passed:
                self.passed += 1
            else:
                self.failed += 1
            return self.completed, self.total


# ---------------------------------------------------------------------------
# Invariant parsing
# ---------------------------------------------------------------------------

def parse_invariants(spec_path: Path) -> list[Invariant]:
    lines = spec_path.read_text().splitlines()
    invariants: list[Invariant] = []
    for idx, line in enumerate(lines):
        match = INV_RE.match(line)
        if not match:
            continue
        default_steps = None
        scan = idx - 1
        while scan >= 0 and lines[scan].lstrip().startswith("///"):
            dm = DEFAULT_STEPS_RE.match(lines[scan])
            if dm:
                default_steps = int(dm.group(1))
                break
            scan -= 1
        invariants.append(Invariant(match.group(1), default_steps))
    return invariants


# ---------------------------------------------------------------------------
# SSH helpers
# ---------------------------------------------------------------------------

SPECS_DIR = Path(__file__).parent.resolve()
RUNNER_KEY = SPECS_DIR / "quint-runner-key"


def get_ssh_key_path() -> Path:
    if RUNNER_KEY.exists():
        return RUNNER_KEY
    for name in ("id_ed25519", "id_rsa"):
        p = Path.home() / ".ssh" / name
        if p.exists():
            return p
    sys.exit("ERROR: No SSH key found. Generate one with:\n"
             "  ssh-keygen -t ed25519 -f specs/quint-runner-key -N '' -C quint-cloud-runner")


def get_ssh_public_key() -> str:
    key_path = get_ssh_key_path()
    pub_path = key_path.with_suffix(".pub")
    if not pub_path.exists():
        sys.exit(f"ERROR: Public key not found: {pub_path}")
    return pub_path.read_text().strip()


def wait_for_ssh(ip: str, timeout: int = 300, interval: int = 5) -> None:
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        try:
            sock = socket.create_connection((ip, 22), timeout=5)
            sock.close()
            return
        except (ConnectionRefusedError, TimeoutError, OSError):
            time.sleep(interval)
    raise TimeoutError(f"SSH not reachable on {ip} after {timeout}s")


def connect_ssh(ip: str) -> paramiko.SSHClient:
    key_path = get_ssh_key_path()
    client = paramiko.SSHClient()
    client.set_missing_host_key_policy(paramiko.AutoAddPolicy())
    client.connect(
        hostname=ip,
        username="root",
        key_filename=str(key_path),
        timeout=30,
        auth_timeout=30,
    )
    return client


def ssh_exec(
    ssh: paramiko.SSHClient,
    cmd: str,
    *,
    timeout: int = 600,
    check: bool = True,
    stream: bool = False,
    prefix: str = "",
) -> tuple[int, str, str]:
    """Execute command over SSH. Returns (exit_code, stdout, stderr)."""
    chan = ssh.get_transport().open_session()
    chan.set_combine_stderr(False)
    chan.settimeout(timeout)
    chan.exec_command(cmd)

    stdout_chunks: list[str] = []
    stderr_chunks: list[str] = []

    stdout_stream = chan.makefile("rb", -1)
    stderr_stream = chan.makefile_stderr("rb", -1)

    if stream:
        for raw_line in stdout_stream:
            line = raw_line.decode("utf-8", errors="replace").rstrip("\n")
            stdout_chunks.append(line)
            if prefix:
                print(f"  [{prefix}] {line}", flush=True)
            else:
                print(f"  {line}", flush=True)
        for raw_line in stderr_stream:
            stderr_chunks.append(raw_line.decode("utf-8", errors="replace").rstrip("\n"))
    else:
        stdout_chunks = [stdout_stream.read().decode("utf-8", errors="replace")]
        stderr_chunks = [stderr_stream.read().decode("utf-8", errors="replace")]

    exit_code = chan.recv_exit_status()
    stdout = "\n".join(stdout_chunks)
    stderr = "\n".join(stderr_chunks)

    if check and exit_code != 0:
        raise RuntimeError(
            f"Command failed (exit {exit_code}): {cmd}\n"
            f"stdout: {stdout[:500]}\nstderr: {stderr[:500]}"
        )
    return exit_code, stdout, stderr


def scp_upload(ssh: paramiko.SSHClient, local_path: Path, remote_path: str) -> None:
    sftp = ssh.open_sftp()
    try:
        sftp.put(str(local_path), remote_path)
    finally:
        sftp.close()


# ---------------------------------------------------------------------------
# Cloud provider interface
# ---------------------------------------------------------------------------

class CloudProvider(abc.ABC):
    @abc.abstractmethod
    def init(self) -> None:
        """Validate credentials and prepare provider state."""

    @abc.abstractmethod
    def create_vm(self, name: str, vm_type: str, location: str) -> VMInstance:
        """Create a VM and return it with a public IP."""

    @abc.abstractmethod
    def destroy_vm(self, vm: VMInstance) -> None:
        """Destroy a VM."""

    @property
    @abc.abstractmethod
    def default_vm_type(self) -> str: ...

    @property
    @abc.abstractmethod
    def default_location(self) -> str: ...

    @property
    @abc.abstractmethod
    def name(self) -> str: ...

    @abc.abstractmethod
    def price_per_hour(self, vm_type: str) -> float | None:
        """Return EUR/hour for the given VM type, or None if unknown."""


# ---------------------------------------------------------------------------
# Scaleway provider
# ---------------------------------------------------------------------------

class ScalewayProvider(CloudProvider):
    # Ubuntu 24.04 Noble Numbat (marketplace ID) — resolved per-zone at init
    UBUNTU_MARKETPLACE_ID = "607b12c2-685d-45f7-905f-57bc23863834"
    PROJECT_NAME = "testing"
    ZONES = ["fr-par-1", "fr-par-2", "fr-par-3", "nl-ams-1", "nl-ams-2",
             "nl-ams-3", "pl-waw-1", "pl-waw-2", "pl-waw-3"]

    def __init__(self) -> None:
        self._api = None
        self._project_id: str | None = None
        self._image_by_zone: dict[str, str] = {}

    @property
    def name(self) -> str:
        return "scaleway"

    @property
    def default_vm_type(self) -> str:
        return "POP2-HC-8C-16G"

    @property
    def default_location(self) -> str:
        return "fr-par-1"

    def init(self) -> None:
        try:
            from scaleway import Client as ScwClient
            from scaleway.instance.v1 import InstanceV1API
            from scaleway.account.v3 import AccountV3ProjectAPI
            from scaleway.iam.v1alpha1 import IamV1Alpha1API
        except ImportError:
            sys.exit("ERROR: scaleway is required. Install with: pip install scaleway")

        client = ScwClient.from_config_file_and_env()
        self._api = InstanceV1API(client)

        # Resolve project ID from name
        projects_api = AccountV3ProjectAPI(client)
        projects = projects_api.list_projects(page_size=100)
        for p in projects.projects:
            if p.name == self.PROJECT_NAME:
                self._project_id = p.id
                print(f"  Using Scaleway project: {p.name} ({p.id})")
                break
        if not self._project_id:
            sys.exit(f"ERROR: Scaleway project '{self.PROJECT_NAME}' not found")

        # Resolve Ubuntu 24.04 image IDs per zone (POP2 needs instance_sbs)
        from scaleway.marketplace.v2 import MarketplaceV2API
        mp = MarketplaceV2API(client)
        for zone in self.ZONES:
            local_images = mp.list_local_images(
                image_id=self.UBUNTU_MARKETPLACE_ID, zone=zone, page_size=10,
            )
            for li in local_images.local_images:
                if li.arch == "x86_64" and li.type_ == "instance_sbs":
                    self._image_by_zone[zone] = li.id
                    break
        print(f"  Resolved Ubuntu 24.04 images for {len(self._image_by_zone)} zones")

        # Ensure SSH key is registered in Scaleway IAM
        pub_key = get_ssh_public_key()
        iam = IamV1Alpha1API(client)
        existing_keys = iam.list_ssh_keys(page_size=100)
        found = False
        for k in existing_keys.ssh_keys:
            if k.public_key.strip() == pub_key:
                print(f"  SSH key already registered: {k.name}")
                found = True
                break
        if not found:
            iam.create_ssh_key(name="quint-runner", public_key=pub_key)
            print(f"  Registered SSH key: quint-runner")

    def create_vm(self, name: str, vm_type: str, location: str) -> VMInstance:
        from scaleway.instance.v1.types import ServerAction, ServerState

        zones = [location] + [z for z in self.ZONES if z != location]
        last_error = None

        for zone in zones:
            image_id = self._image_by_zone.get(zone)
            if not image_id:
                continue  # no image available in this zone

            try:
                from scaleway.instance.v1.types import VolumeServerTemplate

                resp = self._api._create_server(
                    zone=zone,
                    commercial_type=vm_type,
                    name=name,
                    image=image_id,
                    project=self._project_id,
                    dynamic_ip_required=True,
                    protected=False,
                    volumes={
                        "0": VolumeServerTemplate(
                            boot=True,
                            size=10_000_000_000,  # 10 GB block storage
                            volume_type="sbs_volume",
                        ),
                    },
                )
                server = resp.server
                server_id = server.id

                # Power on
                self._api.server_action(
                    server_id=server_id, zone=zone,
                    action=ServerAction.POWERON,
                )

                # Wait for running + IP
                ip = None
                deadline = time.monotonic() + 120
                while time.monotonic() < deadline:
                    info = self._api.get_server(
                        server_id=server_id, zone=zone,
                    )
                    if info.server.state == ServerState.RUNNING and info.server.public_ip:
                        ip = info.server.public_ip.address
                        break
                    time.sleep(5)

                if not ip:
                    # Cleanup failed VM
                    try:
                        self._api.server_action(
                            server_id=server_id, zone=zone,
                            action=ServerAction.TERMINATE,
                        )
                    except Exception:
                        pass
                    raise RuntimeError(f"VM {name} did not get a public IP in {zone}")

                print(f"  Created VM {name} ({vm_type}) at {ip} [{zone}]")
                return VMInstance(
                    provider_handle={"id": server_id, "zone": zone},
                    ip=ip,
                    name=name,
                )
            except Exception as e:
                last_error = e
                if "unavailable" in str(e).lower() or "quota" in str(e).lower():
                    print(f"  [{name}] Zone {zone} unavailable, trying next...")
                    continue
                raise

        raise RuntimeError(f"Failed to create VM {name} in any zone: {last_error}")

    def destroy_vm(self, vm: VMInstance) -> None:
        from scaleway.instance.v1.types import ServerAction
        handle = vm.provider_handle
        try:
            self._api.server_action(
                server_id=handle["id"],
                zone=handle["zone"],
                action=ServerAction.TERMINATE,
            )
            print(f"  Destroyed VM {vm.name}")
        except Exception as e:
            print(f"  WARNING: Failed to destroy {vm.name}: {e}")

    # Scaleway hourly pricing (EUR) — https://www.scaleway.com/en/pricing/
    _PRICING = {
        "POP2-HC-2C-4G":   0.07,
        "POP2-HC-4C-8G":   0.14,
        "POP2-HC-8C-16G":  0.28,
        "POP2-HC-16C-32G": 0.55,
        "POP2-HC-32C-64G": 1.10,
    }

    def price_per_hour(self, vm_type: str) -> float | None:
        return self._PRICING.get(vm_type)


# ---------------------------------------------------------------------------
# Hetzner provider
# ---------------------------------------------------------------------------

class HetznerProvider(CloudProvider):
    LOCATIONS = ["hel1", "fsn1", "nbg1", "ash", "hil"]

    def __init__(self) -> None:
        self._client = None
        self._ssh_key = None

    @property
    def name(self) -> str:
        return "hetzner"

    @property
    def default_vm_type(self) -> str:
        return "ccx33"

    @property
    def default_location(self) -> str:
        return "hel1"

    def init(self) -> None:
        try:
            from hcloud import Client
        except ImportError:
            sys.exit("ERROR: hcloud is required. Install with: pip install hcloud")

        token = os.environ.get("HCLOUD_TOKEN")
        if not token:
            sys.exit("ERROR: HCLOUD_TOKEN environment variable not set")

        self._client = Client(token=token)

        # Register SSH key
        pub_key = get_ssh_public_key()
        existing = self._client.ssh_keys.get_all()
        for k in existing:
            if k.public_key.strip() == pub_key:
                print(f"  SSH key already registered: {k.name}")
                self._ssh_key = k
                return
        key = self._client.ssh_keys.create(name="quint-runner", public_key=pub_key)
        print(f"  Registered SSH key: {key.data_model.name}")
        self._ssh_key = key

    def create_vm(self, name: str, vm_type: str, location: str) -> VMInstance:
        from hcloud.images import Image
        from hcloud.locations import Location
        from hcloud.server_types import ServerType

        locations = [location] + [l for l in self.LOCATIONS if l != location]
        last_error = None

        for loc in locations:
            try:
                response = self._client.servers.create(
                    name=name,
                    server_type=ServerType(name=vm_type),
                    image=Image(name="ubuntu-24.04"),
                    ssh_keys=[self._ssh_key],
                    location=Location(name=loc),
                )
                server = response.server
                ip = server.public_net.ipv4.ip
                print(f"  Created VM {name} ({vm_type}) at {ip} [{loc}]")
                return VMInstance(
                    provider_handle=server,
                    ip=ip,
                    name=name,
                )
            except Exception as e:
                last_error = e
                print(f"  [{name}] Location {loc} unavailable, trying next...")

        raise RuntimeError(f"Failed to create VM {name} in any location: {last_error}")

    def destroy_vm(self, vm: VMInstance) -> None:
        try:
            self._client.servers.delete(vm.provider_handle)
            print(f"  Destroyed VM {vm.name}")
        except Exception as e:
            print(f"  WARNING: Failed to destroy {vm.name}: {e}")

    # Hetzner hourly pricing (EUR) — https://docs.hetzner.cloud/
    _PRICING = {
        "ccx13":  0.07,
        "ccx23":  0.14,
        "ccx33":  0.27,
        "ccx43":  0.53,
        "ccx53":  1.05,
        "ccx63":  2.10,
        "cpx11":  0.005,
        "cpx21":  0.01,
        "cpx31":  0.02,
        "cpx41":  0.04,
        "cpx51":  0.08,
    }

    def price_per_hour(self, vm_type: str) -> float | None:
        return self._PRICING.get(vm_type)


# ---------------------------------------------------------------------------
# VM provisioning (provider-agnostic)
# ---------------------------------------------------------------------------

def provision_vm(vm: VMInstance, specs_dir: Path) -> None:
    """Install Docker, pull image, upload specs."""
    print(f"  [{vm.name}] Waiting for SSH...")
    wait_for_ssh(vm.ip)
    vm.ssh = connect_ssh(vm.ip)

    print(f"  [{vm.name}] Installing Docker and pulling image...")
    install_script = textwrap.dedent(f"""\
        set -euo pipefail
        export DEBIAN_FRONTEND=noninteractive
        apt-get update -qq
        apt-get install -y -qq docker.io > /dev/null 2>&1
        systemctl enable --now docker
        docker pull {DOCKER_IMAGE}
    """)
    ssh_exec(vm.ssh, install_script, timeout=300, prefix=vm.name)

    # Detect CPU count
    _, cpu_out, _ = ssh_exec(vm.ssh, "nproc")
    vm.cpus = int(cpu_out.strip())
    print(f"  [{vm.name}] Docker ready — {vm.cpus} CPUs")

    # Upload spec files
    print(f"  [{vm.name}] Uploading spec files...")
    ssh_exec(vm.ssh, "mkdir -p /root/specs /root/specs/spells")
    for qnt_file in specs_dir.glob("*.qnt"):
        scp_upload(vm.ssh, qnt_file, f"/root/specs/{qnt_file.name}")
    spells_dir = specs_dir / "spells"
    if spells_dir.exists():
        for spell_file in spells_dir.glob("*.qnt"):
            scp_upload(vm.ssh, spell_file, f"/root/specs/spells/{spell_file.name}")
    print(f"  [{vm.name}] Ready")


def destroy_vms(provider: CloudProvider, vms: list[VMInstance]) -> None:
    for vm in vms:
        try:
            if vm.ssh:
                vm.ssh.close()
        except Exception:
            pass
        provider.destroy_vm(vm)
        vm.destroyed_at = time.monotonic()


# ---------------------------------------------------------------------------
# Invariant execution
# ---------------------------------------------------------------------------

def run_invariant_on_vm(
    vm: VMInstance,
    invariant: Invariant,
    samples: int,
    steps: int | None,
    fallback_steps: int,
) -> InvariantResult:
    effective_steps = steps if steps is not None else (invariant.default_steps or fallback_steps)

    docker_cmd = (
        f"docker run --rm -v /root/specs:/specs {DOCKER_IMAGE} "
        f"quint run /specs/doltswarm_verify.qnt "
        f"--backend=rust "
        f"--invariant={invariant.name} "
        f"--max-samples={samples} "
        f"--max-steps={effective_steps}"
    )

    started = time.monotonic()
    try:
        exit_code, stdout, stderr = ssh_exec(
            vm.ssh, docker_cmd, timeout=7200, check=False
        )
    except (TimeoutError, socket.timeout, paramiko.buffered_pipe.PipeTimeout) as e:
        elapsed = time.monotonic() - started
        return InvariantResult(
            invariant=invariant.name,
            vm_name=vm.name,
            passed=False,
            elapsed_seconds=elapsed,
            output="",
            error=f"SSH timeout after {elapsed:.0f}s: {e}",
        )
    elapsed = time.monotonic() - started

    passed = exit_code == 0
    return InvariantResult(
        invariant=invariant.name,
        vm_name=vm.name,
        passed=passed,
        elapsed_seconds=elapsed,
        output=stdout,
        error=stderr if not passed else "",
    )


def distribute_invariants(
    invariants: list[Invariant],
    vms: list[VMInstance],
) -> dict[str, list[Invariant]]:
    """Distribute invariants across VMs weighted by CPU count."""
    total_cpus = sum(vm.cpus for vm in vms)
    if total_cpus == 0:
        total_cpus = len(vms)

    sorted_invs = sorted(
        invariants,
        key=lambda inv: inv.default_steps or 20,
        reverse=True,
    )

    vm_load: dict[str, float] = {vm.name: 0.0 for vm in vms}
    vm_capacity: dict[str, float] = {vm.name: vm.cpus / total_cpus for vm in vms}
    assignment: dict[str, list[Invariant]] = {vm.name: [] for vm in vms}

    for inv in sorted_invs:
        best_vm = min(
            vms,
            key=lambda v: vm_load[v.name] / max(vm_capacity[v.name], 0.01),
        )
        assignment[best_vm.name].append(inv)
        vm_load[best_vm.name] += 1

    return assignment


def run_vm_batch(
    vm: VMInstance,
    invariants: list[Invariant],
    samples: int,
    steps: int | None,
    fallback_steps: int,
    progress: RunProgress,
) -> list[InvariantResult]:
    """Run a batch of invariants on a VM, parallelising up to vm.cpus containers."""
    results: list[InvariantResult] = []
    concurrency = max(1, vm.cpus)

    def run_one(inv: Invariant) -> InvariantResult:
        result = run_invariant_on_vm(vm, inv, samples, steps, fallback_steps)
        done, total = progress.record(result.passed)
        status = "PASS" if result.passed else "FAIL"
        print(
            f"  [{done}/{total}] {status} {inv.name} "
            f"on {vm.name} ({result.elapsed_seconds:.1f}s)",
            flush=True,
        )
        return result

    with ThreadPoolExecutor(max_workers=concurrency) as pool:
        futures = {pool.submit(run_one, inv): inv for inv in invariants}
        for future in as_completed(futures):
            results.append(future.result())

    return results


# ---------------------------------------------------------------------------
# Report
# ---------------------------------------------------------------------------

def print_report(
    results: list[InvariantResult],
    wall_seconds: float,
    vms: list[VMInstance] | None = None,
    provider: CloudProvider | None = None,
    vm_type: str | None = None,
) -> None:
    passed = [r for r in results if r.passed]
    failed = [r for r in results if not r.passed]

    total_cpu_seconds = sum(r.elapsed_seconds for r in results)

    print("\n" + "=" * 70)
    print("RESULTS SUMMARY")
    print("=" * 70)
    print(f"Total: {len(results)}  Passed: {len(passed)}  Failed: {len(failed)}")
    print(f"Wall time: {wall_seconds:.1f}s ({wall_seconds / 60:.1f}m)")
    print(f"Total CPU time: {total_cpu_seconds:.1f}s ({total_cpu_seconds / 60:.1f}m)")

    if vms and provider and vm_type:
        price = provider.price_per_hour(vm_type)
        if price is not None:
            total_vm_hours = 0.0
            for vm in vms:
                end = vm.destroyed_at if vm.destroyed_at else time.monotonic()
                vm_hours = (end - vm.created_at) / 3600
                # Cloud providers bill per started hour minimum
                total_vm_hours += max(vm_hours, 1 / 60)
            total_cost = total_vm_hours * price
            print(f"Estimated cost: EUR {total_cost:.3f} "
                  f"({len(vms)} VMs x {vm_type} @ EUR {price}/h)")
        else:
            print(f"Cost: unknown pricing for VM type {vm_type}")

    print()

    if failed:
        print("FAILURES:")
        for r in sorted(failed, key=lambda x: x.invariant):
            print(f"  FAIL  {r.invariant}  (on {r.vm_name}, {r.elapsed_seconds:.1f}s)")
            if r.error:
                for line in r.error.strip().splitlines()[:10]:
                    print(f"        {line}")
            if r.output:
                for line in r.output.strip().splitlines()[-10:]:
                    print(f"        {line}")
        print()

    print("ALL RESULTS:")
    for r in sorted(results, key=lambda x: x.invariant):
        status = "PASS" if r.passed else "FAIL"
        print(f"  {status}  {r.invariant:<60s} {r.elapsed_seconds:7.1f}s  {r.vm_name}")


# ---------------------------------------------------------------------------
# Log saving
# ---------------------------------------------------------------------------

def save_logs(
    results: list[InvariantResult],
    output_dir: Path,
    wall_seconds: float,
    args: argparse.Namespace,
    vms: list[VMInstance] | None = None,
    provider: CloudProvider | None = None,
) -> None:
    """Save per-invariant stdout/stderr to local files for inspection."""
    output_dir.mkdir(parents=True, exist_ok=True)

    sorted_results = sorted(results, key=lambda r: r.invariant)
    passed = [r for r in results if r.passed]
    failed = [r for r in results if not r.passed]
    times = [r.elapsed_seconds for r in results]

    # Write individual log files per invariant
    for r in sorted_results:
        status = "pass" if r.passed else "fail"
        log_path = output_dir / f"{r.invariant}.{status}.log"
        with open(log_path, "w") as f:
            f.write(f"# invariant: {r.invariant}\n")
            f.write(f"# status: {'PASS' if r.passed else 'FAIL'}\n")
            f.write(f"# vm: {r.vm_name}\n")
            f.write(f"# elapsed: {r.elapsed_seconds:.2f}s\n")
            f.write(f"# samples: {args.samples}\n")
            f.write(f"# steps: {args.steps or 'per-annotation'}\n")
            f.write("\n--- stdout ---\n")
            f.write(r.output)
            if r.error:
                f.write("\n\n--- stderr ---\n")
                f.write(r.error)
            f.write("\n")

    # Categorise failures by error type
    violation_fails: list[InvariantResult] = []
    runtime_error_fails: list[InvariantResult] = []
    other_fails: list[InvariantResult] = []
    for r in failed:
        combined = (r.output + r.error).lower()
        if "invariant violated" in combined:
            violation_fails.append(r)
        elif "runtime error" in combined or "error [qnt" in combined:
            runtime_error_fails.append(r)
        else:
            other_fails.append(r)

    # Write summary
    summary_path = output_dir / "summary.txt"
    with open(summary_path, "w") as f:
        f.write(f"Quint Cloud Runner Results\n")
        f.write(f"{'=' * 70}\n\n")

        # Config
        f.write(f"Date:       {time.strftime('%Y-%m-%d %H:%M:%S')}\n")
        f.write(f"Provider:   {args.provider}\n")
        f.write(f"VMs:        {args.vms} x {args.vm_type}\n")
        f.write(f"Samples:    {args.samples}\n")
        f.write(f"Steps:      {args.steps or 'per-annotation'}\n")
        f.write(f"Spec:       {args.spec}\n\n")

        # Overall stats
        f.write(f"{'=' * 70}\n")
        f.write(f"OVERALL: {len(results)} total, "
                f"{len(passed)} passed, {len(failed)} failed\n")
        f.write(f"{'=' * 70}\n\n")

        f.write(f"Wall time:      {wall_seconds:.1f}s ({wall_seconds / 60:.1f}m)\n")
        if times:
            f.write(f"Avg per check:  {sum(times) / len(times):.1f}s\n")
            f.write(f"Fastest check:  {min(times):.1f}s\n")
            f.write(f"Slowest check:  {max(times):.1f}s\n")
            total_cpu = sum(times)
            f.write(f"Total CPU time: {total_cpu:.1f}s ({total_cpu / 60:.1f}m)\n")
            if wall_seconds > 0:
                f.write(f"Parallelism:    {total_cpu / wall_seconds:.1f}x\n")

        if vms and provider:
            price = provider.price_per_hour(args.vm_type)
            if price is not None:
                total_vm_hours = 0.0
                for vm in vms:
                    end = vm.destroyed_at if vm.destroyed_at else time.monotonic()
                    total_vm_hours += max((end - vm.created_at) / 3600, 1 / 60)
                cost = total_vm_hours * price
                f.write(f"Est. cost:      EUR {cost:.3f} "
                        f"({len(vms)} VMs x {args.vm_type} @ EUR {price}/h)\n")

        f.write("\n")

        # Failure breakdown
        if failed:
            f.write(f"{'=' * 70}\n")
            f.write(f"FAILURE BREAKDOWN\n")
            f.write(f"{'=' * 70}\n\n")

            if violation_fails:
                f.write(f"Invariant violations ({len(violation_fails)}):\n")
                for r in sorted(violation_fails, key=lambda x: x.invariant):
                    f.write(f"  FAIL  {r.invariant}  ({r.elapsed_seconds:.1f}s, {r.vm_name})\n")
                f.write("\n")

            if runtime_error_fails:
                f.write(f"Runtime errors ({len(runtime_error_fails)}):\n")
                for r in sorted(runtime_error_fails, key=lambda x: x.invariant):
                    # Extract first error line for a quick hint
                    hint = ""
                    for line in (r.output + r.error).splitlines():
                        if "error" in line.lower() and "qnt" in line.lower():
                            hint = line.strip()[:80]
                            break
                    f.write(f"  FAIL  {r.invariant}  ({r.elapsed_seconds:.1f}s, {r.vm_name})\n")
                    if hint:
                        f.write(f"        {hint}\n")
                f.write("\n")

            if other_fails:
                f.write(f"Other failures ({len(other_fails)}):\n")
                for r in sorted(other_fails, key=lambda x: x.invariant):
                    f.write(f"  FAIL  {r.invariant}  ({r.elapsed_seconds:.1f}s, {r.vm_name})\n")
                f.write("\n")

        # Full results table
        f.write(f"{'=' * 70}\n")
        f.write(f"ALL RESULTS\n")
        f.write(f"{'=' * 70}\n\n")
        for r in sorted_results:
            status = "PASS" if r.passed else "FAIL"
            f.write(f"  {status}  {r.invariant:<60s} {r.elapsed_seconds:7.1f}s  {r.vm_name}\n")

    print(f"\nLogs saved to {output_dir}/")
    print(f"  summary.txt            — stats, failure breakdown, full results")
    print(f"  <invariant>.pass.log   — full stdout for passing checks")
    print(f"  <invariant>.fail.log   — full stdout+stderr for failures")


# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------

PROVIDERS = {
    "scaleway": ScalewayProvider,
    "hetzner": HetznerProvider,
}


def main() -> None:
    parser = argparse.ArgumentParser(
        description="Run Quint invariant tests in parallel on cloud VMs.",
        formatter_class=argparse.RawDescriptionHelpFormatter,
        epilog=textwrap.dedent("""\
            Environment:
              Scaleway: uses ~/.config/scw/config.yaml or SCW_* env vars
              Hetzner:  HCLOUD_TOKEN env var

            Examples:
              %(prog)s --vms 3 --samples 10000
              %(prog)s --vms 2 --provider hetzner --samples 10000
              %(prog)s --vms 2 --vm-type POP2-HC-16C-32G --samples 50000 --steps 30
              %(prog)s --vms 4 --samples 10000 --invariant inv_lineage_lca_sound
              %(prog)s --vms 1 --samples 1000 --keep-vms
        """),
    )
    parser.add_argument(
        "--provider", choices=list(PROVIDERS.keys()), default="scaleway",
        help="Cloud provider (default: scaleway)",
    )
    parser.add_argument(
        "--vms", type=int, required=True,
        help="Number of VMs to run in parallel",
    )
    parser.add_argument(
        "--vm-type", default=None,
        help="VM type (default: provider-specific — POP2-HC-8C-16G for Scaleway, ccx33 for Hetzner)",
    )
    parser.add_argument(
        "--location", default=None,
        help="Datacenter zone/location (default: provider-specific)",
    )
    parser.add_argument(
        "--samples", type=int, required=True,
        help="Number of random traces per invariant (--max-samples)",
    )
    parser.add_argument(
        "--steps", type=int, default=None,
        help="Override max steps for all invariants (otherwise uses @default_steps)",
    )
    parser.add_argument(
        "--fallback-steps", type=int, default=20,
        help="Steps when no @default_steps annotation exists (default: 20)",
    )
    parser.add_argument(
        "--spec", default="doltswarm_verify.qnt",
        help="Quint spec file (default: doltswarm_verify.qnt)",
    )
    parser.add_argument(
        "--invariant",
        help="Run only this specific invariant",
    )
    parser.add_argument(
        "--keep-vms", action="store_true",
        help="Do not destroy VMs after run (for debugging)",
    )
    parser.add_argument(
        "--json-output",
        help="Write results to a JSON file",
    )
    parser.add_argument(
        "--output-dir", default="cloud-results",
        help="Directory to save per-invariant output logs (default: cloud-results/)",
    )
    args = parser.parse_args()

    # Create provider
    provider = PROVIDERS[args.provider]()
    if args.vm_type is None:
        args.vm_type = provider.default_vm_type
    if args.location is None:
        args.location = provider.default_location

    specs_dir = Path(__file__).parent.resolve()
    spec_path = specs_dir / args.spec
    if not spec_path.exists():
        sys.exit(f"ERROR: Spec file not found: {spec_path}")

    invariants = parse_invariants(spec_path)
    if args.invariant:
        matched = [inv for inv in invariants if inv.name == args.invariant]
        if not matched:
            sys.exit(f"ERROR: Unknown invariant: {args.invariant}")
        invariants = matched

    print(f"Found {len(invariants)} invariant(s) to check")
    print(f"Provider: {provider.name}")
    print(f"Config: {args.vms} VM(s) x {args.vm_type}, "
          f"samples={args.samples}, steps={args.steps or 'per-annotation'}")
    print(f"Docker image: {DOCKER_IMAGE}")
    print()

    # Init provider (validates credentials)
    print("Initializing provider...")
    provider.init()
    print()

    # Create timestamped output directory
    run_timestamp = time.strftime("%Y%m%d-%H%M%S")
    run_output_dir = specs_dir / args.output_dir / run_timestamp
    run_output_dir.mkdir(parents=True, exist_ok=True)
    print(f"Output directory: {run_output_dir}")
    print()

    vms: list[VMInstance] = []
    destroyed_vms: set[str] = set()
    wall_start = time.monotonic()

    try:
        # Phase 1: Create VMs
        print("Phase 1: Creating VMs...")
        with ThreadPoolExecutor(max_workers=args.vms) as pool:
            futures = {
                pool.submit(
                    provider.create_vm,
                    f"quint-runner-{i + 1}",
                    args.vm_type,
                    args.location,
                ): i
                for i in range(args.vms)
            }
            errors = []
            for f in as_completed(futures):
                try:
                    vms.append(f.result())
                except Exception as e:
                    errors.append(e)
        if errors:
            print(f"\n  ERROR: Failed to create {len(errors)}/{args.vms} VM(s):")
            for e in errors:
                print(f"    {e}")
            raise RuntimeError("Not all VMs could be created — aborting")
        print()

        # Phase 2: Provision VMs (Docker + pull image + upload specs)
        print("Phase 2: Provisioning VMs...")
        with ThreadPoolExecutor(max_workers=len(vms)) as pool:
            list(pool.map(lambda vm: provision_vm(vm, specs_dir), vms))
        print()

        # Phase 3: Distribute and run invariants
        print("Phase 3: Running invariant checks...")
        assignment = distribute_invariants(invariants, vms)
        for vm in vms:
            assigned = assignment[vm.name]
            print(f"  {vm.name} ({vm.cpus} CPUs): {len(assigned)} invariants")
        print()

        progress = RunProgress(total=len(invariants))
        all_results: list[InvariantResult] = []

        with ThreadPoolExecutor(max_workers=len(vms)) as pool:
            futures = {
                pool.submit(
                    run_vm_batch,
                    vm,
                    assignment[vm.name],
                    args.samples,
                    args.steps,
                    args.fallback_steps,
                    progress,
                ): vm
                for vm in vms
                if assignment[vm.name]
            }
            for f in as_completed(futures):
                all_results.extend(f.result())
                vm = futures[f]
                if not args.keep_vms:
                    print(f"  [{vm.name}] All invariants done — destroying VM")
                    provider.destroy_vm(vm)
                    vm.destroyed_at = time.monotonic()
                    destroyed_vms.add(vm.name)

        wall_elapsed = time.monotonic() - wall_start

        print_report(all_results, wall_elapsed, vms, provider, args.vm_type)

        # Save per-invariant logs
        save_logs(all_results, run_output_dir, wall_elapsed, args, vms, provider)

        if args.json_output:
            json_data = {
                "config": {
                    "provider": args.provider,
                    "vms": args.vms,
                    "vm_type": args.vm_type,
                    "samples": args.samples,
                    "steps": args.steps,
                    "fallback_steps": args.fallback_steps,
                },
                "wall_seconds": wall_elapsed,
                "total_cpu_seconds": round(sum(r.elapsed_seconds for r in all_results), 2),
                "estimated_cost_eur": None,
                "total": len(all_results),
                "passed": sum(1 for r in all_results if r.passed),
                "failed": sum(1 for r in all_results if not r.passed),
                "results": [
                    {
                        "invariant": r.invariant,
                        "vm": r.vm_name,
                        "passed": r.passed,
                        "elapsed_seconds": round(r.elapsed_seconds, 2),
                        "error": r.error[:500] if r.error else "",
                    }
                    for r in sorted(all_results, key=lambda x: x.invariant)
                ],
            }
            price = provider.price_per_hour(args.vm_type)
            if price is not None:
                total_vm_hours = 0.0
                for vm in vms:
                    end = vm.destroyed_at if vm.destroyed_at else time.monotonic()
                    total_vm_hours += max((end - vm.created_at) / 3600, 1 / 60)
                json_data["estimated_cost_eur"] = round(total_vm_hours * price, 4)
            out_path = Path(args.json_output)
            out_path.write_text(json.dumps(json_data, indent=2) + "\n")
            print(f"\nResults written to {out_path}")

        if any(not r.passed for r in all_results):
            sys.exit(1)

    finally:
        remaining_vms = [vm for vm in vms if vm.name not in destroyed_vms]
        if not args.keep_vms:
            if remaining_vms:
                print("\nCleaning up remaining VMs...")
                destroy_vms(provider, remaining_vms)
        else:
            print("\n--keep-vms set. VMs left running:")
            for vm in remaining_vms:
                print(f"  {vm.name}: ssh root@{vm.ip}")


if __name__ == "__main__":
    main()
