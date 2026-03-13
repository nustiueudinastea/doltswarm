// -*- mode: Bluespec; -*-
/**
 * This module collects definitions that are ubiquitous.
 * One day they will become the standard library of Quint.
 */
module basicSpells {
  /// Option type, which may hold some value or none
  type Option[a] = Some(a) | None

  /// An annotation for writing preconditions.
  /// - @param cond condition to check
  /// - @returns true if and only if cond evaluates to true
  pure def require(cond: bool): bool = cond

  /// Compute the maximum of two integers.
  pure def max(i: int, j: int): int = {
    if (i > j) i else j
  }

  /// Compute the minimum of two integers.
  pure def min(i: int, j: int): int = {
    if (i < j) i else j
  }

  /// Compute the absolute value of an integer
  pure def abs(i: int): int = {
    if (i < 0) -i else i
  }

  /// Remove a set element.
  pure def setRemove(s: Set[a], elem: a): Set[a] = {
    s.exclude(Set(elem))
  }

  /// Adds an element to a set.
  pure def setAdd(s: Set[a], elem: a): Set[a] = {
    s.union(Set(elem))
  }

  /// Test whether a key is present in a map
  pure def has(m: a -> b, key: a): bool = {
    m.keys().contains(key)
  }

  /// Get the map value associated with a key, or the default,
  /// if the key is not present.
  pure def getOrElse(m: a -> b, key: a, default: b): b = {
    if (m.has(key)) {
      m.get(key)
    } else {
      default
    }
  }

  /// Remove a map entry.
  pure def mapRemove(m: a -> b, key: a): a -> b = {
    m.keys().setRemove(key).mapBy(k => m.get(k))
  }

  /// Removes a set of map entries.
  pure def mapRemoveAll(m: a -> b, ks: Set[a]): a -> b = {
    m.keys().exclude(ks).mapBy(k => m.get(k))
  }

  /// Get the set of values of a map.
  pure def values(m: a -> b): Set[b] = {
    m.keys().map(k => m.get(k))
  }

  /// Whether a set is empty
  pure def empty(s: Set[a]): bool = s == Set()

  /// Sort a list, given the ordering operator.
  pure def sortList(list: List[a], lt: (a, a) => bool): List[a] = {
    pure def insertInOrder(sortedList: List[a], num: a): List[a] = {
      val insertion = sortedList.foldl(([], false), (acc, current) => {
        if (acc._2) {
          (acc._1.append(current), true)
        } else if (lt(current, num)) {
          (acc._1.append(current), false)
        } else {
          (acc._1.append(num).append(current), true)
        }
      })
      if (insertion._2) insertion._1 else insertion._1.append(num)
    }

    list.foldl([], (sortedList, num) => insertInOrder(sortedList, num))
  }

  /// Apply an operator to all values of a map
  pure def transformValues(m: a -> b, f: (b) => c): a -> c = {
    m.keys().mapBy(k => f(m.get(k)))
  }

  /// map a function over a list
  pure def listMap(l: List[a], f: (a) => b): List[b] = {
    range(0, l.length()).foldl([], (acc, i) => {
      acc.append(f(l[i]))
    })
  }

  /// The last element of a list
  pure def last(v: List[a]): a = {
    v[v.length() - 1]
  }

  /// `findFirst(l, f)` is the first element of `l` that satisfies the predicate `f`, or None
  pure def findFirst(l, f) = l.foldl(None, (a, i) => if (a == None and f(i)) Some(i) else a)

  /// `find(s, f)` is an element of `s` that satisfies the predicate `f`, or None
  pure def find(s, f) = s.fold(None, (a, i) => if (f(i)) Some(i) else a)

  /// `setByWithDefault(m, k, op, default)` updates m[k] with op, using default if absent
  pure def setByWithDefault(m: a -> b, k: a, op: (b) => b, default: b): a -> b = {
    if (m.has(k))
      m.setBy(k, op)
    else
      m.put(k, default).setBy(k, op)
  }

  pure def unwrap(value: Option[a]): a = {
    match value {
      | None => Map().get(value)
      | Some(x) => x
    }
  }

  pure def filterMap(s: Set[a], f: (a) => Option[b]): Set[b] = {
    s.fold(Set(), (acc, e) => {
      match f(e) {
        | Some(x) => acc.union(Set(x))
        | None => acc
      }
    })
  }

  pure def isPrefixOf(l1: List[a], l2: List[a]): bool = {
    if (l1.length() > l2.length()) {
      false
    } else {
      l1.indices().forall(i => l1[i] == l2[i])
    }
  }

  pure def takeWhile(l: List[a], cond: (a) => bool): List[a] = {
    pure val result = l.foldl(([], true), (acc, e) => {
      if (acc._2 and cond(e)) {
        (acc._1.append(e), true)
      } else {
        (acc._1, false)
      }
    })
    result._1
  }

  pure def decreasingRange(start: int, end: int): List[int] = {
    range(end, start + 1).foldl([], (acc, i) => {
      List(i).concat(acc)
    })
  }
}
