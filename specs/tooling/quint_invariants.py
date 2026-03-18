#!/usr/bin/env python3

from __future__ import annotations

import argparse
import re
import subprocess
import sys
import time
from dataclasses import dataclass
from pathlib import Path
from typing import Iterable


INV_RE = re.compile(r"^\s*val\s+(inv_[A-Za-z0-9_]+)\s*=")
DEFAULT_STEPS_RE = re.compile(r"^\s*///\s*@default_steps\s+(\d+)\s*$")
SUMMARY_RE = re.compile(r"\((\d+)ms at (\d+) traces/second\)")


@dataclass(frozen=True)
class Invariant:
    name: str
    line_index: int
    default_steps: int | None


@dataclass(frozen=True)
class BenchmarkResult:
    invariant: Invariant
    benchmark_samples: int
    benchmark_steps: int
    engine_seconds: float
    wall_seconds: float
    estimated_seconds_default_20: float
    recommended_default_steps: int
    estimated_seconds_recommended: float


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
            default_match = DEFAULT_STEPS_RE.match(lines[scan])
            if default_match:
                default_steps = int(default_match.group(1))
                break
            scan -= 1
        invariants.append(Invariant(match.group(1), idx, default_steps))
    return invariants


def find_invariant(spec_path: Path, invariant_name: str) -> Invariant:
    for invariant in parse_invariants(spec_path):
        if invariant.name == invariant_name:
            return invariant
    raise SystemExit(f"unknown invariant: {invariant_name}")


def run_quint(
    spec_path: Path,
    invariant: str,
    samples: int,
    steps: int,
    *,
    quiet: bool,
    verbosity: int | None = None,
) -> tuple[float, float | None]:
    cmd = [
        "quint",
        "run",
        spec_path.name,
        "--backend=rust",
        f"--invariant={invariant}",
        f"--max-samples={samples}",
        f"--max-steps={steps}",
    ]
    if verbosity is not None:
        cmd.append(f"--verbosity={verbosity}")
    started = time.monotonic()
    completed = subprocess.run(
        cmd,
        cwd=spec_path.parent,
        check=False,
        capture_output=quiet,
        text=True,
    )
    elapsed = time.monotonic() - started
    if completed.returncode != 0:
        if quiet:
            sys.stderr.write(completed.stdout)
            sys.stderr.write(completed.stderr)
        raise SystemExit(f"quint failed for {invariant} with exit code {completed.returncode}")
    engine_seconds = None
    if quiet:
        combined_output = completed.stdout + completed.stderr
        summary_match = SUMMARY_RE.search(combined_output)
        if not summary_match:
            raise SystemExit(f"could not parse quint summary for {invariant}")
        engine_seconds = int(summary_match.group(1)) / 1000.0
    return elapsed, engine_seconds


def estimate_seconds(
    observed_seconds: float,
    observed_samples: int,
    observed_steps: int,
    target_samples: int,
    target_steps: int,
) -> float:
    return observed_seconds * (target_samples / observed_samples) * (target_steps / observed_steps)


def recommend_default_steps(
    observed_seconds: float,
    observed_samples: int,
    observed_steps: int,
    target_seconds: float,
    default_samples: int,
    min_steps: int,
    max_steps: int,
) -> int:
    if observed_seconds <= 0:
        return max_steps
    raw = target_seconds * observed_samples * observed_steps / (observed_seconds * default_samples)
    rounded = int(round(raw))
    return max(min_steps, min(max_steps, rounded))


def update_default_steps(spec_path: Path, defaults: dict[str, int]) -> None:
    lines = spec_path.read_text().splitlines()
    output: list[str] = []
    for line in lines:
        match = INV_RE.match(line)
        if match:
            name = match.group(1)
            if name in defaults:
                indent = line[: len(line) - len(line.lstrip())]
                annotation = f"{indent}/// @default_steps {defaults[name]}"
                if output and DEFAULT_STEPS_RE.match(output[-1]):
                    output[-1] = annotation
                else:
                    output.append(annotation)
        output.append(line)
    spec_path.write_text("\n".join(output) + "\n")


def write_markdown_table(
    output_path: Path,
    spec_path: Path,
    results: Iterable[BenchmarkResult],
    *,
    target_seconds: float,
    default_samples: int,
) -> None:
    rows = list(results)
    bench_samples = rows[0].benchmark_samples if rows else "?"
    bench_steps = rows[0].benchmark_steps if rows else "?"
    lines = [
        "# Quint Invariant Benchmarks",
        "",
        f"- Spec: `{spec_path.name}`",
        f"- Benchmark workload: `--max-samples={bench_samples} --max-steps={bench_steps}`",
        f"- Default suite sample count for estimates: `{default_samples}`",
        f"- Per-invariant target runtime budget: `{target_seconds:.1f}s`",
        "- `Bench engine s` and the estimated columns use Quint's reported simulator time.",
        "- `Bench wall s` includes process startup and output handling overhead.",
        "- Runtime estimates assume near-linear scaling with samples and max steps.",
        "",
        "| Invariant | Bench engine s | Bench wall s | Est engine s @ 10k x 20 | Default steps | Est engine s @ 10k x default |",
        "| --- | ---: | ---: | ---: | ---: | ---: |",
    ]
    for row in rows:
        lines.append(
            f"| `{row.invariant.name}` | "
            f"{row.engine_seconds:.2f} | "
            f"{row.wall_seconds:.2f} | "
            f"{row.estimated_seconds_default_20:.2f} | "
            f"{row.recommended_default_steps} | "
            f"{row.estimated_seconds_recommended:.2f} |"
        )
    output_path.write_text("\n".join(lines) + "\n")


def print_run_header(invariant: str, samples: int, steps: int) -> None:
    print(f"=== Checking {invariant} (samples={samples}, steps={steps}) ===", flush=True)


def cmd_run(args: argparse.Namespace) -> None:
    spec_path = Path(args.spec).resolve()
    invariants = parse_invariants(spec_path)
    if args.invariant:
        selected = [find_invariant(spec_path, args.invariant)]
    else:
        selected = invariants
    for invariant in selected:
        steps = args.steps if args.steps is not None else (invariant.default_steps or args.fallback_steps)
        print_run_header(invariant.name, args.samples, steps)
        run_quint(
            spec_path,
            invariant.name,
            args.samples,
            steps,
            quiet=False,
            verbosity=args.verbosity,
        )


def cmd_benchmark(args: argparse.Namespace) -> None:
    spec_path = Path(args.spec).resolve()
    invariants = parse_invariants(spec_path)
    if args.invariant:
        invariants = [find_invariant(spec_path, args.invariant)]
    results: list[BenchmarkResult] = []
    for invariant in invariants:
        wall_seconds, engine_seconds = run_quint(
            spec_path,
            invariant.name,
            args.benchmark_samples,
            args.benchmark_steps,
            quiet=True,
        )
        est_default_20 = estimate_seconds(
            engine_seconds,
            args.benchmark_samples,
            args.benchmark_steps,
            args.default_samples,
            20,
        )
        recommended_steps = recommend_default_steps(
            engine_seconds,
            args.benchmark_samples,
            args.benchmark_steps,
            args.target_seconds,
            args.default_samples,
            args.min_default_steps,
            args.max_default_steps,
        )
        est_recommended = estimate_seconds(
            engine_seconds,
            args.benchmark_samples,
            args.benchmark_steps,
            args.default_samples,
            recommended_steps,
        )
        results.append(
            BenchmarkResult(
                invariant=invariant,
                benchmark_samples=args.benchmark_samples,
                benchmark_steps=args.benchmark_steps,
                engine_seconds=engine_seconds,
                wall_seconds=wall_seconds,
                estimated_seconds_default_20=est_default_20,
                recommended_default_steps=recommended_steps,
                estimated_seconds_recommended=est_recommended,
            )
        )
        print(
            f"{invariant.name}: engine={engine_seconds:.2f}s wall={wall_seconds:.2f}s "
            f"est@{args.default_samples}x20={est_default_20:.2f}s "
            f"default_steps={recommended_steps}",
            flush=True,
        )
    if args.output_markdown:
        write_markdown_table(
            Path(args.output_markdown).resolve(),
            spec_path,
            results,
            target_seconds=args.target_seconds,
            default_samples=args.default_samples,
        )
    if args.write_annotations:
        update_default_steps(
            spec_path,
            {result.invariant.name: result.recommended_default_steps for result in results},
        )


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description="Run and benchmark Quint invariants.")
    subparsers = parser.add_subparsers(dest="command", required=True)

    run_parser = subparsers.add_parser("run", help="Run one or all invariants.")
    run_parser.add_argument("--spec", default="doltswarm_verify.qnt")
    run_parser.add_argument("--samples", type=int, required=True)
    run_parser.add_argument("--steps", type=int)
    run_parser.add_argument("--fallback-steps", type=int, default=20)
    run_parser.add_argument("--invariant")
    run_parser.add_argument("--verbosity", type=int)
    run_parser.set_defaults(func=cmd_run)

    bench_parser = subparsers.add_parser("benchmark", help="Benchmark all invariants.")
    bench_parser.add_argument("--spec", default="doltswarm_verify.qnt")
    bench_parser.add_argument("--benchmark-samples", type=int, default=1000)
    bench_parser.add_argument("--benchmark-steps", type=int, default=20)
    bench_parser.add_argument("--default-samples", type=int, default=10000)
    bench_parser.add_argument("--target-seconds", type=float, default=5.0)
    bench_parser.add_argument("--min-default-steps", type=int, default=3)
    bench_parser.add_argument("--max-default-steps", type=int, default=20)
    bench_parser.add_argument("--output-markdown")
    bench_parser.add_argument("--invariant")
    bench_parser.add_argument("--write-annotations", action="store_true")
    bench_parser.set_defaults(func=cmd_benchmark)
    return parser


def main() -> None:
    parser = build_parser()
    args = parser.parse_args()
    args.func(args)


if __name__ == "__main__":
    main()
