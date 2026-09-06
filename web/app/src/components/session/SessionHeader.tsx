import { useSessionDetail } from "@/lib/useSessions";
import { Button } from "@/components/ui/button";
import { ResourceValue } from "@/components/session/ResourceValue";

// docs/design/web-ui.md: "The header has no breadcrumbs and little vertical
// space between name and resource. It also exposes Details..." (Up/Down are
// this task's excluded scope — see the issue's scope boundary). The name
// renders from the prop immediately; only the resource and run/health wait
// on the detail fetch, which DetailPane shares via the same query key.
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
