import { Button, buttonVariants } from "@/components/ui/button";

export function LoadingState() {
  return (
    <div
      role="status"
      className="flex h-dvh items-center justify-center text-sm text-muted-foreground"
    >
      Connecting…
    </div>
  );
}

// Retrying will not turn an expired/missing session into a valid one, so this
// state links out to the existing Go-rendered /login page rather than
// offering a Retry action — see security.go's safeNextPath for how `next`
// is validated on the way back in.
export function UnauthenticatedState() {
  const next = encodeURIComponent(window.location.pathname + window.location.search);
  return (
    <div className="flex h-dvh flex-col items-center justify-center gap-3 text-sm">
      <p>Sign-in required.</p>
      <a href={`/login?next=${next}`} className={buttonVariants({})}>
        Go to sign in
      </a>
    </div>
  );
}

export function UnavailableState({ onRetry }: { onRetry: () => void }) {
  return (
    <div role="alert" className="flex h-dvh flex-col items-center justify-center gap-3 text-sm">
      <p>The Plecture server is unavailable.</p>
      <Button onClick={onRetry}>Retry</Button>
    </div>
  );
}
