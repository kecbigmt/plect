// Pure session-tree logic, independent of rendering: docs/design/web-ui.md
// requires the tree to come only from `parentSession` (never from splitting
// names on "/" or "+"), stable sibling ordering, and independent roots and
// missing parents to remain visible rather than hidden or invented.
import type { SessionSummary } from "@/lib/sessionsApi";

export interface SessionTreeNode {
  session: SessionSummary;
  children: SessionTreeNode[];
}

export interface SessionTreeRow {
  session: SessionSummary;
  depth: number;
  hasChildren: boolean;
  isExpanded: boolean;
}

// A parentless session's own implicit root (domain.ImplicitRootParent) is
// scoped to that one session; a session opts into being its sibling by
// naming that root explicitly as its own parentSession. Either way, the
// root itself is a pseudo-parent, never an addressable, selectable session.
function isPseudoRootParent(parentSession: string | undefined): boolean {
  return parentSession === undefined || parentSession === "" || parentSession.startsWith("root:");
}

function byNameAscending(a: SessionTreeNode, b: SessionTreeNode): number {
  return a.session.sessionName < b.session.sessionName
    ? -1
    : a.session.sessionName > b.session.sessionName
      ? 1
      : 0;
}

// buildSessionForest groups sessions by parentSession only. A session whose
// declared parent is a pseudo-root, absent, or not present among the fetched
// sessions (an orphan — the parent may exist server-side but not in this
// list, or may have been destroyed) surfaces as its own top-level row rather
// than being hidden or attached to an invented parent.
export function buildSessionForest(sessions: SessionSummary[]): SessionTreeNode[] {
  const byName = new Map(sessions.map((s) => [s.sessionName, s]));
  const childrenByParent = new Map<string, SessionTreeNode[]>();
  const roots: SessionTreeNode[] = [];

  const nodeByName = new Map<string, SessionTreeNode>();
  for (const session of sessions) {
    nodeByName.set(session.sessionName, { session, children: [] });
  }

  for (const session of sessions) {
    const node = nodeByName.get(session.sessionName)!;
    const parentSession = session.parentSession;
    if (!isPseudoRootParent(parentSession) && byName.has(parentSession!)) {
      const siblings = childrenByParent.get(parentSession!) ?? [];
      siblings.push(node);
      childrenByParent.set(parentSession!, siblings);
    } else {
      roots.push(node);
    }
  }

  function attachChildren(node: SessionTreeNode) {
    const children = (childrenByParent.get(node.session.sessionName) ?? []).sort(byNameAscending);
    node.children = children;
    for (const child of children) {
      attachChildren(child);
    }
  }
  for (const root of roots) {
    attachChildren(root);
  }

  return roots.sort(byNameAscending);
}

// ancestorNames walks real parentSession links only, root-to-target order,
// excluding the target itself. It stops (rather than inventing a node) at a
// pseudo-root parent or at a parentSession that names a session absent from
// this list.
export function ancestorNames(sessions: SessionSummary[], sessionName: string): string[] {
  const byName = new Map(sessions.map((s) => [s.sessionName, s]));
  const result: string[] = [];
  const seen = new Set<string>([sessionName]);
  let current = byName.get(sessionName);
  while (current && !isPseudoRootParent(current.parentSession)) {
    const parentName = current.parentSession!;
    if (seen.has(parentName)) {
      break; // defensive cycle guard; well-formed data never round-trips here
    }
    const parent = byName.get(parentName);
    if (!parent) {
      break; // orphan: the declared parent isn't in this list
    }
    result.unshift(parentName);
    seen.add(parentName);
    current = parent;
  }
  return result;
}

export function matchesSessionQuery(session: SessionSummary, query: string): boolean {
  const q = query.trim().toLowerCase();
  if (q === "") {
    return true;
  }
  return (
    session.sessionName.toLowerCase().includes(q) ||
    (session.resourceId ?? "").toLowerCase().includes(q)
  );
}

function subtreeMatches(node: SessionTreeNode, query: string): boolean {
  return matchesSessionQuery(node.session, query) || node.children.some((c) => subtreeMatches(c, query));
}

// visibleTreeRows flattens the forest into display order. With no query,
// only nodes in expandedNames reveal their children — expansion and
// selection stay independent, per docs/design/web-ui.md. With a query, only
// matches and their ancestors are shown, force-expanded, so a match is never
// hidden behind a collapsed row.
export function visibleTreeRows(
  forest: SessionTreeNode[],
  expandedNames: ReadonlySet<string>,
  query: string,
): SessionTreeRow[] {
  const trimmed = query.trim();
  const rows: SessionTreeRow[] = [];

  function walk(nodes: SessionTreeNode[], depth: number) {
    for (const node of nodes) {
      if (trimmed !== "" && !subtreeMatches(node, trimmed)) {
        continue;
      }
      const hasChildren = node.children.length > 0;
      const isExpanded =
        trimmed !== "" ? hasChildren : expandedNames.has(node.session.sessionName);
      rows.push({ session: node.session, depth, hasChildren, isExpanded });
      if (hasChildren && isExpanded) {
        walk(node.children, depth + 1);
      }
    }
  }
  walk(forest, 0);
  return rows;
}
