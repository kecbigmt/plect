import { useState } from "react";
import { CheckIcon, CopyIcon } from "lucide-react";

import { isWebUrl } from "@/lib/url";
import { Button } from "@/components/ui/button";

// A remote server's filesystem path or a resolver-less id is not something
// this UI can navigate to, so it stays plain text plus a copy affordance
// instead of a broken or sanitized link.
export function ResourceValue({ value }: { value: string }) {
  if (value === "") {
    return <span className="text-muted-foreground">—</span>;
  }
  if (isWebUrl(value)) {
    return (
      <a
        href={value}
        target="_blank"
        rel="noreferrer"
        className="break-all underline underline-offset-2 hover:text-muted-foreground"
      >
        {value}
      </a>
    );
  }
  return <CopyableText value={value} />;
}

function CopyableText({ value }: { value: string }) {
  const [copied, setCopied] = useState(false);

  async function copy() {
    await navigator.clipboard.writeText(value);
    setCopied(true);
    setTimeout(() => setCopied(false), 1500);
  }

  return (
    <span className="inline-flex min-w-0 items-center gap-1">
      <code className="min-w-0 break-all">{value}</code>
      <Button
        type="button"
        variant="ghost"
        size="icon"
        aria-label={`Copy ${value}`}
        onClick={() => void copy()}
      >
        {copied ? <CheckIcon className="size-3.5" /> : <CopyIcon className="size-3.5" />}
      </Button>
    </span>
  );
}
