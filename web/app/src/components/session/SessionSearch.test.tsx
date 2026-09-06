import { useState } from "react";
import { describe, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";

import { SessionSearch } from "@/components/session/SessionSearch";

// A stateful wrapper, not a fixed value prop: SessionSearch is a controlled
// input, so typing "ab" only accumulates in the DOM value when something
// actually feeds each onChange back in as the next value prop.
function ControlledSearch({ onChange }: { onChange: (value: string) => void }) {
  const [value, setValue] = useState("");
  return (
    <SessionSearch
      value={value}
      onChange={(v) => {
        setValue(v);
        onChange(v);
      }}
    />
  );
}

describe("SessionSearch", () => {
  it("reports each keystroke through onChange", async () => {
    const user = userEvent.setup();
    const onChange = vi.fn();
    render(<ControlledSearch onChange={onChange} />);
    await user.type(screen.getByRole("searchbox", { name: /search sessions/i }), "ab");
    expect(onChange).toHaveBeenNthCalledWith(1, "a");
    expect(onChange).toHaveBeenNthCalledWith(2, "ab");
  });

  it("reflects the controlled value", () => {
    render(<SessionSearch value="release" onChange={vi.fn()} />);
    expect(screen.getByRole("searchbox", { name: /search sessions/i })).toHaveValue("release");
  });
});
