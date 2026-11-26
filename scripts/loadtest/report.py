"""Reports for scripts/loadtest/loadtest.sh.

  report.py <results dir>                    report.md and steps.csv for a run
  report.py --verdict <summary.json> <phase> "holds ...", "slow ..." or "broken ..." for one step

For each load phase, one row per step: offered load, what was served, latency, errors and
the server's CPU, memory, proxy CPU and network during that step. A step holds when its p95
is under 500 ms (1.5 s for the 1 s /sleep requests) with under 1% errors; a phase stops at the
first step with over 10% errors or a p95 over 5 s (6 s for /sleep), or when k6 did not finish.
"""
import csv
import json
import os
import sys

TITLES = {
    "light": ("Raw throughput: / (the app does nothing)", "req/s"),
    "cpu": ("CPU-bound app with autoscaling: /work", "req/s"),
    "conns": ("Concurrent connections: /sleep?ms=1000", "connections"),
    "bw": ("Bandwidth: 1 MiB responses", "req/s"),
}


def base(phase):
    return 1000 if phase == "conns" else 0  # each /sleep request takes 1 s on purpose


def load_step(path):
    """The step's numbers from a k6 summary, or None when k6 did not finish."""
    try:
        data = json.load(open(path, encoding="utf-8"))
    except (OSError, ValueError):
        return None
    m = data.get("metrics", {})
    sc = next((k.split("scenario:")[1].rstrip("}") for k in m if "scenario:" in k), None)
    if not sc:
        return None
    g = lambda metric, stat: m.get(f"{metric}{{scenario:{sc}}}", {}).get("values", {}).get(stat, 0)
    return {"count": g("http_reqs", "count"), "p50": g("http_req_duration", "med"), "p95": g("http_req_duration", "p(95)"),
            "p99": g("http_req_duration", "p(99)"), "err": 100 * g("http_req_failed", "rate"),
            "dropped": int(g("dropped_iterations", "count")), "bytes": g("data_received", "count"),
            "duration_s": data.get("state", {}).get("testRunDurationMs", 0) / 1000}


def judge(st, phase):
    if st is None or not st["count"]:
        return "broken", "k6 did not finish (killed or no response)"
    b = base(phase)
    desc = f"p95 {st['p95']:.0f} ms, {st['err']:.1f}% errors, {st['dropped']} not sent"
    if st["err"] > 10 or st["p95"] > b + 5000:
        return "broken", desc
    if st["p95"] < b + 500 and st["err"] < 1:
        return "holds", desc
    return "slow", desc


if sys.argv[1] == "--verdict":
    v, d = judge(load_step(sys.argv[2]), sys.argv[3])
    print(f"{v} ({d})")
    sys.exit(0)

OUT = sys.argv[1]


def rows(fn):
    p = os.path.join(OUT, fn)
    return list(csv.DictReader(open(p, encoding="utf-8"))) if os.path.exists(p) else []


host = [{k: float(v) for k, v in r.items() if v not in ("", None)} for r in rows("host.csv")]
inst = []
if os.path.exists(os.path.join(OUT, "cpu-instances.csv")):
    for line in open(os.path.join(OUT, "cpu-instances.csv")):
        parts = line.strip().split(",")
        if len(parts) >= 2 and parts[0] and parts[1]:
            inst.append((float(parts[0]), int(parts[1])))


def peak(t0, t1, k):
    return max((h[k] for h in host if t0 <= h["t"] < t1 and k in h), default=0.0)


meta = dict(l.strip().split("=", 1) for l in open(os.path.join(OUT, "meta.txt")) if "=" in l)
md = [f"# Load test {meta.get('run', '')}", "",
      f"Platform: {meta.get('console', '')}. Server (cores, MB, kernel, containers at start): {meta.get('host', '')}.",
      f"Load sent with k6 from {meta.get('generator', 'a workstation')} over HTTPS, one k6 process per step. "
      "A step holds when p95 < 500 ms (1.5 s for the 1 s /sleep requests) and errors < 1%.", ""]

idx = []
if os.path.exists(os.path.join(OUT, "steps.idx")):
    for line in open(os.path.join(OUT, "steps.idx")):
        ph, v, t0, t1, hold = line.strip().split(",")
        idx.append((ph, int(v), float(t0), float(t1), float(hold)))

steps_out = []
for ph in dict.fromkeys(i[0] for i in idx):
    title, unit = TITLES.get(ph, (ph, "req/s"))
    cpu = ph == "cpu"
    md += [f"## {title}", "",
           f"| {unit} offered | served req/s | p50 ms | p95 ms | p99 ms | errors | not sent | MB/s in | server CPU | memory | proxy CPU | out Mbit/s |" + (" instances |" if cpu else ""),
           "|---|---|---|---|---|---|---|---|---|---|---|---|" + ("---|" if cpu else "")]
    held = broke = None
    for _, v, t0, t1, hold in (i for i in idx if i[0] == ph):
        st = load_step(os.path.join(OUT, f"{ph}-{v:06d}.summary.json"))
        verdict, desc = judge(st, ph)
        # the load ran for the last part of the step (after k6 started)
        w0, w1 = max(t0, t1 - (st["duration_s"] if st else hold) - 2), t1
        row = {"phase": ph, "offered": v, "verdict": verdict,
               "served_rps": round(st["count"] / hold, 1) if st else 0, "p50_ms": round(st["p50"], 1) if st else "",
               "p95_ms": round(st["p95"], 1) if st else "", "p99_ms": round(st["p99"], 1) if st else "",
               "error_pct": round(st["err"], 2) if st else "", "not_sent": st["dropped"] if st else "",
               "mb_in_per_s": round(st["bytes"] / hold / 1e6, 2) if st else "",
               "cpu_pct": peak(w0, w1, "cpu_pct"), "mem_pct": peak(w0, w1, "mem_pct"),
               "caddy_cpu_pct": peak(w0, w1, "caddy_cpu_pct"), "tx_mbps": peak(w0, w1, "tx_mbps")}
        if cpu:
            row["instances"] = max((k for t, k in inst if w0 <= t <= w1), default="")
        steps_out.append(row)
        if verdict == "holds" and broke is None:
            held = row
        if verdict != "holds" and broke is None:
            broke = row
        if st is None:
            md.append(f"| {v} | k6 did not finish |  |  |  |  |  |  | {row['cpu_pct']:.0f}% | {row['mem_pct']:.0f}% | {row['caddy_cpu_pct']:.0f}% | {row['tx_mbps']:.0f} |" + (" |" if cpu else ""))
            continue
        md.append(f"| {v} | {row['served_rps']} | {row['p50_ms']} | {row['p95_ms']} | {row['p99_ms']} | {row['error_pct']}% | {row['not_sent']} | {row['mb_in_per_s']} | "
                  f"{row['cpu_pct']:.0f}% | {row['mem_pct']:.0f}% | {row['caddy_cpu_pct']:.0f}% | {row['tx_mbps']:.0f} |" + (f" {row.get('instances', '')} |" if cpu else ""))
    line = f"**Holds up to:** {held['offered']} {unit} ({held['served_rps']} req/s served, p95 {held['p95_ms']} ms)" if held else "**Holds up to:** none of the steps"
    if broke:
        line += f". **First step that did not hold:** {broke['offered']} {unit}" + (f" (p95 {broke['p95_ms']} ms, {broke['error_pct']}% errors)." if broke["p95_ms"] != "" else " (k6 did not finish).")
    else:
        line += ". Every step held."
    md += ["", line, ""]

dens = rows("density.csv")
if dens:
    stop = open(os.path.join(OUT, "density-stop.txt")).read().strip() if os.path.exists(os.path.join(OUT, "density-stop.txt")) else ""
    md += ["## Density: small services (100m CPU, 64 MB each)", "", "| services | batch deploy s | live | answering | memory | containers |", "|---|---|---|---|---|---|"]
    md += [f"| {d['services']} | {d['deploy_s']} | {d['live']} | {d['serving']} | {d['mem_pct']}% | {d['containers']} |" for d in dens]
    md += ["", f"**Stopped:** {stop}. At the last batch: {dens[-1]['live']} services live, {dens[-1]['serving']} answering.", ""]

if steps_out:
    keys = list(dict.fromkeys(k for r in steps_out for k in r))
    with open(os.path.join(OUT, "steps.csv"), "w", newline="", encoding="utf-8") as f:
        w = csv.DictWriter(f, fieldnames=keys)
        w.writeheader()
        w.writerows(steps_out)
open(os.path.join(OUT, "report.md"), "w", encoding="utf-8", newline="\n").write("\n".join(md) + "\n")
print("\n".join(md))
