// Minimal workspace layout placeholder: the session tree itself is later
// work (#403 exposes the read model this would list from).
export function Sidebar() {
  return (
    <nav aria-label="Sessions" className="flex h-full flex-col gap-2 p-3 text-sm text-muted-foreground">
      <p>No sessions wired into this shell yet.</p>
    </nav>
  );
}
