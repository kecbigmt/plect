// The shared optional detail-pane shell (docs/design/web-ui.md: "The right
// pane is closed initially. Selecting a Session, Task, or Node replaces its
// content..."). This task provides only the container: it always renders
// the empty-selection placeholder, since no data source feeds it a
// selection yet (Session read models land in a later task, #403).
export function DetailPane() {
  return (
    <div className="flex h-full flex-col gap-2 p-3 text-sm text-muted-foreground">
      <p>Nothing selected.</p>
    </div>
  );
}
