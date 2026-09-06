import { SearchIcon } from "lucide-react";

import { cn } from "@/lib/utils";

// docs/design/web-ui.md: "Search session names and resources, retaining
// ancestors of matching sessions." This component only owns the input;
// src/lib/sessionTree.ts's matchesSessionQuery/visibleTreeRows do the
// matching and ancestor retention against the value it reports.
export function SessionSearch({
  value,
  onChange,
}: {
  value: string;
  onChange: (value: string) => void;
}) {
  return (
    <label className="relative flex items-center px-2 pt-2 pb-1">
      <SearchIcon className="pointer-events-none absolute left-4 size-3.5 text-muted-foreground" />
      <span className="sr-only">Search sessions</span>
      <input
        type="search"
        value={value}
        onChange={(e) => onChange(e.target.value)}
        placeholder="Search sessions"
        className={cn(
          "h-8 w-full rounded-md border border-border bg-background pl-7 pr-2 text-sm outline-none",
          "focus-visible:border-ring focus-visible:ring-2 focus-visible:ring-ring/50",
        )}
      />
    </label>
  );
}
