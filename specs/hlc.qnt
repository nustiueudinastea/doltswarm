// -*- mode: Bluespec; -*-

/**
 * Hybrid Logical Clock (HLC) model for DoltSwarm.
 *
 * Provides a total order over distributed events using (wall, logical, peer)
 * tuples. This is the foundation for deterministic event ordering.
 *
 * Abstraction: wall times are small integers (1..MAX_WALL) rather than
 * nanoseconds. The ordering math is identical.
 */
module hlc {
  // PeerID is an integer for model-checking (enables comparison).
  // In the real system this is a string; using int preserves the total order.
  type PeerID = int

  type HLC = {
    wall: int,
    logical: int,
    peer: PeerID,
  }

  pure val ZERO: HLC = { wall: 0, logical: 0, peer: 0 }

  /// Total order: (wall, logical, peer) lexicographic comparison.
  pure def less(a: HLC, b: HLC): bool =
    if (a.wall != b.wall) a.wall < b.wall
    else if (a.logical != b.logical) a.logical < b.logical
    else a.peer < b.peer

  /// Equality
  pure def hlcEq(a: HLC, b: HLC): bool =
    a.wall == b.wall and a.logical == b.logical and a.peer == b.peer

  /// Less than or equal
  pure def leq(a: HLC, b: HLC): bool =
    hlcEq(a, b) or less(a, b)

  /// Maximum of two HLCs
  pure def hlcMax(a: HLC, b: HLC): HLC =
    if (less(a, b)) b else a

  /// Minimum of two HLCs
  pure def hlcMin(a: HLC, b: HLC): HLC =
    if (less(a, b)) a else b

  /// Is this the zero HLC?
  pure def isZero(h: HLC): bool = hlcEq(h, ZERO)

  /// Tick: advance the local clock for a new local event.
  /// Returns a new HLC that is strictly greater than the current clock.
  pure def tick(clock: HLC, localWall: int, peer: PeerID): HLC =
    if (localWall > clock.wall)
      { wall: localWall, logical: 0, peer: peer }
    else
      { wall: clock.wall, logical: clock.logical + 1, peer: peer }

  /// Observe/merge: merge local clock with a received remote timestamp.
  /// Returns a new HLC that is strictly greater than both.
  pure def observe(clock: HLC, remote: HLC, localWall: int, peer: PeerID): HLC = {
    val maxWall = if (localWall >= remote.wall and localWall >= clock.wall) localWall
                  else if (remote.wall >= clock.wall) remote.wall
                  else clock.wall
    val newLogical =
      if (maxWall > clock.wall and maxWall > remote.wall) 0
      else if (maxWall == clock.wall and maxWall == remote.wall) {
        if (clock.logical > remote.logical) clock.logical + 1
        else remote.logical + 1
      }
      else if (maxWall == clock.wall) clock.logical + 1
      else remote.logical + 1
    { wall: maxWall, logical: newLogical, peer: peer }
  }

  /// String key for an HLC (used for dedup maps).
  pure def hlcKey(h: HLC): (int, int, int) = (h.wall, h.logical, h.peer)

  /// Minimum HLC over a map of peers (for stable_hlc computation).
  /// Returns a sentinel max if the set is empty.
  pure val HLC_SENTINEL: HLC = { wall: 999999, logical: 999, peer: 999 }

  pure def hlcMinOver(m: PeerID -> HLC, peers: Set[PeerID]): HLC =
    peers.fold(HLC_SENTINEL, (acc, p) =>
      if (m.keys().contains(p) and less(m.get(p), acc)) m.get(p) else acc
    )
}
