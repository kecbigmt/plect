import { useMemo, useRef, useState } from "react";
import type { KeyboardEvent } from "react";
import { ChevronDownIcon, ChevronRightIcon } from "lucide-react";

import { cn } from "@/lib/utils";
import { buildSessionForest, visibleTreeRows, type SessionTreeRow } from "@/lib/sessionTree";
import type { SessionSummary } from "@/lib/sessionsApi";

export interface SessionTreeProps {
  sessions: SessionSummary[];
  selectedName: string | null;
  expandedNames: ReadonlySet<string>;
  query: string;
  onToggleExpanded: (sessionName: string) => void;
  onSelect: (sessionName: string) => void;
}

// health and run are independent dimensions (a session can be up and
// unhealthy), so unhealthy overrides the dot's color for that one negative
// case; every other combination just reflects run.
function dotClass(session: SessionSummary): string {
  if (session.health === "unhealthy") {
    return "bg-destructive";
  }
  return session.run === "up" ? "bg-green-500" : "bg-muted-foreground/40";
}

// Keyboard support follows the WAI-ARIA treeview pattern's roving tabindex,
// scoped to one focusable control per row: the separate expand/collapse
// button stays reachable by click but out of the Tab sequence, since
// ArrowLeft/ArrowRight already reach the same action from the row itself.
export function SessionTree({
  sessions,
  selectedName,
  expandedNames,
  query,
  onToggleExpanded,
  onSelect,
}: SessionTreeProps) {
  const forest = useMemo(() => buildSessionForest(sessions), [sessions]);
  const rows = useMemo(
    () => visibleTreeRows(forest, expandedNames, query),
    [forest, expandedNames, query],
  );
  const [focusedName, setFocusedName] = useState<string | null>(null);
  const rowRefs = useRef(new Map<string, HTMLButtonElement>());

  const rowIndex = new Map(rows.map((r, i) => [r.session.sessionName, i]));
  const focusable =
    (focusedName && rowIndex.has(focusedName) && focusedName) ||
    (selectedName && rowIndex.has(selectedName) && selectedName) ||
    rows[0]?.session.sessionName ||
    null;

  function moveFocusTo(row: SessionTreeRow | undefined) {
    if (!row) {
      return;
    }
    setFocusedName(row.session.sessionName);
    rowRefs.current.get(row.session.sessionName)?.focus();
  }

  function handleKeyDown(event: KeyboardEvent<HTMLButtonElement>, row: SessionTreeRow) {
    const index = rowIndex.get(row.session.sessionName)!;
    switch (event.key) {
      case "ArrowDown":
        event.preventDefault();
        moveFocusTo(rows[index + 1]);
        break;
      case "ArrowUp":
        event.preventDefault();
        moveFocusTo(rows[index - 1]);
        break;
      case "ArrowRight":
        event.preventDefault();
        if (row.hasChildren && !row.isExpanded) {
          onToggleExpanded(row.session.sessionName);
        } else if (row.hasChildren) {
          moveFocusTo(rows[index + 1]);
        }
        break;
      case "ArrowLeft":
        event.preventDefault();
        if (row.hasChildren && row.isExpanded) {
          onToggleExpanded(row.session.sessionName);
        } else if (row.depth > 0) {
          for (let i = index - 1; i >= 0; i--) {
            if (rows[i].depth === row.depth - 1) {
              moveFocusTo(rows[i]);
              break;
            }
          }
        }
        break;
      case "Home":
        event.preventDefault();
        moveFocusTo(rows[0]);
        break;
      case "End":
        event.preventDefault();
        moveFocusTo(rows[rows.length - 1]);
        break;
      case "Enter":
      case " ":
        event.preventDefault();
        onSelect(row.session.sessionName);
        break;
      default:
        break;
    }
  }

  if (sessions.length === 0) {
    return <p className="p-3 text-sm text-muted-foreground">No sessions yet.</p>;
  }
  if (rows.length === 0) {
    return <p className="p-3 text-sm text-muted-foreground">No sessions match &ldquo;{query}&rdquo;.</p>;
  }

  return (
    <div role="tree" aria-label="Sessions" className="flex flex-col py-1">
      {rows.map((row) => {
        const name = row.session.sessionName;
        const isSelected = name === selectedName;
        return (
          <div
            key={name}
            className={cn(
              "flex items-center gap-1 rounded-sm pr-2 text-sm",
              isSelected && "bg-muted",
            )}
            style={{ paddingLeft: 4 + row.depth * 16 }}
          >
            <button
              type="button"
              tabIndex={-1}
              disabled={!row.hasChildren}
              aria-label={`${row.isExpanded ? "Collapse" : "Expand"} ${name}`}
              onClick={() => onToggleExpanded(name)}
              className="flex size-5 shrink-0 items-center justify-center rounded-sm text-muted-foreground hover:bg-muted disabled:opacity-0"
            >
              {row.hasChildren ? (
                row.isExpanded ? (
                  <ChevronDownIcon className="size-3.5" />
                ) : (
                  <ChevronRightIcon className="size-3.5" />
                )
              ) : null}
            </button>
            <button
              type="button"
              role="treeitem"
              ref={(el) => {
                if (el) {
                  rowRefs.current.set(name, el);
                } else {
                  rowRefs.current.delete(name);
                }
              }}
              aria-level={row.depth + 1}
              aria-expanded={row.hasChildren ? row.isExpanded : undefined}
              aria-selected={isSelected}
              tabIndex={name === focusable ? 0 : -1}
              onKeyDown={(e) => handleKeyDown(e, row)}
              onClick={() => onSelect(name)}
              title={name}
              className="flex min-w-0 flex-1 items-center justify-between gap-2 rounded-sm py-1 text-left outline-none focus-visible:ring-2 focus-visible:ring-ring"
            >
              <span className="min-w-0 truncate">{name}</span>
              <span
                aria-hidden="true"
                className={cn("size-1.5 shrink-0 rounded-full", dotClass(row.session))}
              />
            </button>
          </div>
        );
      })}
    </div>
  );
}
