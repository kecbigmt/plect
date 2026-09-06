import { useSessionDetail } from "@/lib/useSessions";
import { Button } from "@/components/ui/button";
import { ResourceValue } from "@/components/session/ResourceValue";

// The name renders from the prop immediately, since it's already known from
// the tree the user selected it in; only the resource and run/health wait on
// the detail fetch, shared with DetailPane through the same query key.
export function SessionHeader({
  sessionName,
  onOpenDetails,
}: {
  sessionName: string;
  onOpenDetails: () => void;
}) {
  const detail = useSessionDetail(sessionName);

  return (
    <div className="flex items-start justify-between gap-3 border-b border-border px-3 py-2">
      <div className="min-w-0">
        <h1 className="truncate text-sm font-semibold break-all">{sessionName}</h1>
        <div className="mt-0.5 flex min-w-0 items-center gap-2 text-xs text-muted-foreground">
          {detail.data && (
            <>
              <StatusPill>{detail.data.run}</StatusPill>
              {detail.data.health && <StatusPill>{detail.data.health}</StatusPill>}
              <ResourceValue value={detail.data.resourceId ?? ""} />
            </>
          )}
        </div>
      </div>
      <Button type="button" variant="outline" size="default" onClick={onOpenDetails}>
        Details
      </Button>
    </div>
  );
}

function StatusPill({ children }: { children: string }) {
  return (
    <span className="shrink-0 rounded-full border border-border px-1.5 py-0.5 leading-none">
      {children}
    </span>
  );
}
