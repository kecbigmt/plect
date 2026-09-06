import { SessionNotFoundError } from "@/lib/sessionsApi";
import { useSessionDetail } from "@/lib/useSessions";
import { ResourceValue } from "@/components/session/ResourceValue";

// The shared optional detail-pane shell (docs/design/web-ui.md: "The right
// pane is closed initially. Selecting a Session, Task, or Node replaces
// its content..."). This slice only ever selects a Session; Task/Node
// detail is a later slice's addition. Keyed on sessionName through
// useSessionDetail, so switching sessions resets straight to a loading
// state instead of carrying the previous session's data over — a stale
// session's detail must never remain visible after the selection changes.
export function DetailPane({ sessionName }: { sessionName: string | null }) {
  if (sessionName === null) {
    return (
      <div className="flex h-full flex-col gap-2 p-3 text-sm text-muted-foreground">
        <p>Nothing selected.</p>
      </div>
    );
  }
  return <SessionDetailContent key={sessionName} sessionName={sessionName} />;
}

function SessionDetailContent({ sessionName }: { sessionName: string }) {
  const detail = useSessionDetail(sessionName);

  if (detail.isPending) {
    return (
      <div className="flex h-full flex-col gap-2 p-3 text-sm text-muted-foreground">
        <p>Loading…</p>
      </div>
    );
  }
  if (detail.isError) {
    const notFound = detail.error instanceof SessionNotFoundError;
    return (
      <div role="alert" className="flex h-full flex-col gap-2 p-3 text-sm text-muted-foreground">
        <p>{notFound ? "Session not found." : "Couldn’t load session details."}</p>
      </div>
    );
  }

  const s = detail.data;
  return (
    <div className="flex h-full flex-col gap-3 overflow-auto p-3 text-sm">
      <h2 className="truncate font-semibold break-all">{s.sessionName}</h2>
      <dl className="grid grid-cols-[6rem_1fr] gap-x-3 gap-y-1.5">
        {/* No title is ever synthesized from sessionName/workflow/etc. — only
            a recorded workflow display title renders here, and only when the
            API actually sent one. */}
        {s.title && <Row label="title">{s.title}</Row>}
        <Row label="run">{s.run}</Row>
        {s.health && <Row label="health">{s.health}</Row>}
        <Row label="resource">
          <ResourceValue value={s.resourceId ?? ""} />
        </Row>
        {s.message && (
          <Row label="message">
            {s.message.text}
            <span className="ml-1 text-muted-foreground">
              ({new Date(s.message.updatedAt).toLocaleString()})
            </span>
          </Row>
        )}
        {s.branch && <Row label="branch">{s.branch}</Row>}
        {s.workflow && <Row label="workflow">{s.workflow}</Row>}
        {s.tag && <Row label="tag">{s.tag}</Row>}
        {s.parentSession && <Row label="parent">{s.parentSession}</Row>}
        {s.children && s.children.length > 0 && (
          <Row label="children">{s.children.join(", ")}</Row>
        )}
        <Row label="created">{new Date(s.createdAt).toLocaleString()}</Row>
        <Row label="workspace">
          {s.workspaceDirExists ? "present" : "absent"}
          {s.workspaceDirPath ? ` (${s.workspaceDirPath})` : ""}
        </Row>
        {s.destroyed && (
          <Row label="destroyed">
            {s.destroyedAt ? new Date(s.destroyedAt).toLocaleString() : "yes"}
          </Row>
        )}
      </dl>
      {s.tasks && s.tasks.length > 0 && (
        <section>
          <h3 className="mb-1 text-xs font-semibold text-muted-foreground">Tasks</h3>
          <ul className="grid gap-1">
            {s.tasks.map((t) => (
              <li key={t.instance} className="flex justify-between gap-2">
                <span className="break-all">{t.instance}</span>
                <span className="text-muted-foreground">{t.status}</span>
              </li>
            ))}
          </ul>
        </section>
      )}
      {s.warnings && s.warnings.length > 0 && (
        <section role="alert">
          <h3 className="mb-1 text-xs font-semibold text-muted-foreground">Warnings</h3>
          <ul className="grid gap-1">
            {s.warnings.map((w, i) => (
              <li key={i}>{w}</li>
            ))}
          </ul>
        </section>
      )}
    </div>
  );
}

function Row({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <>
      <dt className="text-muted-foreground">{label}</dt>
      <dd className="min-w-0 break-all">{children}</dd>
    </>
  );
}
