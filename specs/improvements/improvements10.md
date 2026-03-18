# DoltSwarm Protocol & Spec Improvements (Round 10, Consolidated)

Consolidated findings after investigating the cloud regressions around retained finalized event bodies and tentative convergence.

This file reflects the implemented state in:
- `doltswarm-protocol.md`
- `specs/doltswarm_verify.qnt`

It supersedes the earlier assumption that the retained clock plus `finalized_prolly_root_hash` is sufficient to determine the tentative projection.

## Outcome Summary

Round 10 closes a tentative/finalized boundary mismatch that surfaced in two stages:
- retained finalized event bodies could become structural heads after compaction and re-enter tentative parking,
- and, after that boundary bug was fixed, convergence checks were still stated against inputs that were too weak to determine the tentative projection.

The important result is not that finalized history semantics changed. The real correction is that the protocol and the spec now agree on a sharper local rule:
- retained finalized event bodies may remain in `clock` for closure and bookkeeping,
- but `RECONCILE` operates only on unsettled tentative heads, `head_event_ids \ finalized_events.keys()`,
- the tentative projection depends on both the finalized content anchor and the derived finalized-event catalog,
- and closure-ready / re-anchoring checks must reason about tentative heads, not raw structural heads.

## Decisions Applied In This Round

- Retained finalized event bodies remain below the tentative boundary even if they appear in `head_event_ids` after `GC_CLOCK`.
- `projection_basis` is now explicitly an unsettled tentative basis; finalized retained ids cannot linger in it.
- Tentative convergence and parking agreement are now defined in terms of the full tentative-projection inputs:
  - `clock`
  - `chunks_ready`
  - `finalized_prolly_root_hash`
  - `finalized_events`
- Closure-lag and re-anchoring regressions now use tentative heads instead of raw structural heads.
- The protocol text now matches the spec's post-fix meaning of `RECONCILE`.

## Resolved Items

### 1. ~~Retained finalized event bodies could re-enter tentative reconciliation after compaction~~ FIXED

**Refs:** `RECONCILE` state definitions and boundary rules, tentative projection helper, finalized/tentative boundary invariants.

- Protocol: `doltswarm-protocol.md` lines 298-299, 335-336, 435-446, 559, 708, 919.
- Spec: `specs/doltswarm_verify.qnt` lines 411-421, 516-535, 3044-3050, 3406-3411.

**Status.** Fixed by making tentative reconciliation operate on `tentativeHeadEventIDs(st) = st.head_event_ids \ st.finalized_events.keys()`.

**Problem in the earlier design.**

The old model treated raw structural `head_event_ids` as reconcile inputs. That was wrong once `GC_CLOCK` compacted newer finalized descendants and left an older finalized retained body as a structural head:
- the finalized event body was still present in `clock`,
- it could reappear in `head_event_ids`,
- and `RECONCILE` could then re-park or re-project an event that was already hardened into finalized history.

That violated the protocol's core boundary:
- finalized state may be retained locally for bookkeeping,
- but it must not fall back into live tentative merge/parking.

**Implemented protocol behavior.**

- `head_event_ids` is now documented as a structural set that may contain retained finalized bodies.
- `RECONCILE` now defines `tentative_head_event_ids = head_event_ids \ finalized_events.keys()`.
- Closure-lag retention and `projection_basis` preservation are now described in terms of tentative heads only.
- The protocol adds an explicit disjointness rule: finalized ids never appear in `projection_basis`.

**Implemented spec behavior.**

- Added `tentativeHeadEventIDs(st)`.
- `mergeableHeadEventIDs(st)` now filters tentative heads, not raw heads.
- `computeTentativeProjection(st)` now:
  - returns the finalized root when no tentative heads remain,
  - retains only parked/projection state intersected with tentative heads under closure lag,
  - computes the live basis only from mergeable tentative heads.
- Added `inv_finalized_projection_basis_disjoint`.

**Why this closes the issue.**

Retained finalized bodies can still exist locally, but they are no longer eligible to:
- be re-merged,
- be re-parked,
- or reappear in the live projection basis.

That restores the intended one-way boundary from tentative state into finalized bookkeeping.

### 2. ~~Data convergence and parking agreement were specified against inputs too weak to determine tentative projection~~ FIXED

**Refs:** data-convergence / parking-agreement invariants and the protocol convergence statements.

- Protocol: `doltswarm-protocol.md` lines 336, 676, 691, 919.
- Spec: `specs/doltswarm_verify.qnt` lines 1193-1200, 2741-2752, 3113-3158, 3311-3322.

**Status.** Fixed by defining and using explicit tentative-projection inputs.

**Problem in the earlier design.**

After excluding finalized ids from tentative heads, two peers could legitimately have:
- the same `clock`,
- the same `chunks_ready`,
- the same `finalized_prolly_root_hash`,
- but different `finalized_events`.

That difference matters because `finalized_events` determines which structural heads are still unsettled tentative heads. So the old convergence antecedents were too weak: equal finalized root no longer implied equal tentative projection inputs.

**Implemented protocol behavior.**

- Tentative state is now described as depending on the current finalized boundary inputs:
  - finalized content anchor (`finalized_prolly_root_hash`)
  - derived finalized event catalog (`finalized_events`)
- `INV2` now states convergence for peers with the same tentative-projection inputs.
- `INV7d` now states parking agreement for the same inputs once all tentative heads are mergeable.
- The silent-merge explanatory text now uses the same definition.

**Implemented spec behavior.**

- Added `sameTentativeProjectionInputs(s1, s2)`.
- `inv_data_convergence` now uses `sameTentativeProjectionInputs(...)`.
- `inv_parking_agreement` now uses the same helper and closure-ready tentative-head equality.
- Re-anchoring and closure-ready regressions now compare against tentative heads rather than raw `head_event_ids`.

**Why this closes the issue.**

The spec and the protocol now talk about the same function with the same inputs. Bookkeeping-only finalization can change `finalized_events` without changing `finalized_prolly_root_hash`, and the convergence language now reflects that.

### 3. ~~The explanatory silent-reconcile text had drifted from the updated boundary semantics~~ FIXED

**Refs:** protocol and spec explanatory text for silent merges.

- Protocol: `doltswarm-protocol.md` line 919.
- Spec: `specs/doltswarm_verify.qnt` lines 1643-1647.

**Status.** Fixed by rewriting the silent-merge commentary to use the same tentative-projection-input definition as the operational rules.

**Problem in the earlier design.**

The normative sections had already been updated, but the explanatory silent-reconcile comments were still using the older mental model:
- the protocol paragraph said that `RECONCILE` was derived entirely from the Merkle clock and `finalized_prolly_root_hash`,
- and the spec commentary still said that "the same event set" implied the same tentative result.

That was stale after the finalized-bookkeeping fix. It under-described both:
- the role of `finalized_events`, and
- the role of `chunks_ready` / closure lag in the local projection.

**Implemented behavior.**

The commentary now states that `RECONCILE` is derived from:
- retained `clock`,
- `chunks_ready`,
- `finalized_prolly_root_hash`,
- and the locally derived `finalized_events` catalog,

with tentative-head exclusion and closure-lag behavior described explicitly.

**Why this closes the issue.**

The Motivation section, the operational `RECONCILE` rules, the invariants, and the explanatory text now describe the same boundary model.

## Validation Run

This round was driven by two cloud-result investigations:
- `specs/cloud-results/20260312-191425`
- `specs/cloud-results/20260313-111034`

Observed progression:
- the original cloud failure was `inv_finalized_parked_disjoint`,
- after the tentative-head boundary fix, that failure passed,
- the remaining cloud failure was `inv_data_convergence`,
- that second failure turned out to be a stale convergence invariant rather than a reappearance of the original bug.

Targeted local checks that passed after the consolidated fix:
- `inv_finalized_parked_disjoint`
- `inv_finalized_projection_basis_disjoint`
- `inv_data_convergence`
- `inv_parking_agreement`
- `inv_parked_mergeable_when_closure_ready`
- `inv_reanchor_after_closure_lag`
- `inv_reanchor_provisional_parked_retained`

The targeted runs used the Rust backend with up to `50000` samples; the deeper finalized-parked regression was checked at `50` steps and the rest at `20` steps unless otherwise noted during investigation.

## Remaining Follow-Up

Round 10 closes the known tentative/finalized boundary mismatch and the related stale convergence statements.

The next sensible follow-up is:
- rerun the full cloud suite from the current tree so the bounded regression set reflects the consolidated post-fix state rather than the intermediate one-invariant failure snapshot.
