# DoltSwarm Protocol & Spec Improvements (Round 5, Consolidated)

Consolidated findings after moving finalization from `stable_hlc` to an active-peer, causality-based seen frontier (`peer_seen_frontier`).

Decisions applied in this round:
- The finalization witness is now causal: events harden only when covered by every peer in the current `active_peers` witness set.
- HLC remains only a deterministic ordering/tie-break mechanism.
- This round reassesses the original five top-level review findings against that new stability model.

Items below are ordered by remaining protocol-correctness importance. Resolved or materially-improved items are retained here so the review history stays closed over the original five findings.

---

## Resolved in this round

### 1. ~~Event replayability after finalized-root changes is still under-specified~~ FIXED

**Refs:** protocol data types `CIDPayload` / `Event`, local write payload construction, receive lifecycle, anchored replay/finalization, and `SYNC_FINALIZED` re-anchor contract; spec `Event`, `anchoredFold`, `computeMergedRoot`, `finalizeBatch`, and local event constructors.

- Protocol: `doltswarm-protocol.md` lines 147-174, 272-274, 289-291, 305-308, 317, 389-390, 473.
- Spec: `specs/doltswarm_verify.qnt` lines 72-80, 297-307, 323-330, 354-363, 748-755, 1109-1113, 1498-1504.

**Status.** Fixed by choosing **Option A** from this round: add `anchor_root_hash` to every event and replay/finalize events against their recorded anchors.

**Implemented protocol behavior.**
- `CIDPayload`, `SignedPayload`, and `Event` now carry `anchor_root_hash` in addition to `root_hash`.
- `LOCAL_WRITE` stamps `anchor_root_hash = finalized_root` when the event is created.
- `CHUNKS_READY` now means both `root_hash` and `anchor_root_hash` are locally available for that event, and receive-side chunk fetch requests both when needed.
- `RECONCILE` now uses a shared `AnchoredFold(heads, start_root)` contract:
  - sort heads by HLC,
  - start from the current accumulator,
  - fold `MergeRoots(acc, head.root_hash, head.anchor_root_hash)`.
- `ADVANCE_STABILITY` uses the same anchored fold for stable-frontier finalization.
- The singleton-head receive path was tightened to use the same anchored replay semantics instead of blindly fast-forwarding to `head.root_hash`.
- `SYNC_FINALIZED` re-anchor text now explicitly says tentative events replay over the new accumulator using each event's recorded `anchor_root_hash`.

**Implemented spec behavior.**
- `Event` now has an `anchor_root_hash` field.
- `fixedAnchorFold` was replaced with `anchoredFold`, which merges each event against its own recorded anchor.
- `computeMergedRoot` and `finalizeBatch` now use the anchored fold, so the Quint replay/finalization semantics match the protocol text.
- All local event constructors (`do_write`, `do_resolve_conflict`, `resolveFinalizedConflictResult`) stamp `anchor_root_hash = st.finalized_root`.
- The finalized-conflict resolution contract now explicitly checks that emitted resolution events carry the creator's current finalized anchor.

**Why this closes the issue.**
- Events are no longer ambiguous snapshots; they are anchored tentative branch states.
- After `finalized_root` changes, the protocol has enough information to replay retained tentative events coherently.
- The protocol/spec pair now define the same replay rule, including the single-head path and the chunk-readiness requirements for anchor roots.

### 2. ~~`SYNC_FINALIZED` still excludes the `EMPTY_ROOT` recovery path~~ FIXED

**Refs:** protocol heartbeat-triggered sync gate, `SYNC_FINALIZED` precondition, and sync-design notes; spec `syncEnabled` and empty-root ADOPT regression.

- Protocol: `doltswarm-protocol.md` lines 377, 426, 580, 586, 710.
- Spec: `specs/doltswarm_verify.qnt` lines 609-615, 1362-1373.

**Status.** Fixed by choosing **Option A** from this round: allow `SYNC_FINALIZED` whenever finalized roots differ, with `EMPTY_ROOT` treated as the root of finalized lineage.

**Implemented protocol behavior.**
- HEARTBEAT now triggers `SYNC_FINALIZED` whenever `received.finalized_root ≠ finalized_root`; the special non-empty guard was removed.
- `SYNC_FINALIZED` no longer requires both sides to have non-empty finalized roots.
- The protocol now states explicitly that `EMPTY_ROOT` participates in normal lineage classification, so:
  - `EMPTY_ROOT -> non-empty` is an **ADOPT** case,
  - `non-empty -> EMPTY_ROOT` is a **NOOP** case,
  - only incomparable non-empty roots take the **MERGE** branch.
- Modeling/design notes now describe empty-peer recovery as part of the same finalized-sync machinery, not as a special excluded case.

**Implemented spec behavior.**
- Removed the `st_local.finalized_root != EMPTY_ROOT` and `st_remote.finalized_root != EMPTY_ROOT` guards from `syncEnabled`.
- Added `inv_sync_empty_root_adopts`, which checks that a connected peer at `EMPTY_ROOT` takes the normal `SYNC_ADOPT` branch and adopts the remote non-empty finalized root/events.

**Why this closes the issue.**
- `EMPTY_ROOT` is now a normal lineage root instead of a protocol exception.
- Recovery of empty peers no longer depends on retaining finalized event bodies in `clock` or on always using `PEER_JOIN`.
- The protocol/spec pair now define one uniform finalized-recovery story for bootstrap, crash recovery, and reconnect after compaction.

---

## Correctness-Prioritized Open Items

### 3. ~~Seen-frontier finalization fixes the witness semantics, but heartbeat anti-entropy still needs one more tightening~~ PARTIALLY FIXED

**Refs:** protocol heartbeat digests + pull-on-demand; spec heartbeat over-approximation note and heartbeat pull action.

- Protocol: `doltswarm-protocol.md` lines 361-376.
- Spec: `specs/doltswarm_verify.qnt` lines 14-18, 252-284, 892-930.

**Status.** The major semantic problem is fixed. Finalization now depends on what the active witness set has actually observed, not on a scalar HLC proxy.

**Remaining problem.** Heartbeat still uses digest mismatch to decide when to refresh the remote head set and remote seen frontier. That is good for bandwidth, but it leaves one remaining ambiguity:
- a peer can already know the remote heads/frontier by digest,
- still be locally missing some ancestor closure for those already-known remote targets,
- and have no explicit rule saying "continue closure pull even when digests still match".

The Quint model currently over-approximates this path by reading the remote state directly, so the spec is slightly stronger and simpler than the prose here.

**Options.**

**Option A (recommended if minimizing traffic is the higher priority): keep digests, add an explicit `closure_deficit(q)` rule.**

Meaning: digest equality suppresses remote-set refresh only if the local peer has no known closure deficit for peer `q`.

Pros:
- keeps the common-case communication pattern compact;
- closes the last stall case without changing message shapes.

Cons:
- adds one more abstract state concept.

**Option B (recommended if proof/spec simplicity is the higher priority): send the full seen frontier on every heartbeat.**

Pros:
- simpler semantics;
- easier to explain and model;
- still much smaller than sending full remote clock state.

Cons:
- more control-plane traffic than digest-only comparison.

**Option C: send both the full head set and the full seen frontier on every heartbeat.**

Pros:
- simplest protocol text.

Cons:
- highest heartbeat traffic;
- least aligned with the protocol goal of minimizing communication.

**Recommended protocol fix.**
Pick one of two directions explicitly:
- bandwidth-first: define `closure_deficit(q)` and require ancestor pull while it remains true, even if both digests match; or
- proof-first: stop digesting `seen_frontier` and broadcast it in full every heartbeat.

I would choose:
- `closure_deficit(q)` if network minimization remains a hard design goal;
- full `seen_frontier` if protocol/spec simplicity now matters more than a small amount of extra heartbeat traffic.

**Recommended spec fix.**
- If keeping digests, add an explicit modeled concept of known remote targets / closure deficit so the spec matches the prose.
- If sending full frontier, simplify the heartbeat abstraction so the model no longer over-approximates a digest gate that the protocol depends on.

**Why this should be done.**
The seen-frontier move made the finalization witness correct. This last tightening makes the anti-entropy story congruent with it.

---

### 4. Event admission/authentication semantics are still deployment-dependent

**Refs:** protocol event-signing section and receive-path verification rule; spec `Event` / `do_receive` omit authentication semantics entirely.

- Protocol: `doltswarm-protocol.md` lines 248-259, 294-296.
- Spec: `specs/doltswarm_verify.qnt` lines 68-75, 783-823.

**Problem.** The protocol still says:
- verify signatures if an `IdentityResolver` is configured;
- otherwise accept events best-effort.

That means event admission is deployment-dependent. Two peers can apply different admission rules to the same event stream, which weakens any strong claim that the clock and finalized history converge deterministically for all compliant peers.

The Quint model currently abstracts signatures away completely, so it silently assumes a uniform admission predicate even though the protocol text does not require one.

**Options.**

**Option A: make signature verification mandatory in the normative core.**

Pros:
- strongest convergence/security story;
- simplest operational semantics once keys are configured.

Cons:
- pushes key-distribution assumptions into the normative protocol.

**Option B (recommended): define a swarm-wide abstract `ValidEvent(e)` predicate and require every peer in one swarm to apply the same predicate.**

Pros:
- clean protocol/spec boundary;
- transport/auth implementation stays pluggable;
- easiest formalization.

Cons:
- the protocol no longer defines one concrete security profile by itself.

**Option C: keep the current best-effort mode and weaken the convergence claims.**

Pros:
- least immediate change.

Cons:
- weakest semantics;
- preserves avoidable deployment drift.

**Recommended protocol fix.**
- Replace the current `IdentityResolver`-optional wording with a swarm-level admission contract: all peers in one swarm MUST apply the same `ValidEvent(e)` predicate.
- If desired, define deployment profiles above that contract, e.g. `trusted` and `signed`.
- Keep transport/encoding/key-distribution details out of the protocol core.

**Recommended spec fix.**
- Add an abstract validity predicate or assumption.
- State explicitly that `do_receive` models only valid events.
- If you want to explore misconfiguration, make it a separate adversarial model rather than implicit behavior in the core correctness model.

**Why this should be done.**
This is mostly a semantics cleanup, but it matters. The current protocol is clearer than before on finalization, and that makes it more important that admission be equally uniform.

---

## Resolved / Mostly Resolved Items

### 5. ~~`clock` retention and GC semantics were inconsistent~~ MOSTLY FIXED

**Refs:** protocol `clock` state definition and `GC_CLOCK`; spec `gcPruneable` and `do_gc`.

- Protocol: `doltswarm-protocol.md` lines 205-216, 399-410.
- Spec: `specs/doltswarm_verify.qnt` lines 366-376, 974-1000.

**Status.** This is mostly fixed.

The protocol now explicitly defines `clock` as mixed retained state:
- all non-finalized events;
- plus retained finalized events not yet compacted.

`GC_CLOCK` and `gcPruneable` were also strengthened so a finalized event body is only pruned when no other retained event depends on it. That preserves parent-closure of the retained subgraph, and the Quint invariant suite now passes with that stronger rule.

**Remaining cleanup.** The protocol should state the retained-subgraph invariant directly in the state/invariants discussion, not only inside the `GC_CLOCK` action.

**Options.**

**Option A (recommended): keep a single mixed `clock` map and make retained-subgraph closure explicit.**

Pros:
- smallest state surface;
- aligns with the current protocol and spec;
- keeps the retained-state model simple.

Cons:
- readers still need one sentence explaining that `clock` is not "tentative only".

**Option B: split `clock` into `tentative_clock` and `retained_finalized_events`.**

Pros:
- more explicit naming.

Cons:
- extra state and invariants for little gain;
- complicates both prose and model.

**Recommended protocol fix.**
- Keep the current single-map design.
- Add one explicit invariant sentence: the retained `clock` subgraph is parent-closed.
- Keep `GC_CLOCK` as the mechanism that preserves that invariant.

**Recommended spec fix.**
- No structural change required.
- Optionally add a named invariant/comment making retained-subgraph closure visible as a first-class property, rather than only an implementation consequence of `gcPruneable`.

**Why this should be done.**
This issue is functionally closed. The remaining work is documentation clarity, not a protocol redesign.

---

## Recommended Next Order

1. Make admission semantics swarm-uniform via `ValidEvent(e)`.
2. Tighten heartbeat anti-entropy with either `closure_deficit(q)` or full seen-frontier exchange.
3. Add the retained-subgraph closure sentence to the protocol invariants/state text.
