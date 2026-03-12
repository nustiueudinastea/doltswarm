# Quint Invariant Benchmarks

- Spec: `doltswarm_verify.qnt`
- Benchmark workload: `--max-samples=1000 --max-steps=20`
- Default suite sample count for estimates: `10000`
- Per-invariant target runtime budget: `5.0s`
- `Bench engine s` and the estimated columns use Quint's reported simulator time.
- `Bench wall s` includes process startup and output handling overhead.
- Runtime estimates assume near-linear scaling with samples and max steps.

| Invariant | Bench engine s | Bench wall s | Est engine s @ 10k x 20 | Default steps | Est engine s @ 10k x default |
| --- | ---: | ---: | ---: | ---: | ---: |
| `inv_retained_structure_convergence` | 0.27 | 3.95 | 2.71 | 20 | 2.71 |
| `inv_finalized_commit_id_matches_state` | 0.23 | 4.20 | 2.33 | 20 | 2.33 |
| `inv_finalized_agreement` | 0.26 | 4.01 | 2.62 | 20 | 2.62 |
| `inv_lineage_well_formed` | 0.41 | 4.39 | 4.14 | 20 | 4.14 |
| `inv_lineage_closure` | 0.46 | 4.44 | 4.58 | 20 | 4.58 |
| `inv_lineage_lca_sound` | 1.56 | 5.61 | 15.64 | 6 | 4.69 |
| `inv_sync_metadata_union` | 0.31 | 4.30 | 3.08 | 20 | 3.08 |
| `inv_sync_lineage_union_monotone` | 0.30 | 4.29 | 3.04 | 20 | 3.04 |
| `inv_sync_merge_parent_edges` | 0.30 | 4.28 | 2.98 | 20 | 2.98 |
| `inv_sync_merge_frontier_reduction` | 0.31 | 3.99 | 3.06 | 20 | 3.06 |
| `inv_sync_nested_merge_reconcilable` | 0.37 | 4.47 | 3.67 | 20 | 3.67 |
| `inv_sync_contract_adopt` | 0.30 | 4.31 | 3.03 | 20 | 3.03 |
| `inv_sync_empty_root_adopts` | 0.33 | 4.01 | 3.26 | 20 | 3.26 |
| `inv_sync_contract_noop` | 0.32 | 4.33 | 3.20 | 20 | 3.20 |
| `inv_sync_equal_tip_metadata_repair` | 0.29 | 3.96 | 2.91 | 20 | 2.91 |
| `inv_sync_noop_retained_finalized_clean` | 0.31 | 4.40 | 3.13 | 20 | 3.13 |
| `inv_sync_contract_merge` | 0.30 | 4.33 | 3.03 | 20 | 3.03 |
| `inv_sync_unresolved_conflicts_monotone` | 0.33 | 4.31 | 3.25 | 20 | 3.25 |
| `inv_sync_conflict_recorded` | 0.30 | 3.97 | 3.02 | 20 | 3.02 |
| `inv_sync_conflict_id_symmetric` | 0.33 | 4.31 | 3.25 | 20 | 3.25 |
| `inv_resolve_finalized_conflict_contract` | 0.23 | 4.23 | 2.35 | 20 | 2.35 |
| `inv_sync_subset_roundtrip_converges` | 0.34 | 4.32 | 3.35 | 20 | 3.35 |
| `inv_sync_incomparable_pairwise_converges` | 0.31 | 4.30 | 3.09 | 20 | 3.09 |
| `inv_fullmesh_heal_round_frontier_nonincreasing` | 0.37 | 4.06 | 3.73 | 20 | 3.73 |
| `inv_fullmesh_frontier_rank_strict_progress` | 0.30 | 4.32 | 2.96 | 20 | 2.96 |
| `inv_heal_frontier_growth_bounded` | 0.32 | 4.00 | 3.21 | 20 | 3.21 |
| `inv_frontier_merge_strictly_decreases` | 0.32 | 4.02 | 3.19 | 20 | 3.19 |
| `inv_fullmesh_heal_bounded_converges` | 0.54 | 4.53 | 5.38 | 19 | 5.11 |
| `inv_component_converged_frontier_absorbing` | 0.32 | 4.35 | 3.23 | 20 | 3.23 |
| `inv_fullmesh_unique_frontier_round_converges` | 0.38 | 4.41 | 3.79 | 20 | 3.79 |
| `inv_converged_finalized_state_absorbing` | 0.35 | 4.35 | 3.52 | 20 | 3.52 |
| `inv_fullmesh_heal_converged_absorbing` | 0.51 | 4.19 | 5.10 | 20 | 5.10 |
| `inv_data_convergence` | 0.39 | 4.42 | 3.91 | 20 | 3.91 |
| `inv_causal_consistency` | 0.23 | 4.24 | 2.33 | 20 | 2.33 |
| `inv_no_silent_data_loss` | 0.23 | 4.24 | 2.33 | 20 | 2.33 |
| `inv_finalized_events_derived_from_checkpoints` | 0.40 | 4.09 | 3.96 | 20 | 3.96 |
| `inv_write_preserves_finalized_state` | 4.82 | 8.51 | 48.19 | 3 | 7.23 |
| `inv_reconcile_preserves_finalized_state` | 0.28 | 3.96 | 2.81 | 20 | 2.81 |
| `inv_finalize_linear_extension_only` | 0.54 | 4.60 | 5.35 | 19 | 5.08 |
| `inv_finalize_one_event_per_checkpoint` | 0.54 | 4.22 | 5.40 | 19 | 5.13 |
| `inv_resolve_conflict_contract` | 0.30 | 4.01 | 3.03 | 20 | 3.03 |
| `inv_clock_events_valid` | 0.26 | 4.28 | 2.58 | 20 | 2.58 |
| `inv_inbox_events_valid` | 0.29 | 3.97 | 2.94 | 20 | 2.94 |
| `inv_event_root_not_conflict` | 0.25 | 4.29 | 2.48 | 20 | 2.48 |
| `inv_merge_disjoint_commutative` | 0.64 | 4.34 | 6.40 | 16 | 5.12 |
| `inv_no_conflict_tentative_root` | 0.22 | 4.33 | 2.23 | 20 | 2.23 |
| `inv_no_conflict_finalized_prolly_root` | 0.24 | 4.26 | 2.40 | 20 | 2.40 |
| `inv_conflict_visibility` | 0.24 | 3.91 | 2.39 | 20 | 2.39 |
| `inv_finalized_conflict_visibility` | 0.23 | 3.92 | 2.30 | 20 | 2.30 |
| `inv_finalized_conflict_authenticity` | 0.24 | 4.22 | 2.36 | 20 | 2.36 |
| `inv_finalized_conflict_no_resurrection` | 0.24 | 4.27 | 2.41 | 20 | 2.41 |
| `inv_chunks_ready_known` | 0.23 | 4.23 | 2.31 | 20 | 2.31 |
| `inv_self_in_active_peers` | 0.23 | 3.91 | 2.34 | 20 | 2.34 |
| `inv_parked_are_heads` | 0.24 | 4.23 | 2.43 | 20 | 2.43 |
| `inv_parked_mergeable_when_closure_ready` | 0.44 | 4.11 | 4.42 | 20 | 4.42 |
| `inv_retained_finalized_subgraph_parent_closed` | 0.24 | 4.24 | 2.41 | 20 | 2.41 |
| `inv_finalized_parent_closed` | 0.24 | 3.93 | 2.45 | 20 | 2.45 |
| `inv_tentative_root_is_compute_projection` | 0.40 | 4.08 | 3.98 | 20 | 3.98 |
| `inv_parked_conflicts_is_compute_projection` | 0.39 | 4.39 | 3.93 | 20 | 3.93 |
| `inv_projection_basis_is_compute_projection` | 0.37 | 4.06 | 3.73 | 20 | 3.73 |
| `inv_heartbeat_closure_catchup_bounded` | 0.68 | 4.38 | 6.81 | 15 | 5.11 |
| `inv_reanchor_provisional_parked_retained` | 0.31 | 4.00 | 3.09 | 20 | 3.09 |
| `inv_reanchor_after_closure_lag` | 0.29 | 4.30 | 2.94 | 20 | 2.94 |
| `inv_heartbeat_no_redundant_pull_when_closed` | 0.47 | 4.47 | 4.69 | 20 | 4.69 |
| `inv_heartbeat_refreshes_remote_witness_exactly` | 0.33 | 4.34 | 3.30 | 20 | 3.30 |
| `inv_self_seen_frontier_exact` | 0.41 | 4.10 | 4.12 | 20 | 4.12 |
| `inv_self_seen_frontier_antichain` | 0.30 | 3.98 | 3.01 | 20 | 3.01 |
| `inv_self_seen_frontier_covers_parent_closed_clock` | 0.53 | 4.53 | 5.25 | 19 | 4.99 |
| `inv_bootstrap_boundary_ready` | 0.64 | 4.32 | 6.40 | 16 | 5.12 |
| `inv_singleton_membership_self_coverage` | 0.27 | 4.26 | 2.67 | 20 | 2.67 |
| `inv_write_enabled_despite_conflicts` | 0.29 | 4.27 | 2.87 | 20 | 2.87 |
| `inv_rejoin_does_not_rewind_finalized` | 0.27 | 4.26 | 2.68 | 20 | 2.68 |
| `inv_parking_agreement` | 0.50 | 4.48 | 4.96 | 20 | 4.96 |
| `inv_heads_consistent` | 0.24 | 4.25 | 2.39 | 20 | 2.39 |
| `inv_parked_subsumed_disjoint` | 0.26 | 3.97 | 2.62 | 20 | 2.62 |
| `inv_subsumed_parent_link` | 0.25 | 4.24 | 2.46 | 20 | 2.46 |
| `inv_subsumed_finalize_bookkeeping_only` | 0.24 | 4.21 | 2.38 | 20 | 2.38 |
| `inv_finalized_parked_disjoint` | 0.24 | 3.89 | 2.43 | 20 | 2.43 |
| `inv_three_peer_partition_heal_reconcilable` | 0.35 | 4.01 | 3.46 | 20 | 3.46 |
