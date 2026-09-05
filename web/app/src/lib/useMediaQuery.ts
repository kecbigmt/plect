import { useEffect, useState } from "react";

// Drives the narrow/wide layout split in AppShell. A change listener (not a
// one-shot check) matters here: rotating a tablet or resizing a desktop
// window must re-decide sidebar/detail-pane placement without a reload.
export function useMediaQuery(query: string): boolean {
  const [matches, setMatches] = useState(() => window.matchMedia(query).matches);

  useEffect(() => {
    const mql = window.matchMedia(query);
    const onChange = () => setMatches(mql.matches);
    onChange();
    mql.addEventListener("change", onChange);
    return () => mql.removeEventListener("change", onChange);
  }, [query]);

  return matches;
}
