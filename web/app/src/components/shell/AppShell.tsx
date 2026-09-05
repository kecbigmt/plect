import { useState } from "react";
import { PanelLeftIcon, PanelRightIcon } from "lucide-react";

import type { BootstrapInfo } from "@/lib/bootstrap";
import { useMediaQuery } from "@/lib/useMediaQuery";
import { Button } from "@/components/ui/button";
import { Sheet, SheetContent, SheetHeader, SheetTitle } from "@/components/ui/sheet";
import { Sidebar } from "@/components/shell/Sidebar";
import { DetailPane } from "@/components/shell/DetailPane";

// Narrower than this, the sidebar and detail pane move into an overlay
// instead of a persistent column (docs/design/web-ui.md: "Narrow screens use
// an overlay or temporary main-area detail view").
const WIDE_LAYOUT_QUERY = "(min-width: 768px)";

export function AppShell({ bootstrap }: { bootstrap: BootstrapInfo }) {
  const isWide = useMediaQuery(WIDE_LAYOUT_QUERY);
  const [sidebarOpen, setSidebarOpen] = useState(false);
  // Closed initially, per docs/design/web-ui.md's shared-details contract.
  const [detailOpen, setDetailOpen] = useState(false);

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
          <aside className="w-56 shrink-0 border-r border-border">
            <Sidebar />
          </aside>
        )}
        <main className="min-w-0 flex-1 overflow-auto p-4 text-sm text-muted-foreground">
          <p>Session data isn't wired into this shell yet.</p>
        </main>
        {isWide && detailOpen && (
          <aside className="w-72 shrink-0 border-l border-border" aria-label="Details">
            <DetailPane />
          </aside>
        )}
      </div>

      {!isWide && (
        <Sheet open={sidebarOpen} onOpenChange={setSidebarOpen}>
          <SheetContent>
            <SheetHeader>
              <SheetTitle>Sessions</SheetTitle>
            </SheetHeader>
            <Sidebar />
          </SheetContent>
        </Sheet>
      )}
      {!isWide && (
        <Sheet open={detailOpen} onOpenChange={setDetailOpen}>
          <SheetContent>
            <SheetHeader>
              <SheetTitle>Details</SheetTitle>
            </SheetHeader>
            <DetailPane />
          </SheetContent>
        </Sheet>
      )}
    </div>
  );
}
