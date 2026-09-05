import { BootstrapAuthError, IncompatibleApiError } from "@/lib/bootstrap";
import { useBootstrap } from "@/lib/useBootstrap";
import {
  IncompatibleApiState,
  LoadingState,
  UnauthenticatedState,
  UnavailableState,
} from "@/components/shell/ConnectionState";
import { AppShell } from "@/components/shell/AppShell";

export function App() {
  const bootstrap = useBootstrap();

  if (bootstrap.isPending) {
    return <LoadingState />;
  }
  if (bootstrap.isError) {
    if (bootstrap.error instanceof BootstrapAuthError) {
      return <UnauthenticatedState />;
    }
    if (bootstrap.error instanceof IncompatibleApiError) {
      return <IncompatibleApiState reportedVersion={bootstrap.error.reportedVersion} />;
    }
    return <UnavailableState onRetry={() => void bootstrap.refetch()} />;
  }
  return <AppShell bootstrap={bootstrap.data} />;
}
