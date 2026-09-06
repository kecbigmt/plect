import { useEffect, useRef } from "react";

import { useSessionList } from "@/lib/useSessions";
import { SessionSearch } from "@/components/session/SessionSearch";
import { SessionTree } from "@/components/session/SessionTree";

export interface SidebarProps {
  selectedName: string | null;
  expandedNames: ReadonlySet<string>;
  query: string;
  onQueryChange: (query: string) => void;
  onToggleExpanded: (sessionName: string) => void;
  onSelect: (sessionName: string) => void;
  // Owned by AppShell, above this component's own mount boundary: a
  // narrow-layout Sheet unmounts Sidebar on close, and a layout-mode change
  // swaps the persistent aside for the Sheet (or back) — a plain useState
  // here would reset to 0 across either. A ref (not state) also avoids a
  // parent re-render on every scroll tick.
  scrollTopRef: { current: number };
}

export function Sidebar({
  selectedName,
  expandedNames,
  query,
  onQueryChange,
  onToggleExpanded,
  onSelect,
  scrollTopRef,
}: SidebarProps) {
  const sessionList = useSessionList();
  const scrollContainerRef = useRef<HTMLDivElement | null>(null);

  // Runs once the list has actually rendered (not just on mount): restoring
  // scrollTop against an empty, zero-height container is a no-op the
  // browser silently drops.
  useEffect(() => {
    if (scrollContainerRef.current) {
      scrollContainerRef.current.scrollTop = scrollTopRef.current;
    }
  }, [sessionList.data, scrollTopRef]);

  return (
    <nav aria-label="Sessions" className="flex h-full flex-col gap-1 text-sm">
      <SessionSearch value={query} onChange={onQueryChange} />
      {sessionList.isPending ? (
        <p className="p-3 text-muted-foreground">Loading sessions…</p>
      ) : sessionList.isError ? (
        <p role="alert" className="p-3 text-muted-foreground">
          Couldn&rsquo;t load sessions.
        </p>
      ) : (
        <div
          ref={scrollContainerRef}
          role="region"
          aria-label="Session list"
          className="min-h-0 flex-1 overflow-auto"
          onScroll={(e) => {
            scrollTopRef.current = e.currentTarget.scrollTop;
          }}
        >
          <SessionTree
            sessions={sessionList.data}
            selectedName={selectedName}
            expandedNames={expandedNames}
            query={query}
            onToggleExpanded={onToggleExpanded}
            onSelect={onSelect}
          />
        </div>
      )}
    </nav>
  );
}
