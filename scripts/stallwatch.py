#!/usr/bin/env python3
"""stallwatch: catch what the player's main thread is doing during a stall.

RefluxApp's PlayerKit logs "display tick gap Nms (main thread stall?)" only
AFTER a stall has ended, so sampling on demand always misses the culprit.
`watch` keeps short, overlapping `sample` runs going (a new one every --stride
seconds, each --window seconds long, so every instant is covered). When a
tick-gap event >= --threshold shows up it keeps the run that covers the stall
window, then prints the main thread's heaviest non-idle call chains.

    stallwatch.py watch                  # attach to RefluxAppleMac, 300ms threshold
    stallwatch.py watch --threshold 150  # more sensitive
    stallwatch.py analyze FILE           # re-read a sample file

Output (default /tmp/stallwatch):
    captures/stall-<time>-<gap>ms.txt          raw `sample` output that covers the stall
    captures/stall-<time>-<gap>ms.summary.txt  main-thread summary of that capture
    events.jsonl                               every tick gap >= --record-min, top CPU at that moment
    context.log                                tick gaps, frame-queue stats, EnrichPipeline / network errors

Sampling itself perturbs the target a little; raise --interval to reduce it.
"""
import argparse
import collections
import json
import os
import re
import shutil
import signal
import subprocess
import sys
import threading
import time

LOG = "/usr/bin/log"
STAMP_RE = re.compile(r"^(\d{4}-\d\d-\d\d \d\d:\d\d:\d\d)\.(\d+)")
TICK_RE = re.compile(r"display tick gap (\d+)ms")
NODE_RE = re.compile(r"^([ +!:|]*)(\d+) (.*)$")
INTERVAL_RE = re.compile(r"every (\d+) millisecond")


def parse_stamp(line):
    m = STAMP_RE.match(line)
    if not m:
        return None
    return time.mktime(time.strptime(m.group(1), "%Y-%m-%d %H:%M:%S")) + float("0." + m.group(2))


def say(msg):
    print(msg, flush=True)


# ---- analysis ---------------------------------------------------------------


def main_thread_block(lines):
    """Lines of the main thread's tree in a `sample` call graph."""
    start = next((i + 1 for i, l in enumerate(lines) if l.startswith("Call graph:")), None)
    if start is None:
        return []
    block, inside = [], False
    for l in lines[start:]:
        m = NODE_RE.match(l)
        if not m:
            if inside:
                break
            continue
        name = m.group(3)
        if name.startswith("Thread_"):
            if inside:
                break
            inside = "Main Thread" in name or "com.apple.main-thread" in name
        if inside:
            block.append(l)
    return block


def short(name):
    name = name.split("  (in ")[0]
    name = re.sub(r"\s*\[0x[0-9a-f]+\]$", "", name).strip()
    return name[:70]


def heaviest_chains(block):
    """(total, idle, Counter{chain tuple: self samples}) for the main thread.

    Idle = leaf is mach_msg* directly under the run loop's wait. A mach_msg under
    anything else (sync XPC, dispatch_sync ...) is a real block and stays busy.
    """
    names, depths, counts, parents = [], [], [], []
    stack = []
    child_sum = collections.Counter()
    for l in block:
        m = NODE_RE.match(l)
        d, n = len(m.group(1)), int(m.group(2))
        while stack and depths[stack[-1]] >= d:
            stack.pop()
        parent = stack[-1] if stack else None
        names.append(short(m.group(3)))
        depths.append(d)
        counts.append(n)
        parents.append(parent)
        if parent is not None:
            child_sum[parent] += n
        stack.append(len(names) - 1)
    if not names:
        return 0, 0, collections.Counter()

    chains = collections.Counter()
    idle = 0
    for i, n in enumerate(counts):
        own = n - child_sum[i]
        if own <= 0:
            continue
        chain, j = [], i
        while j is not None:
            chain.append(names[j])
            j = parents[j]
        chain.reverse()
        if chain[-1].startswith("mach_msg") and "__CFRunLoopServiceMachPort" in chain[-7:-1]:
            idle += own
        else:
            chains[tuple(chain)] += own
    return counts[0], idle, chains


def analyze_file(path, top=5):
    text = open(path, errors="replace").read()
    m = INTERVAL_RE.search(text)
    interval = int(m.group(1)) if m else 10
    total, idle, chains = heaviest_chains(main_thread_block(text.splitlines()))
    if not total:
        return ["  (no main thread found in %s)" % path]
    busy = total - idle
    out = ["  main thread: %d samples @%dms, idle %.0f%%, busy %d ms" % (total, interval, 100.0 * idle / total, busy * interval)]
    for chain, n in chains.most_common(top):
        leaf_first = list(reversed(chain))[:12]
        out.append("  %6d ms  %s" % (n * interval, " < ".join(leaf_first)))
    return out


# ---- watch ------------------------------------------------------------------


class Chunk:
    def __init__(self, path, start, window, proc):
        self.path, self.start, self.end, self.proc = path, start, start + window + 0.5, proc

    def done(self):
        return self.proc.poll() is not None

    def usable(self):
        return self.done() and self.proc.returncode == 0 and os.path.exists(self.path)


class Watcher:
    def __init__(self, args):
        self.a = args
        self.tmp = os.path.join(args.out, "tmp")
        self.caps = os.path.join(args.out, "captures")
        os.makedirs(self.tmp, exist_ok=True)
        os.makedirs(self.caps, exist_ok=True)
        self.events = open(os.path.join(args.out, "events.jsonl"), "a")
        self.context = open(os.path.join(args.out, "context.log"), "a")
        self.lock = threading.Lock()
        self.chunks, self.pending = [], []
        self.stop = threading.Event()
        self.logproc = None
        self.captured = 0

    def find_pid(self):
        if self.a.pid:
            return self.a.pid
        r = subprocess.run(["pgrep", "-x", self.a.process], capture_output=True, text=True)
        pids = r.stdout.split()
        return int(pids[0]) if pids else None

    def sampler_loop(self):
        n, warned = 0, False
        while not self.stop.is_set():
            pid = self.find_pid()
            if pid is None:
                if not warned:
                    say("waiting for process %s ..." % self.a.process)
                    warned = True
                self.stop.wait(2)
                continue
            warned = False
            path = os.path.join(self.tmp, "chunk-%06d.txt" % n)
            n += 1
            proc = subprocess.Popen(["sample", str(pid), str(self.a.window), str(self.a.interval), "-file", path],
                                    stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
            with self.lock:
                self.chunks.append(Chunk(path, time.time(), self.a.window, proc))
            self.stop.wait(self.a.stride)

    def tick_loop(self):
        while not self.stop.wait(0.5):
            self.resolve()
            self.cleanup()

    def top_cpu(self):
        r = subprocess.run(["ps", "-Ar", "-o", "pcpu=,comm="], capture_output=True, text=True)
        return [" ".join(l.split(None, 1)[0:1] + [os.path.basename(l.split(None, 1)[1].strip())]) for l in r.stdout.splitlines()[:6] if l.strip()]

    def on_line(self, line):
        ts = parse_stamp(line)
        if ts is None:
            return
        self.context.write(line if line.endswith("\n") else line + "\n")
        self.context.flush()
        m = TICK_RE.search(line)
        if not m:
            return
        gap = int(m.group(1))
        if gap < self.a.record_min:
            return
        ev = {"ts": ts, "gap": gap, "top_cpu": self.top_cpu()}
        self.events.write(json.dumps({"time": time.strftime("%H:%M:%S", time.localtime(ts)), "gap_ms": gap, "top_cpu": ev["top_cpu"]}, ensure_ascii=False) + "\n")
        self.events.flush()
        if gap >= self.a.threshold:
            say("stall %dms @ %s  (waiting for the covering sample to finish)" % (gap, time.strftime("%H:%M:%S", time.localtime(ts))))
            with self.lock:
                self.pending.append(ev)

    def resolve(self, force=False):
        now = time.time()
        with self.lock:
            for ev in list(self.pending):
                a, b = ev["ts"] - ev["gap"] / 1000.0 - 0.5, ev["ts"] + 0.5
                cands = [c for c in self.chunks if c.start <= b and c.end >= a]
                if any(not c.done() for c in cands):
                    continue
                if not force and now < ev["ts"] + self.a.stride:
                    continue  # a chunk that also covers the tail may not have launched yet
                self.pending.remove(ev)
                usable = [c for c in cands if c.usable()]
                stamp = time.strftime("%Y%m%d-%H%M%S", time.localtime(ev["ts"]))
                if not usable:
                    say("stall %dms @ %s: no sample covered it" % (ev["gap"], stamp))
                    continue
                best = max(usable, key=lambda c: min(c.end, b) - max(c.start, a))
                dest = os.path.join(self.caps, "stall-%s-%dms.txt" % (stamp, ev["gap"]))
                shutil.copy(best.path, dest)
                summary = analyze_file(dest, self.a.top)
                open(dest[:-4] + ".summary.txt", "w").write("\n".join(summary) + "\n")
                self.captured += 1
                say("CAPTURED stall %dms @ %s -> %s" % (ev["gap"], stamp, dest))
                say("  top CPU then: " + "; ".join(ev["top_cpu"][:4]))
                say("\n".join(summary))

    def cleanup(self):
        now = time.time()
        with self.lock:
            keep = []
            for c in self.chunks:
                needed = any(c.start <= ev["ts"] + 0.5 and c.end >= ev["ts"] - ev["gap"] / 1000.0 - 0.5 for ev in self.pending)
                if c.done() and c.end < now - 30 and not needed:
                    try:
                        os.remove(c.path)
                    except FileNotFoundError:
                        pass
                else:
                    keep.append(c)
            self.chunks = keep

    def shutdown(self):
        self.stop.set()
        if self.logproc:
            self.logproc.terminate()

    def run(self):
        signal.signal(signal.SIGINT, lambda *_: self.shutdown())
        signal.signal(signal.SIGTERM, lambda *_: self.shutdown())
        pred = ('process == "%s" AND (eventMessage CONTAINS "tick gap" OR eventMessage CONTAINS "EnrichPipeline" '
                'OR eventMessage CONTAINS "finished with error" OR eventMessage CONTAINS "FigVideoQueueGMStats")') % self.a.process
        self.logproc = subprocess.Popen([LOG, "stream", "--style", "compact", "--predicate", pred],
                                        stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, text=True, bufsize=1)
        threads = [threading.Thread(target=self.sampler_loop, daemon=True), threading.Thread(target=self.tick_loop, daemon=True)]
        for t in threads:
            t.start()
        say("stallwatch: process=%s threshold=%dms window=%ds stride=%ds interval=%dms out=%s"
            % (self.a.process, self.a.threshold, self.a.window, self.a.stride, self.a.interval, self.a.out))
        for line in self.logproc.stdout:
            self.on_line(line)
            if self.stop.is_set():
                break
        # let samples that cover an already-seen stall finish before leaving
        self.stop.set()
        deadline = time.time() + self.a.window + 5
        while self.pending and time.time() < deadline:
            self.resolve(force=True)
            time.sleep(0.5)
        with self.lock:
            for c in self.chunks:
                if not c.done() and not any(c.start <= ev["ts"] + 0.5 for ev in self.pending):
                    c.proc.terminate()
        shutil.rmtree(self.tmp, ignore_errors=True)
        say("stallwatch: stopped, %d stall(s) captured in %s" % (self.captured, self.caps))


def main():
    p = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = p.add_subparsers(dest="cmd", required=True)
    w = sub.add_parser("watch", help="sample continuously and keep the samples that cover a stall")
    w.add_argument("--process", default="RefluxAppleMac", help="process name (log predicate and pgrep -x)")
    w.add_argument("--pid", type=int, help="sample this pid instead of looking the process up")
    w.add_argument("--threshold", type=int, default=300, help="ms; keep a capture at or above this tick gap")
    w.add_argument("--record-min", type=int, default=100, help="ms; log tick gaps at or above this to events.jsonl")
    w.add_argument("--window", type=int, default=4, help="seconds per sample run")
    w.add_argument("--stride", type=int, default=2, help="seconds between sample run starts (< window keeps coverage continuous)")
    w.add_argument("--interval", type=int, default=10, help="ms between samples")
    w.add_argument("--top", type=int, default=5, help="call chains to print per capture")
    w.add_argument("--out", default="/tmp/stallwatch")
    an = sub.add_parser("analyze", help="summarize the main thread of a sample file")
    an.add_argument("file")
    an.add_argument("--top", type=int, default=5)
    args = p.parse_args()
    if args.cmd == "analyze":
        say("\n".join(analyze_file(args.file, args.top)))
        return
    if args.stride >= args.window:
        p.error("--stride must be smaller than --window or there are coverage gaps")
    Watcher(args).run()


if __name__ == "__main__":
    main()
