import { useRef, useState } from "react";
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
import { Conversation } from "@/components/session/Conversation";

// Narrower than this, the sidebar and detail pane move into an overlay
// instead of a persistent column (docs/design/web-ui.md: "Narrow screens use
// an overlay or temporary main-area detail view").
const WIDE_LAYOUT_QUERY = "(min-width: 768px)";

export function AppShell({ bootstrap }: { bootstrap: BootstrapInfo }) {
  const isWide = useMediaQuery(WIDE_LAYOUT_QUERY);
  const [sidebarOpen, setSidebarOpen] = useState(false);
  // Closed initially, per docs/design/web-ui.md's shared-details contract.
  const [detailOpen, setDetailOpen] = useState(false);

  // Owned here rather than inside Sidebar: a persistent aside (wide layout)
  // and a Sheet's portal content (narrow layout) never coexist, and either
  // can unmount on a layout change, which would reset state owned below it.
  const [selectedName, setSelectedName] = useState<string | null>(null);
  const [expandedNames, setExpandedNames] = useState<ReadonlySet<string>>(new Set());
  const [query, setQuery] = useState("");
  // A ref, not state: it must survive Sidebar's remounts just like the
  // state above, but a scroll position update has no reason to re-render
  // AppShell the way a selection or expansion change does.
  const sidebarScrollTopRef = useRef(0);
  // Same query key as Sidebar's own useSessionList() call, so this shares
  // its cache rather than issuing a second fetch; only used here to compute
  // the ancestor chain to expand on selection.
  const sessionList = useSessionList();

  function selectSession(name: string) {
    setSelectedName(name);
    // Every selection path (a tree click, a search result, a future link
    // from elsewhere) funnels through here, so ancestors expand regardless
    // of how the session was reached.
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
      scrollTopRef={sidebarScrollTopRef}
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
        <main className="flex min-w-0 flex-1 flex-col overflow-hidden">
          {selectedName ? (
            <>
              <SessionHeader sessionName={selectedName} onOpenDetails={() => setDetailOpen(true)} />
              <Conversation sessionName={selectedName} onSelectSession={selectSession} />
            </>
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
