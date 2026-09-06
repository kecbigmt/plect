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
}

// The session tree's own data source: fetches the list once here so both
// the search box and SessionTree (pure rendering over an already-fetched
// list — see src/lib/sessionTree.ts) share one load/error/empty story.
export function Sidebar({
  selectedName,
  expandedNames,
  query,
  onQueryChange,
  onToggleExpanded,
  onSelect,
}: SidebarProps) {
  const sessionList = useSessionList();

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
        <div className="min-h-0 flex-1 overflow-auto">
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
