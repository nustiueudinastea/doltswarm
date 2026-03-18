#!/usr/bin/env python3
"""
Parallel Quint invariant test runner on cloud VMs (Scaleway, Hetzner, or UpCloud).

Provisions N VMs, installs Docker, pulls the pre-built quint image,
uploads specs, and distributes invariant checks across VMs using all
available CPUs.

Usage:
    python3 specs/tooling/quint_cloud_runner.py --vms 3 --samples 10000
    python3 specs/tooling/quint_cloud_runner.py --vms 2 --provider hetzner --samples 10000
    python3 specs/tooling/quint_cloud_runner.py --vms 2 --provider upcloud --samples 10000
    python3 specs/tooling/quint_cloud_runner.py --vms 2 --vm-type POP2-HC-16C-32G --samples 50000

Requirements:
    pip install paramiko
    pip install scaleway          # for Scaleway (default)
    pip install hcloud            # for Hetzner
    pip install upcloud-api       # for UpCloud
"""

from __future__ import annotations

import abc
import argparse
import json
import os
import re
import signal
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

MONITOR_SCRIPT = r"""#!/bin/bash
# Resource monitor: writes CSV to /root/monitor.csv every second
CSV=/root/monitor.csv
echo "ts,cpu_pct,mem_used_mb,mem_total_mb,load_1m,nprocs" > "$CSV"
prev_idle=0; prev_total=0
while true; do
    read -r _ user nice system idle iowait irq softirq steal _ < /proc/stat
    total=$((user+nice+system+idle+iowait+irq+softirq+steal))
    if [ "$prev_total" -ne 0 ]; then
        dt=$((total-prev_total)); di=$((idle-prev_idle))
        [ "$dt" -gt 0 ] && cpu=$((100*(dt-di)/dt)) || cpu=0
        mt=$(awk '/MemTotal/{print int($2/1024)}' /proc/meminfo)
        ma=$(awk '/MemAvailable/{print int($2/1024)}' /proc/meminfo)
        mu=$((mt-ma))
        la=$(awk '{print $1}' /proc/loadavg)
        np=$(awk '{print $4}' /proc/loadavg | cut -d/ -f2)
        echo "$(date +%s),$cpu,$mu,$mt,$la,$np" >> "$CSV"
    fi
    prev_idle=$idle; prev_total=$total
    sleep 1
done
"""

# ---------------------------------------------------------------------------
# Graceful shutdown support
# ---------------------------------------------------------------------------

_shutdown_requested = threading.Event()


_all_vms: list = []  # populated in main() for shutdown handler access
_provider_ref: list = []  # [provider] for shutdown handler


def _shutdown_handler(signum, frame):
    """Handle SIGINT/SIGTERM by setting the shutdown flag and unblocking SSH."""
    sig_name = signal.Signals(signum).name
    if _shutdown_requested.is_set():
        # Second signal — force-destroy VMs and exit immediately
        vm_names = [vm.name for vm in _all_vms] if _all_vms else []
        print(f"\n[shutdown] {sig_name} received again — force-destroying {len(vm_names)} VM(s) and exiting",
              flush=True)
        if _provider_ref and _all_vms:
            for vm in _all_vms:
                try:
                    print(f"[shutdown] Force-destroying {vm.name} ({vm.ip})...", flush=True)
                    _provider_ref[0].destroy_vm(vm)
                except Exception as e:
                    print(f"[shutdown] Failed to destroy {vm.name}: {e}", flush=True)
        print("[shutdown] Done. Exiting.", flush=True)
        os._exit(1)
    vm_names = [vm.name for vm in _all_vms] if _all_vms else []
    print(f"\n[shutdown] {sig_name} received — stopping all work and destroying {len(vm_names)} VM(s): "
          f"{', '.join(vm_names) or 'none yet'}", flush=True)
    print("[shutdown] Closing SSH connections to unblock running commands...", flush=True)
    _shutdown_requested.set()
    # Close all SSH transports to unblock any blocking reads in ssh_exec
    for vm in _all_vms:
        print(f"[shutdown] Closing SSH to {vm.name} ({vm.ip})", flush=True)
        _close_ssh_safe(vm)
    print("[shutdown] SSH closed. Waiting for threads to exit, then destroying VMs...", flush=True)
    print("[shutdown] Press Ctrl-C again to force-destroy immediately.", flush=True)

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
    memory_mb: int = 0
    name: str = ""
    created_at: float = field(default_factory=time.monotonic)
    destroyed_at: float | None = None
    hw_info: dict = field(default_factory=dict)


@dataclass
class ResourceStats:
    """Resource usage stats for a time window or full VM lifetime."""
    avg_cpu_pct: float | None = None
    max_cpu_pct: float | None = None
    avg_mem_used_mb: float | None = None
    max_mem_used_mb: float | None = None
    mem_total_mb: float | None = None
    avg_load_1m: float | None = None
    max_load_1m: float | None = None
    samples: int = 0


@dataclass
class InvariantResult:
    invariant: str
    vm_name: str
    passed: bool
    elapsed_seconds: float
    output: str
    error: str = ""
    exit_code: int | None = None
    failure_category: str = ""  # violation, oom, timeout, infra, runtime_error, unknown
    resource_stats: ResourceStats | None = None


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

TOOLING_DIR = Path(__file__).parent.resolve()
SPECS_DIR = TOOLING_DIR.parent.resolve()
RUNNER_KEY = TOOLING_DIR / "quint-runner-key"


def get_ssh_key_path() -> Path:
    if RUNNER_KEY.exists():
        return RUNNER_KEY
    for name in ("id_ed25519", "id_rsa"):
        p = Path.home() / ".ssh" / name
        if p.exists():
            return p
    sys.exit("ERROR: No SSH key found. Generate one with:\n"
             "  ssh-keygen -t ed25519 -f specs/tooling/quint-runner-key -N '' -C quint-cloud-runner")


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
    log_fh=None,
    line_callback=None,
) -> tuple[int, str, str]:
    """Execute command over SSH. Returns (exit_code, stdout, stderr).

    If log_fh is set, output is streamed line-by-line and written to the file
    handle (regardless of the stream flag). The stream flag controls whether
    lines are also printed to the console.

    If line_callback is set, it is called with each stdout line (str).
    """
    chan = ssh.get_transport().open_session()
    chan.set_combine_stderr(False)
    chan.settimeout(timeout)
    chan.exec_command(cmd)

    stdout_chunks: list[str] = []
    stderr_chunks: list[str] = []

    stdout_stream = chan.makefile("rb", -1)
    stderr_stream = chan.makefile_stderr("rb", -1)

    if stream or log_fh or line_callback:
        for raw_line in stdout_stream:
            line = raw_line.decode("utf-8", errors="replace").rstrip("\n")
            stdout_chunks.append(line)
            if log_fh:
                log_fh.write(line + "\n")
                log_fh.flush()
            if line_callback:
                line_callback(line)
            if stream:
                if prefix:
                    print(f"  [{prefix}] {line}", flush=True)
                else:
                    print(f"  {line}", flush=True)
        for raw_line in stderr_stream:
            line = raw_line.decode("utf-8", errors="replace").rstrip("\n")
            stderr_chunks.append(line)
            if log_fh:
                log_fh.write(f"[stderr] {line}\n")
                log_fh.flush()
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
# UpCloud provider
# ---------------------------------------------------------------------------

class UpCloudProvider(CloudProvider):
    LOCATIONS = ["nl-ams1", "pl-waw1", "de-fra1"]
    # Ubuntu 24.04 template UUID
    UBUNTU_TEMPLATE = "01000000-0000-4000-8000-000030240200"

    def __init__(self) -> None:
        self._manager = None

    @property
    def name(self) -> str:
        return "upcloud"

    @property
    def default_vm_type(self) -> str:
        return "HICPU-8xCPU-12GB"

    @property
    def default_location(self) -> str:
        return "nl-ams1"

    def init(self) -> None:
        try:
            import upcloud_api  # noqa: F401
        except ImportError:
            sys.exit("ERROR: upcloud-api is required. Install with: pip install upcloud-api")

        from upcloud_api import CloudManager

        token = os.environ.get("UPCLOUD_TOKEN")
        username = os.environ.get("UPCLOUD_USERNAME")
        password = os.environ.get("UPCLOUD_PASSWORD")

        if token:
            self._manager = CloudManager(token=token)
            self._manager.authenticate()
            print("  Authenticated with UpCloud using API token")
        elif username and password:
            self._manager = CloudManager(username, password)
            self._manager.authenticate()
            print(f"  Authenticated with UpCloud as {username}")
        else:
            sys.exit("ERROR: Set UPCLOUD_TOKEN, or both UPCLOUD_USERNAME and UPCLOUD_PASSWORD")

    def create_vm(self, name: str, vm_type: str, location: str) -> VMInstance:
        from upcloud_api import Server, Storage, login_user_block

        pub_key = get_ssh_public_key()
        login_user = login_user_block(
            username="root",
            ssh_keys=[pub_key],
            create_password=False,
        )

        locations = [location] + [l for l in self.LOCATIONS if l != location]
        last_error = None

        for loc in locations:
            try:
                server = Server(
                    plan=vm_type,
                    hostname=f"{name}.quint-runner",
                    title=name,
                    zone=loc,
                    storage_devices=[
                        Storage(
                            os=self.UBUNTU_TEMPLATE,
                            size=25,
                        ),
                    ],
                    login_user=login_user,
                )
                server = self._manager.create_server(server)
                server.ensure_started()
                ip = server.get_public_ip()
                if not ip:
                    raise RuntimeError(f"VM {name} did not get a public IP in {loc}")
                print(f"  Created VM {name} ({vm_type}) at {ip} [{loc}]")
                return VMInstance(
                    provider_handle=server,
                    ip=ip,
                    name=name,
                )
            except Exception as e:
                last_error = e
                if "unavailable" in str(e).lower() or "capacity" in str(e).lower():
                    print(f"  [{name}] Zone {loc} unavailable, trying next...")
                    continue
                raise

        raise RuntimeError(f"Failed to create VM {name} in any zone: {last_error}")

    def destroy_vm(self, vm: VMInstance) -> None:
        try:
            vm.provider_handle.stop_and_destroy()
            print(f"  Destroyed VM {vm.name}")
        except Exception as e:
            print(f"  WARNING: Failed to destroy {vm.name}: {e}")

    # UpCloud hourly pricing (EUR) — https://upcloud.com/pricing
    _PRICING = {
        "HICPU-2xCPU-4GB":   0.06,
        "HICPU-4xCPU-8GB":   0.11,
        "HICPU-8xCPU-12GB":  0.22,
        "HICPU-12xCPU-16GB": 0.33,
        "HICPU-16xCPU-32GB": 0.44,
    }

    def price_per_hour(self, vm_type: str) -> float | None:
        return self._PRICING.get(vm_type)


# ---------------------------------------------------------------------------
# VM provisioning (provider-agnostic)
# ---------------------------------------------------------------------------

def provision_vm(vm: VMInstance, specs_dir: Path, mode: str = "run") -> None:
    """Install Docker, pull image, upload specs. In verify mode, also install Java and quint."""
    print(f"  [{vm.name}] Waiting for SSH...")
    wait_for_ssh(vm.ip)
    vm.ssh = connect_ssh(vm.ip)

    # Tighten TCP keepalive — helps keep long-running gRPC connections alive
    ssh_exec(vm.ssh, (
        "sysctl -w net.ipv4.tcp_keepalive_time=60 "
        "net.ipv4.tcp_keepalive_intvl=10 "
        "net.ipv4.tcp_keepalive_probes=6 "
        "> /dev/null 2>&1"
    ), timeout=10, check=False)

    print(f"  [{vm.name}] Installing Docker and pulling image...")
    install_script = textwrap.dedent(f"""\
        set -euo pipefail
        export DEBIAN_FRONTEND=noninteractive
        for attempt in 1 2 3; do
            apt-get update -qq && break
            echo "apt-get update failed (attempt $attempt), retrying in 10s..."
            sleep 10
        done
        if apt-cache show docker.io > /dev/null 2>&1; then
            apt-get install -y -qq docker.io > /dev/null 2>&1
        else
            apt-get install -y -qq ca-certificates curl > /dev/null 2>&1
            install -m 0755 -d /etc/apt/keyrings
            curl -fsSL https://download.docker.com/linux/ubuntu/gpg -o /etc/apt/keyrings/docker.asc
            echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.asc] \
https://download.docker.com/linux/ubuntu $(. /etc/os-release && echo "$VERSION_CODENAME") stable" \
> /etc/apt/sources.list.d/docker.list
            apt-get update -qq
            apt-get install -y -qq docker-ce docker-ce-cli > /dev/null 2>&1
        fi
        systemctl enable --now docker
        docker pull {DOCKER_IMAGE}
    """)
    ssh_exec(vm.ssh, install_script, timeout=600, prefix=vm.name)

    # Detect CPU count and collect hardware info
    _, cpu_out, _ = ssh_exec(vm.ssh, "nproc")
    vm.cpus = int(cpu_out.strip())

    hw_script = textwrap.dedent("""\
        echo "CPU_MODEL=$(grep -m1 'model name' /proc/cpuinfo | cut -d: -f2 | xargs)"
        echo "CPU_CORES=$(nproc)"
        echo "CPU_SOCKETS=$(grep -c 'physical id' /proc/cpuinfo 2>/dev/null | sort -u | wc -l || echo 1)"
        echo "CPU_MHZ=$(grep -m1 'cpu MHz' /proc/cpuinfo | cut -d: -f2 | xargs)"
        echo "MEM_TOTAL=$(awk '/MemTotal/{printf "%.1f GB", $2/1024/1024}' /proc/meminfo)"
        echo "MEM_MB=$(awk '/MemTotal/{printf "%d", $2/1024}' /proc/meminfo)"
        echo "ARCH=$(uname -m)"
        echo "KERNEL=$(uname -r)"
    """)
    _, hw_out, _ = ssh_exec(vm.ssh, hw_script)
    vm.hw_info = {}
    for line in hw_out.strip().splitlines():
        if "=" in line:
            key, val = line.split("=", 1)
            vm.hw_info[key] = val
    if "MEM_MB" in vm.hw_info:
        vm.memory_mb = int(vm.hw_info["MEM_MB"])
    print(f"  [{vm.name}] Docker ready — {vm.cpus} CPUs")

    # Upload spec files
    print(f"  [{vm.name}] Uploading spec files...")
    qnt_files = sorted(specs_dir.rglob("*.qnt"))
    remote_dirs = {Path("/root/specs")}
    for qnt_file in qnt_files:
        rel = qnt_file.relative_to(specs_dir)
        remote_dirs.add(Path("/root/specs") / rel.parent)
    mkdir_cmd = "mkdir -p " + " ".join(sorted(str(p) for p in remote_dirs))
    ssh_exec(vm.ssh, mkdir_cmd)
    for qnt_file in qnt_files:
        rel = qnt_file.relative_to(specs_dir)
        scp_upload(vm.ssh, qnt_file, f"/root/specs/{rel.as_posix()}")
    # Start resource monitor
    try:
        ssh_exec(vm.ssh,
                 f"cat > /root/monitor.sh << 'MONEOF'\n{MONITOR_SCRIPT}\nMONEOF\n"
                 f"chmod +x /root/monitor.sh",
                 timeout=10, check=True)
        ssh_exec(vm.ssh, "nohup /root/monitor.sh > /dev/null 2>&1 &",
                 timeout=10, check=False)
        print(f"  [{vm.name}] Resource monitor started")
    except Exception as e:
        print(f"  [{vm.name}] WARNING: could not start resource monitor: {e}")

    print(f"  [{vm.name}] Ready")


def _close_ssh_safe(vm: VMInstance) -> None:
    """Close SSH connection with a timeout to avoid hanging."""
    if not vm.ssh:
        return
    try:
        transport = vm.ssh.get_transport()
        if transport and transport.is_active():
            transport.close()
        vm.ssh.close()
    except Exception:
        pass
    vm.ssh = None


# ---------------------------------------------------------------------------
# Resource stats helpers
# ---------------------------------------------------------------------------

_AWK_BODY = "n++;ct+=$2;if($2>mc)mc=$2;mt+=$3;if($3>mm)mm=$3;tt=$4;lt+=$5;if($5>ml)ml=$5"
_AWK_END = (
    'END{if(n>0)printf "%d,%.1f,%.1f,%.1f,%.1f,%.1f,%.1f,%.1f\\n",'
    'n,ct/n,mc,mt/n,mm,tt,lt/n,ml;else print "0"}'
)


def _parse_resource_stats(raw: str) -> ResourceStats | None:
    raw = raw.strip()
    if not raw or raw == "0":
        return None
    parts = raw.split(",")
    if len(parts) < 8:
        return None
    return ResourceStats(
        samples=int(float(parts[0])),
        avg_cpu_pct=float(parts[1]),
        max_cpu_pct=float(parts[2]),
        avg_mem_used_mb=float(parts[3]),
        max_mem_used_mb=float(parts[4]),
        mem_total_mb=float(parts[5]),
        avg_load_1m=float(parts[6]),
        max_load_1m=float(parts[7]),
    )


def fetch_resource_stats_window(
    vm: VMInstance, start_ts: int, end_ts: int,
) -> ResourceStats | None:
    """Fetch resource stats from monitor.csv for a Unix timestamp window."""
    try:
        cmd = (
            f"awk -F, 'NR>1 && $1>={start_ts} && $1<={end_ts}"
            f"{{{_AWK_BODY}}}{_AWK_END}' /root/monitor.csv"
        )
        _, out, _ = ssh_exec(vm.ssh, cmd, timeout=10, check=False)
        return _parse_resource_stats(out)
    except Exception:
        return None


def fetch_resource_stats_full(vm: VMInstance) -> ResourceStats | None:
    """Fetch resource stats from the entire monitor.csv (full VM lifetime)."""
    try:
        cmd = f"awk -F, 'NR>1{{{_AWK_BODY}}}{_AWK_END}' /root/monitor.csv"
        _, out, _ = ssh_exec(vm.ssh, cmd, timeout=10, check=False)
        return _parse_resource_stats(out)
    except Exception:
        return None


def download_monitor_csv(vm: VMInstance, output_dir: Path) -> Path | None:
    """Download /root/monitor.csv from the VM."""
    try:
        local_path = output_dir / f"{vm.name}_monitor.csv"
        sftp = vm.ssh.open_sftp()
        try:
            sftp.get("/root/monitor.csv", str(local_path))
        finally:
            sftp.close()
        return local_path
    except Exception:
        return None


def format_resource_stats(stats: ResourceStats | None, prefix: str = "#") -> str:
    """Format resource stats as comment lines for log files."""
    if stats is None:
        return f"{prefix} resource_stats: unavailable\n"
    mem_pct = (100.0 * stats.max_mem_used_mb / stats.mem_total_mb
               if stats.mem_total_mb else 0)
    return (
        f"{prefix} cpu_avg_pct: {stats.avg_cpu_pct:.1f}\n"
        f"{prefix} cpu_max_pct: {stats.max_cpu_pct:.1f}\n"
        f"{prefix} mem_avg_mb: {stats.avg_mem_used_mb:.0f}\n"
        f"{prefix} mem_max_mb: {stats.max_mem_used_mb:.0f}\n"
        f"{prefix} mem_total_mb: {stats.mem_total_mb:.0f}\n"
        f"{prefix} mem_max_pct: {mem_pct:.1f}\n"
        f"{prefix} load_avg_1m: {stats.avg_load_1m:.2f}\n"
        f"{prefix} load_max_1m: {stats.max_load_1m:.2f}\n"
        f"{prefix} monitor_samples: {stats.samples}\n"
    )


def destroy_vms(provider: CloudProvider, vms: list[VMInstance]) -> None:
    for vm in vms:
        _close_ssh_safe(vm)
        try:
            provider.destroy_vm(vm)
        except Exception as e:
            print(f"  WARNING: Failed to destroy {vm.name}: {e}", flush=True)
        vm.destroyed_at = time.monotonic()


# ---------------------------------------------------------------------------
# Failure classification
# ---------------------------------------------------------------------------

def extract_apalache_phase(stdout: str) -> str | None:
    """Extract the last Apalache phase reached from stdout."""
    last_pass = None
    last_state = None
    for line in stdout.splitlines():
        stripped = line.strip()
        # Match "PASS #13: BoundedChecker" or "State 1: Checking 1 state invariants"
        if stripped.startswith("PASS #"):
            # e.g. "PASS #13: BoundedChecker  I@10:36:02.762"
            part = stripped.split("I@")[0].strip() if "I@" in stripped else stripped
            last_pass = part
        elif "state " in stripped.lower() and ("checking" in stripped.lower() or "picking" in stripped.lower()):
            last_state = stripped.split("I@")[0].strip() if "I@" in stripped else stripped
    if last_state:
        return last_state
    return last_pass


def classify_failure(exit_code: int | None, stdout: str, stderr: str) -> str:
    """Classify a failed invariant check into a category."""
    combined = (stdout + stderr).lower()

    if "invariant violated" in combined or "[ok] no violation found" in combined:
        return "violation"

    if "ssh timeout" in combined:
        return "timeout"

    if "ran out of heap memory" in combined or "java heap space" in combined:
        return "oom"

    if "address already in use" in combined:
        return "infra_port_conflict"

    if "rst_stream" in combined or "grpc" in combined:
        return "infra_grpc"

    if "runtime error" in combined or "error [qnt" in combined:
        return "runtime_error"

    # Truncated output — likely OOM-killed by the kernel
    if exit_code is not None and exit_code != 0:
        if "state 0: checking" in combined or "state 2: checking" in combined:
            last_line = stdout.strip().splitlines()[-1] if stdout.strip() else ""
            if "checking" in last_line.lower() or "picking" in last_line.lower():
                return "oom_killed"

    if exit_code == 137:
        return "oom_killed"

    return "unknown"


FAILURE_LABELS = {
    "violation": "Invariant violations",
    "timeout": "SSH timeouts",
    "oom": "JVM out-of-memory",
    "oom_killed": "Likely OOM-killed (truncated output)",
    "infra_port_conflict": "Apalache port conflicts",
    "infra_grpc": "gRPC transport errors",
    "infra_ssh": "SSH connection errors",
    "runtime_error": "Quint runtime errors",
    "shutdown": "Cancelled (shutdown requested)",
    "unknown": "Unknown failures",
}

# Categories that are worth retrying
RETRYABLE_CATEGORIES = {"infra_port_conflict", "infra_grpc"}


# ---------------------------------------------------------------------------
# Invariant execution
# ---------------------------------------------------------------------------

def _docker_resource_flags(vm: VMInstance) -> str:
    """Docker flags to reserve ~5% of CPU and memory for OS/sshd."""
    flags = []
    if vm.cpus > 1:
        # Reserve 0.5 CPU for OS (at least 1 core for the container)
        limit = max(1.0, vm.cpus - 0.5)
        flags.append(f"--cpus={limit:.1f}")
    if vm.memory_mb > 0:
        # Reserve 5% of RAM (at least 512MB) for OS/sshd
        reserved = max(512, int(vm.memory_mb * 0.05))
        container_mb = vm.memory_mb - reserved
        flags.append(f"--memory={container_mb}m")
    return " ".join(flags)


def _build_verify_cmd(
    invariant_name: str,
    effective_steps: int,
    heap_arg: str,
    vm: VMInstance | None = None,
    spec: str = "doltswarm_verify.qnt",
    init_action: str | None = None,
    step_action: str | None = None,
    inductive: bool = False,
) -> str:
    """Build a quint verify command that runs inside Docker.

    JVM flags (passed via -e JVM_ARGS):
      -Xmx{heap}               — max heap sized to 65% of container RAM
      -XX:+UseG1GC             — low-pause garbage collector
      -XX:MaxGCPauseMillis=2000 — keep GC pauses short to avoid gRPC timeouts
      -XX:+CrashOnOutOfMemoryError — crash immediately on OOM instead of
                                     thrashing for minutes then dropping gRPC
    """
    import random
    port = random.randint(10000, 60000)
    jvm_flags = (
        f"-Xmx{heap_arg} "
        f"-XX:+UseG1GC "
        f"-XX:MaxGCPauseMillis=2000 "
        f"-XX:+CrashOnOutOfMemoryError"
    )
    res_flags = _docker_resource_flags(vm) if vm else ""
    inv_flag = "--inductive-invariant" if inductive else "--invariant"
    parts = [
        f"docker run --rm {res_flags}",
        f"-v /root/specs:/specs",
        f"-e JVM_ARGS='{jvm_flags}'",
        DOCKER_IMAGE,
        f"quint verify /specs/{spec}",
        f"{inv_flag}={invariant_name}",
        f"--max-steps={effective_steps}",
        f"--server-endpoint=localhost:{port}",
        f"--verbosity=5",
    ]
    if init_action:
        parts.append(f"--init={init_action}")
    if step_action:
        parts.append(f"--step={step_action}")
    return " ".join(parts)


def _collect_failure_diagnostics(vm: VMInstance, invariant_name: str) -> str:
    """After a gRPC/crash failure, collect diagnostics from the VM."""
    diag_lines: list[str] = []
    try:
        # Check if java/apalache is still running
        _, ps_out, _ = ssh_exec(vm.ssh, "pgrep -af 'java|apalache' || echo 'no java process'",
                                timeout=5, check=False)
        diag_lines.append(f"[diag] java processes: {ps_out.strip()}")

        # Check dmesg for OOM killer
        _, dmesg_out, _ = ssh_exec(vm.ssh, "dmesg | grep -i -E 'oom|killed|out of memory' | tail -5",
                                   timeout=5, check=False)
        if dmesg_out.strip():
            diag_lines.append(f"[diag] dmesg OOM: {dmesg_out.strip()}")

        # Check Apalache output directory for crash logs
        _, apa_out, _ = ssh_exec(vm.ssh,
                                 "ls -t /root/_apalache-out/server/*/detailed.log 2>/dev/null | head -1 "
                                 "| xargs tail -20 2>/dev/null || echo 'no apalache log'",
                                 timeout=10, check=False)
        if apa_out.strip() and apa_out.strip() != "no apalache log":
            diag_lines.append(f"[diag] apalache log tail:\n{apa_out.strip()}")

        # Check available memory at time of failure
        _, mem_out, _ = ssh_exec(vm.ssh, "free -m | head -2", timeout=5, check=False)
        diag_lines.append(f"[diag] memory: {mem_out.strip()}")
    except Exception:
        diag_lines.append("[diag] could not collect diagnostics (SSH failed)")
    return "\n".join(diag_lines)


def run_invariant_on_vm(
    vm: VMInstance,
    invariant: Invariant,
    samples: int,
    steps: int | None,
    fallback_steps: int,
    mode: str = "run",
    jvm_memory: str = "8g",
    verify_heap_ratio: float = 0.65,
    max_retries: int = 2,
    output_dir: Path | None = None,
    spec: str = "doltswarm_verify.qnt",
    init_action: str | None = None,
    step_action: str | None = None,
    inductive: bool = False,
) -> InvariantResult:
    effective_steps = steps if steps is not None else (invariant.default_steps or fallback_steps)

    # Compute JVM heap for verify mode.
    # Docker container gets 95% of VM RAM.  JVM heap must fit within that,
    # leaving room for Z3 native memory (~20% of heap) + JVM metaspace/stacks.
    # Formula: container_mb * verify_heap_ratio.
    if mode == "verify":
        if vm.memory_mb > 0:
            reserved_os = max(512, int(vm.memory_mb * 0.05))
            container_mb = vm.memory_mb - reserved_os
            heap_mb = int(container_mb * verify_heap_ratio)
            heap_arg = f"{heap_mb}m"
        else:
            heap_arg = jvm_memory

    # Timeout: 4 hours for verify (SMT solving can be very slow), 2 hours for run
    ssh_timeout = 14400 if mode == "verify" else 7200

    attempts = 0
    while True:
        if _shutdown_requested.is_set():
            return InvariantResult(
                invariant=invariant.name,
                vm_name=vm.name,
                passed=False,
                elapsed_seconds=0,
                output="",
                error="Shutdown requested",
                failure_category="shutdown",
            )

        attempts += 1

        res_flags = _docker_resource_flags(vm)
        if mode == "verify":
            cmd = _build_verify_cmd(invariant.name, effective_steps, heap_arg,
                                    vm=vm, spec=spec, init_action=init_action,
                                    step_action=step_action, inductive=inductive)
        else:
            cmd = (
                f"docker run --rm {res_flags} "
                f"-v /root/specs:/specs {DOCKER_IMAGE} "
                f"quint run /specs/doltswarm_verify.qnt "
                f"--backend=rust "
                f"--invariant={invariant.name} "
                f"--max-samples={samples} "
                f"--max-steps={effective_steps}"
            )

        # Record VM timestamp before the run for resource stats
        vm_start_ts = None
        try:
            _, ts_out, _ = ssh_exec(vm.ssh, "date +%s", timeout=5, check=False)
            vm_start_ts = int(ts_out.strip())
        except Exception:
            pass

        # Progressive log: write a .running.log with streaming output
        running_log_path = None
        log_fh = None
        if output_dir:
            running_log_path = output_dir / f"{invariant.name}.running.log"
            try:
                running_log_path.parent.mkdir(parents=True, exist_ok=True)
                log_fh = open(running_log_path, "w")
                log_fh.write(f"# invariant: {invariant.name}\n")
                log_fh.write(f"# status: RUNNING (attempt {attempts})\n")
                log_fh.write(f"# vm: {vm.name}\n")
                log_fh.write(f"# started: {time.strftime('%Y-%m-%d %H:%M:%S')}\n")
                log_fh.write(f"# mode: {mode}\n\n")
                log_fh.flush()
            except Exception:
                log_fh = None

        # Progress callback: print condensed Apalache milestones only
        progress_cb = None
        if mode == "verify":
            _run_start = time.monotonic()
            def _apalache_progress(line: str) -> None:
                stripped = line.strip()
                elapsed_m = (time.monotonic() - _run_start) / 60
                info = stripped.split("I@")[0].strip() if "I@" in stripped else stripped
                # BoundedChecker start (the main solving phase)
                if "PASS #13: BoundedChecker" in stripped or "PASS #12:" in stripped:
                    print(f"  [{vm.name}] {invariant.name}: {info} ({elapsed_m:.1f}m)", flush=True)
                # Step transitions (the key progress indicator during solving)
                elif stripped.lower().startswith("step ") and "picking" in stripped.lower():
                    print(f"  [{vm.name}] {invariant.name}: {info} ({elapsed_m:.1f}m)", flush=True)
                # State checking result (only the final state per step matters)
                elif stripped.lower().startswith("state ") and "checking" in stripped.lower():
                    print(f"  [{vm.name}] {invariant.name}: {info} ({elapsed_m:.1f}m)", flush=True)
                # Final verdict
                elif "no violation found" in stripped.lower() or "invariant violated" in stripped.lower():
                    print(f"  [{vm.name}] {invariant.name}: {stripped} ({elapsed_m:.1f}m)", flush=True)
            progress_cb = _apalache_progress

        started = time.monotonic()
        try:
            exit_code, stdout, stderr = ssh_exec(
                vm.ssh, cmd, timeout=ssh_timeout, check=False,
                log_fh=log_fh, line_callback=progress_cb,
            )
        except (TimeoutError, socket.timeout, paramiko.buffered_pipe.PipeTimeout) as e:
            elapsed = time.monotonic() - started
            if log_fh:
                log_fh.write(f"\n[TIMEOUT after {elapsed:.0f}s: {e}]\n")
                log_fh.close()
            return InvariantResult(
                invariant=invariant.name,
                vm_name=vm.name,
                passed=False,
                elapsed_seconds=elapsed,
                output="",
                error=f"SSH timeout after {elapsed:.0f}s: {e}",
                exit_code=None,
                failure_category="timeout",
            )
        except (OSError, paramiko.SSHException, EOFError) as e:
            elapsed = time.monotonic() - started
            category = "shutdown" if _shutdown_requested.is_set() else "infra_ssh"
            if log_fh:
                log_fh.write(f"\n[SSH ERROR: {e}]\n")
                log_fh.close()
            return InvariantResult(
                invariant=invariant.name,
                vm_name=vm.name,
                passed=False,
                elapsed_seconds=elapsed,
                output="",
                error=f"SSH error: {e}",
                exit_code=None,
                failure_category=category,
            )
        finally:
            if log_fh and not log_fh.closed:
                log_fh.close()

        elapsed = time.monotonic() - started

        # Clean up .running.log (final log written by on_result callback)
        if running_log_path and running_log_path.exists():
            try:
                running_log_path.unlink()
            except Exception:
                pass

        # Fetch resource stats for this invariant's time window
        resource_stats = None
        if vm_start_ts is not None:
            try:
                _, ts_out, _ = ssh_exec(vm.ssh, "date +%s", timeout=5, check=False)
                vm_end_ts = int(ts_out.strip())
                resource_stats = fetch_resource_stats_window(vm, vm_start_ts, vm_end_ts)
            except Exception:
                pass

        if exit_code == 0:
            return InvariantResult(
                invariant=invariant.name,
                vm_name=vm.name,
                passed=True,
                elapsed_seconds=elapsed,
                output=stdout,
                exit_code=exit_code,
                resource_stats=resource_stats,
            )

        category = classify_failure(exit_code, stdout, stderr)

        # Collect diagnostics for gRPC/OOM failures (skip during shutdown)
        oom_detected = False
        if category in ("infra_grpc", "oom", "oom_killed") and not _shutdown_requested.is_set():
            diag = _collect_failure_diagnostics(vm, invariant.name)
            if diag:
                oom_detected = "oom" in diag.lower() or "killed process" in diag.lower()
                stderr = stderr + "\n\n" + diag if stderr else diag

        if category in RETRYABLE_CATEGORIES and attempts <= max_retries and not _shutdown_requested.is_set():
            reason = "OOM killed by Docker cgroup" if oom_detected else FAILURE_LABELS.get(category, category)
            last_phase = extract_apalache_phase(stdout) or "unknown phase"
            print(
                f"  [{vm.name}] {invariant.name}: CRASHED ({reason}) "
                f"at '{last_phase}' after {elapsed:.0f}s — "
                f"restarting (attempt {attempts + 1}/{max_retries + 1})",
                flush=True,
            )
            time.sleep(2)
            continue

        return InvariantResult(
            invariant=invariant.name,
            vm_name=vm.name,
            passed=False,
            elapsed_seconds=elapsed,
            output=stdout,
            error=stderr,
            exit_code=exit_code,
            failure_category=category,
            resource_stats=resource_stats,
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
    mode: str = "run",
    jvm_memory: str = "8g",
    verify_heap_ratio: float = 0.65,
    on_result: callable = None,
    output_dir: Path | None = None,
    spec: str = "doltswarm_verify.qnt",
    init_action: str | None = None,
    step_action: str | None = None,
    inductive: bool = False,
) -> list[InvariantResult]:
    """Run a batch of invariants on a VM, parallelising up to vm.cpus containers."""
    results: list[InvariantResult] = []
    # verify is memory-heavy — run only 1 at a time per VM
    concurrency = 1 if mode == "verify" else max(1, vm.cpus)

    def run_one(inv: Invariant) -> InvariantResult:
        try:
            result = run_invariant_on_vm(vm, inv, samples, steps, fallback_steps,
                                         mode=mode, jvm_memory=jvm_memory,
                                         verify_heap_ratio=verify_heap_ratio,
                                         output_dir=output_dir, spec=spec,
                                         init_action=init_action,
                                         step_action=step_action,
                                         inductive=inductive)
        except Exception as e:
            result = InvariantResult(
                invariant=inv.name,
                vm_name=vm.name,
                passed=False,
                elapsed_seconds=0,
                output="",
                error=f"Uncaught exception: {type(e).__name__}: {e}",
                failure_category="infra_ssh",
            )
        done, total = progress.record(result.passed)
        status = "PASS" if result.passed else "FAIL"
        print(
            f"  [{done}/{total}] {status} {inv.name} "
            f"on {vm.name} ({result.elapsed_seconds:.1f}s)",
            flush=True,
        )
        if on_result:
            on_result(result)
        return result

    with ThreadPoolExecutor(max_workers=concurrency) as pool:
        futures = {pool.submit(run_one, inv): inv for inv in invariants}
        for future in as_completed(futures):
            try:
                results.append(future.result())
            except Exception as e:
                inv = futures[future]
                result = InvariantResult(
                    invariant=inv.name,
                    vm_name=vm.name,
                    passed=False,
                    elapsed_seconds=0,
                    output="",
                    error=f"Future exception: {type(e).__name__}: {e}",
                    failure_category="infra_ssh",
                )
                results.append(result)

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
        # Group by category
        by_cat: dict[str, list[InvariantResult]] = {}
        for r in failed:
            by_cat.setdefault(r.failure_category or "unknown", []).append(r)

        print("FAILURE BREAKDOWN:")
        for cat, label in FAILURE_LABELS.items():
            if cat in by_cat:
                print(f"  {label}: {len(by_cat[cat])}")
        print()

        # Show details for violations (most important)
        if "violation" in by_cat:
            print("INVARIANT VIOLATIONS:")
            for r in sorted(by_cat["violation"], key=lambda x: x.invariant):
                print(f"  FAIL  {r.invariant}  (on {r.vm_name}, {r.elapsed_seconds:.1f}s)")
                if r.output:
                    for line in r.output.strip().splitlines()[-10:]:
                        print(f"        {line}")
            print()

    print("ALL RESULTS:")
    for r in sorted(results, key=lambda x: x.invariant):
        status = "PASS" if r.passed else "FAIL"
        cat_suffix = f"  [{r.failure_category}]" if not r.passed and r.failure_category else ""
        print(f"  {status}  {r.invariant:<60s} {r.elapsed_seconds:7.1f}s  {r.vm_name}{cat_suffix}")


# ---------------------------------------------------------------------------
# Log saving
# ---------------------------------------------------------------------------

def save_invariant_log(
    result: InvariantResult,
    output_dir: Path,
    args: argparse.Namespace,
) -> None:
    """Write a single invariant result log file. Safe to call as each result arrives."""
    output_dir.mkdir(parents=True, exist_ok=True)
    status = "pass" if result.passed else "fail"
    log_path = output_dir / f"{result.invariant}.{status}.log"
    with open(log_path, "w") as f:
        f.write(f"# invariant: {result.invariant}\n")
        f.write(f"# status: {'PASS' if result.passed else 'FAIL'}\n")
        f.write(f"# vm: {result.vm_name}\n")
        f.write(f"# exit_code: {result.exit_code}\n")
        f.write(f"# elapsed: {result.elapsed_seconds:.2f}s\n")
        f.write(f"# samples: {args.samples}\n")
        f.write(f"# steps: {args.steps or 'per-annotation'}\n")
        f.write(f"# mode: {args.mode}\n")
        if not result.passed and result.failure_category:
            f.write(f"# failure_category: {result.failure_category}\n")
        apalache_phase = extract_apalache_phase(result.output)
        if apalache_phase:
            f.write(f"# last_apalache_phase: {apalache_phase}\n")
        f.write(format_resource_stats(result.resource_stats))
        f.write("\n--- stdout ---\n")
        f.write(result.output)
        if result.error:
            f.write("\n\n--- stderr ---\n")
            f.write(result.error)
        f.write("\n")


def save_logs(
    results: list[InvariantResult],
    output_dir: Path,
    wall_seconds: float,
    args: argparse.Namespace,
    vms: list[VMInstance] | None = None,
    provider: CloudProvider | None = None,
) -> None:
    """Save summary file. Individual logs are already written progressively."""
    output_dir.mkdir(parents=True, exist_ok=True)

    sorted_results = sorted(results, key=lambda r: r.invariant)
    passed = [r for r in results if r.passed]
    failed = [r for r in results if not r.passed]
    times = [r.elapsed_seconds for r in results]

    # Categorise failures by category
    failures_by_category: dict[str, list[InvariantResult]] = {}
    for r in failed:
        cat = r.failure_category or "unknown"
        failures_by_category.setdefault(cat, []).append(r)

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

        # Failure breakdown by category
        if failed:
            f.write(f"{'=' * 70}\n")
            f.write(f"FAILURE BREAKDOWN\n")
            f.write(f"{'=' * 70}\n\n")

            # Show category summary first
            for cat, label in FAILURE_LABELS.items():
                if cat in failures_by_category:
                    f.write(f"  {label}: {len(failures_by_category[cat])}\n")
            f.write("\n")

            # Then detailed list per category
            # Show violations first (most important), then infra, then rest
            category_order = [
                "violation", "oom", "oom_killed", "timeout",
                "infra_port_conflict", "infra_grpc", "infra_ssh",
                "runtime_error", "shutdown", "unknown",
            ]
            for cat in category_order:
                if cat not in failures_by_category:
                    continue
                results_in_cat = failures_by_category[cat]
                label = FAILURE_LABELS.get(cat, cat)
                f.write(f"{label} ({len(results_in_cat)}):\n")
                for r in sorted(results_in_cat, key=lambda x: x.invariant):
                    exit_str = f"exit={r.exit_code}" if r.exit_code is not None else "no exit"
                    phase = extract_apalache_phase(r.output)
                    phase_str = f", last phase: {phase}" if phase else ""
                    res_str = ""
                    if r.resource_stats:
                        s = r.resource_stats
                        res_str = f", CPU avg={s.avg_cpu_pct:.0f}% MEM max={s.max_mem_used_mb:.0f}MB"
                    f.write(f"  FAIL  {r.invariant}  ({r.elapsed_seconds:.1f}s, {r.vm_name}, {exit_str}{phase_str}{res_str})\n")
                f.write("\n")

        # Resource usage summary
        results_with_stats = [r for r in results if r.resource_stats is not None]
        if results_with_stats:
            f.write(f"{'=' * 70}\n")
            f.write(f"RESOURCE USAGE\n")
            f.write(f"{'=' * 70}\n\n")
            for r in sorted(results_with_stats, key=lambda x: x.invariant):
                s = r.resource_stats
                status = "PASS" if r.passed else "FAIL"
                mem_pct = (100.0 * s.max_mem_used_mb / s.mem_total_mb
                           if s.mem_total_mb else 0)
                f.write(
                    f"  {status}  {r.invariant:<50s}  "
                    f"CPU avg={s.avg_cpu_pct:5.1f}% max={s.max_cpu_pct:5.1f}%  "
                    f"MEM max={s.max_mem_used_mb:6.0f}MB/{s.mem_total_mb:.0f}MB ({mem_pct:.0f}%)  "
                    f"load={s.avg_load_1m:.1f}\n"
                )
            f.write("\n")

        # Full results table
        f.write(f"{'=' * 70}\n")
        f.write(f"ALL RESULTS\n")
        f.write(f"{'=' * 70}\n\n")
        for r in sorted_results:
            status = "PASS" if r.passed else "FAIL"
            extra = ""
            if not r.passed:
                parts = []
                if r.exit_code is not None:
                    parts.append(f"exit={r.exit_code}")
                if r.failure_category:
                    parts.append(r.failure_category)
                if parts:
                    extra = f"  [{', '.join(parts)}]"
            f.write(f"  {status}  {r.invariant:<60s} {r.elapsed_seconds:7.1f}s  {r.vm_name}{extra}\n")

    print(f"\nLogs saved to {output_dir}/")
    print(f"  summary.txt            — stats, failure breakdown, full results")
    print(f"  <invariant>.pass.log   — full stdout for passing checks")
    print(f"  <invariant>.fail.log   — full stdout+stderr for failures")
    print(f"  <vm>_monitor.csv       — raw per-second resource data")


# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------

PROVIDERS = {
    "scaleway": ScalewayProvider,
    "hetzner": HetznerProvider,
    "upcloud": UpCloudProvider,
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
        "--samples", type=int, default=10000,
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
        "--invariant", action="append", dest="invariants",
        help="Run only these invariants (can be repeated: --invariant inv_a --invariant inv_b)",
    )
    parser.add_argument(
        "--mode", choices=["run", "verify"], default="run",
        help="Quint mode: 'run' for random simulation (default), 'verify' for Apalache model checking",
    )
    parser.add_argument(
        "--init", default=None,
        help="Override the init action for verify mode (default: spec's init)",
    )
    parser.add_argument(
        "--step", default=None,
        help="Override the step action for verify mode (default: spec's step)",
    )
    parser.add_argument(
        "--inductive", action="store_true",
        help="Use --inductive-invariant instead of --invariant for verify mode",
    )
    parser.add_argument(
        "--jvm-memory", default="8g",
        help="Max JVM heap for Apalache in verify mode (default: 8g)",
    )
    parser.add_argument(
        "--verify-heap-ratio", type=float, default=0.65,
        help="Fraction of Docker RAM to give the JVM heap in verify mode when VM RAM is known (default: 0.65)",
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
    if args.invariants:
        wanted = set(args.invariants)
        matched = [inv for inv in invariants if inv.name in wanted]
        missing = wanted - {inv.name for inv in matched}
        if missing:
            sys.exit(f"ERROR: Unknown invariant(s): {', '.join(sorted(missing))}")
        invariants = matched

    print(f"Found {len(invariants)} invariant(s) to check")
    print(f"Provider: {provider.name}")
    print(f"Mode: {args.mode}")
    if not (0.0 < args.verify_heap_ratio < 1.0):
        sys.exit("ERROR: --verify-heap-ratio must be between 0 and 1")
    if args.mode == "verify":
        print(f"Config: {args.vms} VM(s) x {args.vm_type}, "
              f"steps={args.steps or 'per-annotation'}, "
              f"JVM heap={args.verify_heap_ratio * 100:.0f}% of container RAM")
    else:
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
    run_output_dir = specs_dir / args.output_dir / f"{args.mode}-{run_timestamp}"
    run_output_dir.mkdir(parents=True, exist_ok=True)
    print(f"Output directory: {run_output_dir}")
    print()

    vms: list[VMInstance] = []
    _all_vms.clear()       # share with signal handler
    _provider_ref.clear()
    _provider_ref.append(provider)
    destroyed_vms: set[str] = set()
    summary_written = False
    wall_start = time.monotonic()

    # Install signal handlers for graceful shutdown
    original_sigint = signal.signal(signal.SIGINT, _shutdown_handler)
    original_sigterm = signal.signal(signal.SIGTERM, _shutdown_handler)

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
                    vm = f.result()
                    vms.append(vm)
                    _all_vms.append(vm)
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
            list(pool.map(lambda vm: provision_vm(vm, specs_dir, mode=args.mode), vms))
        print()

        # Print VM hardware summary
        print("VM Hardware:")
        for vm in vms:
            hw = vm.hw_info
            cpu_model = hw.get("CPU_MODEL", "unknown")
            cpu_mhz = hw.get("CPU_MHZ", "")
            mem = hw.get("MEM_TOTAL", "unknown")
            arch = hw.get("ARCH", "unknown")
            kernel = hw.get("KERNEL", "unknown")
            freq = f" @ {float(cpu_mhz):.0f} MHz" if cpu_mhz else ""
            print(f"  {vm.name} ({vm.ip}):")
            print(f"    CPU:    {vm.cpus}x {cpu_model}{freq}")
            print(f"    Memory: {mem}")
            print(f"    Arch:   {arch}, kernel {kernel}")
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
        _results_lock = threading.Lock()

        def _on_result(result: InvariantResult) -> None:
            """Called from worker threads as each invariant finishes."""
            with _results_lock:
                all_results.append(result)
            try:
                save_invariant_log(result, run_output_dir, args)
            except Exception as e:
                print(f"  WARNING: failed to write log for {result.invariant}: {e}", flush=True)

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
                    mode=args.mode,
                    jvm_memory=args.jvm_memory,
                    verify_heap_ratio=args.verify_heap_ratio,
                    on_result=_on_result,
                    output_dir=run_output_dir,
                    spec=args.spec,
                    init_action=args.init,
                    step_action=args.step,
                    inductive=args.inductive,
                ): vm
                for vm in vms
                if assignment[vm.name]
            }
            for f in as_completed(futures):
                try:
                    f.result()  # results already collected via _on_result
                except Exception as e:
                    print(f"  WARNING: VM batch raised: {type(e).__name__}: {e}", flush=True)
                vm = futures[f]
                if not args.keep_vms:
                    if not _shutdown_requested.is_set():
                        # Normal completion: fetch stats and download CSV
                        vm_stats = fetch_resource_stats_full(vm)
                        if vm_stats:
                            mem_pct = (100.0 * vm_stats.max_mem_used_mb / vm_stats.mem_total_mb
                                       if vm_stats.mem_total_mb else 0)
                            print(
                                f"  [{vm.name}] Resources: "
                                f"CPU avg={vm_stats.avg_cpu_pct:.0f}% max={vm_stats.max_cpu_pct:.0f}%, "
                                f"MEM max={vm_stats.max_mem_used_mb:.0f}/{vm_stats.mem_total_mb:.0f}MB ({mem_pct:.0f}%), "
                                f"load avg={vm_stats.avg_load_1m:.1f}",
                                flush=True,
                            )
                        csv_path = download_monitor_csv(vm, run_output_dir)
                        if csv_path:
                            print(f"  [{vm.name}] Monitor CSV saved: {csv_path.name}", flush=True)
                    print(f"  [{vm.name}] All invariants done — destroying VM")
                    provider.destroy_vm(vm)
                    vm.destroyed_at = time.monotonic()
                    destroyed_vms.add(vm.name)

        wall_elapsed = time.monotonic() - wall_start

        print_report(all_results, wall_elapsed, vms, provider, args.vm_type)

        # Save summary (individual logs already written progressively)
        save_logs(all_results, run_output_dir, wall_elapsed, args, vms, provider)
        summary_written = True

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
                        "exit_code": r.exit_code,
                        "failure_category": r.failure_category or None,
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

    except KeyboardInterrupt:
        # In case KeyboardInterrupt sneaks through before signal handler is set
        _shutdown_requested.set()
        print("\n[shutdown] KeyboardInterrupt — will destroy VMs in cleanup phase", flush=True)

    finally:
        # Restore original signal handlers during cleanup
        signal.signal(signal.SIGINT, original_sigint)
        signal.signal(signal.SIGTERM, original_sigterm)

        # Write summary if not already written (e.g. crash or Ctrl-C)
        if all_results and not summary_written:
            try:
                wall_elapsed = time.monotonic() - wall_start
                save_logs(all_results, run_output_dir, wall_elapsed, args, vms, provider)
                print(f"\nSummary written to {run_output_dir}/summary.txt "
                      f"({len(all_results)} result(s))", flush=True)
            except Exception as e:
                print(f"\nWARNING: failed to write summary: {e}", flush=True)

        remaining_vms = [vm for vm in vms if vm.name not in destroyed_vms]
        if not args.keep_vms:
            if remaining_vms:
                names = [f"{vm.name} ({vm.ip})" for vm in remaining_vms]
                print(f"\n[cleanup] Destroying {len(remaining_vms)} remaining VM(s): "
                      f"{', '.join(names)}", flush=True)
                if not _shutdown_requested.is_set():
                    # Normal exit: download monitor CSVs before destroying
                    for vm in remaining_vms:
                        if vm.ssh:
                            try:
                                download_monitor_csv(vm, run_output_dir)
                            except Exception:
                                pass
                # Destroy VMs via provider API (no SSH needed)
                for vm in remaining_vms:
                    print(f"[cleanup] Destroying {vm.name} ({vm.ip})...", flush=True)
                    _close_ssh_safe(vm)
                    try:
                        provider.destroy_vm(vm)
                    except Exception as e:
                        print(f"[cleanup] WARNING: Failed to destroy {vm.name}: {e}", flush=True)
                    vm.destroyed_at = time.monotonic()
                print("[cleanup] All VMs destroyed.", flush=True)
        else:
            if remaining_vms:
                print("\n--keep-vms set. VMs left running:")
                for vm in remaining_vms:
                    print(f"  {vm.name}: ssh root@{vm.ip}")


if __name__ == "__main__":
    main()
