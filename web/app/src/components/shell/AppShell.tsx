import { useState } from "react";
import { PanelLeftIcon, PanelRightIcon } from "lucide-react";

import type { BootstrapInfo } from "@/lib/bootstrap";
import { useMediaQuery } from "@/lib/useMediaQuery";
import { useSessionList } from "@/lib/useSessions";
import { ancestorNames } from "@/lib/sessionTree";
import { Button } from "@/components/ui/button";
import { Sheet, SheetContent, SheetHeader, SheetTitle } from "@/components/ui/sheet";
import { Sidebar } from "@/components/shell/Sidebar";
import { DetailPane } from "@/components/shell/DetailPane";
import { SessionHeader } from "@/components/session/SessionHeader";

// Narrower than this, the sidebar and detail pane move into an overlay
// instead of a persistent column (docs/design/web-ui.md: "Narrow screens use
// an overlay or temporary main-area detail view").
const WIDE_LAYOUT_QUERY = "(min-width: 768px)";

export function AppShell({ bootstrap }: { bootstrap: BootstrapInfo }) {
  const isWide = useMediaQuery(WIDE_LAYOUT_QUERY);
  const [sidebarOpen, setSidebarOpen] = useState(false);
  // Closed initially, per docs/design/web-ui.md's shared-details contract.
  const [detailOpen, setDetailOpen] = useState(false);

  // Owned here, above the sidebar's own mount boundary (a persistent aside
  // on wide layouts, a Sheet's portal content on narrow ones — the two
  // never coexist, and either can unmount on a layout change): selection,
  // expansion, and search must survive both, per docs/design/web-ui.md's
  // "Preserve expansion and scroll position."
  const [selectedName, setSelectedName] = useState<string | null>(null);
  const [expandedNames, setExpandedNames] = useState<ReadonlySet<string>>(new Set());
  const [query, setQuery] = useState("");
  // Shares its cache with Sidebar's own useSessionList() call (same query
  // key) — this is only for computing the ancestor chain to expand, not a
  // second fetch.
  const sessionList = useSessionList();

  function selectSession(name: string) {
    setSelectedName(name);
    // "Opening a session through another view expands its ancestors"
    // (docs/design/web-ui.md) — every selection path funnels through here,
    // so this holds regardless of how the session was reached.
    const ancestors = ancestorNames(sessionList.data ?? [], name);
    if (ancestors.length > 0) {
      setExpandedNames((prev) => new Set([...prev, ...ancestors]));
    }
    if (!isWide) {
      setSidebarOpen(false);
    }
  }

  function toggleExpanded(name: string) {
    setExpandedNames((prev) => {
      const next = new Set(prev);
      if (next.has(name)) {
        next.delete(name);
      } else {
        next.add(name);
      }
      return next;
    });
  }

  const sidebar = (
    <Sidebar
      selectedName={selectedName}
      expandedNames={expandedNames}
      query={query}
      onQueryChange={setQuery}
      onToggleExpanded={toggleExpanded}
      onSelect={selectSession}
    />
  );
  const detailPane = <DetailPane sessionName={selectedName} />;

  return (
    <div className="flex h-dvh flex-col">
      <header className="flex items-center justify-between border-b border-border px-3 py-2">
        <div className="flex items-center gap-2">
          {!isWide && (
            <Button
              variant="ghost"
              size="icon"
              aria-label="Open sessions"
              onClick={() => setSidebarOpen(true)}
            >
              <PanelLeftIcon className="size-4" />
            </Button>
          )}
          <span className="font-medium">Plecture</span>
          <span className="text-xs text-muted-foreground">API {bootstrap.apiVersion}</span>
        </div>
        <Button
          variant="outline"
          aria-expanded={detailOpen}
          aria-label="Toggle details"
          onClick={() => setDetailOpen((open) => !open)}
        >
          <PanelRightIcon className="size-4" />
          Details
        </Button>
      </header>

      <div className="flex min-h-0 flex-1">
        {isWide && (
          <aside className="w-56 shrink-0 border-r border-border">{sidebar}</aside>
        )}
        <main className="flex min-w-0 flex-1 flex-col overflow-auto">
          {selectedName ? (
            <SessionHeader sessionName={selectedName} onOpenDetails={() => setDetailOpen(true)} />
          ) : (
            <p className="p-4 text-sm text-muted-foreground">Select a session to view it.</p>
          )}
        </main>
        {isWide && detailOpen && (
          <aside className="w-72 shrink-0 border-l border-border" aria-label="Details">
            {detailPane}
          </aside>
        )}
      </div>

      {!isWide && (
        <Sheet open={sidebarOpen} onOpenChange={setSidebarOpen}>
          <SheetContent>
            <SheetHeader>
              <SheetTitle>Sessions</SheetTitle>
            </SheetHeader>
            {sidebar}
          </SheetContent>
        </Sheet>
      )}
      {!isWide && (
        <Sheet open={detailOpen} onOpenChange={setDetailOpen}>
          <SheetContent>
            <SheetHeader>
              <SheetTitle>Details</SheetTitle>
            </SheetHeader>
            {detailPane}
          </SheetContent>
        </Sheet>
      )}
    </div>
  );
}
