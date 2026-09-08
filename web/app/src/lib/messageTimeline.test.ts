import { describe, expect, it } from "vitest";

import { buildConversationTimeline } from "@/lib/messageTimeline";
import type { SessionEvent } from "@/lib/eventsApi";

function deltaEvent(id: string, index: number, body: string, final: boolean): SessionEvent {
  return {
    id,
    sessionName: "team/a",
    time: `2026-01-01T00:00:0${index}Z`,
    type: "plect.message_delta",
    source: "cli",
    direction: "outbound",
    summary: body,
    body,
    metadata: {
      message_id: "msg-1",
      message_id_origin: "native",
      source: "claude",
      kind: "text",
      index: String(index),
      final: String(final),
      turn_id: "turn-1",
    },
  };
}

function messageEvent(id: string, body: string): SessionEvent {
  return {
    id,
    sessionName: "team/a",
    time: "2026-01-01T00:00:09Z",
    type: "plect.message",
    source: "cli",
    direction: "outbound",
    summary: body,
    body,
    metadata: {
      message_id: "msg-1",
      message_id_origin: "native",
      source: "claude",
      role: "assistant",
      turn_id: "turn-1",
    },
  };
}

describe("buildConversationTimeline", () => {
  it("groups a delta sequence by message_id, ordered by index, into one growing message", () => {
    const timeline = buildConversationTimeline([deltaEvent("d0", 0, "Hel", false), deltaEvent("d1", 1, "lo", false)]);
    expect(timeline).toHaveLength(1);
    const [entry] = timeline;
    if (entry.kind !== "message") throw new Error("expected a message entry");
    expect(entry.text).toBe("Hello");
    expect(entry.final).toBe(false);
    expect(entry.source).toBe("claude");
    expect(entry.turnId).toBe("turn-1");
  });

  it("orders deltas by their index even when delivered out of order", () => {
    const timeline = buildConversationTimeline([deltaEvent("d1", 1, "lo", false), deltaEvent("d0", 0, "Hel", false)]);
    const [entry] = timeline;
    if (entry.kind !== "message") throw new Error("expected a message entry");
    expect(entry.text).toBe("Hello");
  });

  it("replaces the delta-built preview with the canonical plect.message body, and marks it final", () => {
    const timeline = buildConversationTimeline([
      deltaEvent("d0", 0, "Hel", false),
      deltaEvent("d1", 1, "lo", true),
      messageEvent("m0", "Hello there"),
    ]);
    expect(timeline).toHaveLength(1);
    const [entry] = timeline;
    if (entry.kind !== "message") throw new Error("expected a message entry");
    expect(entry.text).toBe("Hello there");
    expect(entry.final).toBe(true);
  });

  it("closes a delta-only stream at its terminal final=true, ahead of any canonical plect.message", () => {
    const timeline = buildConversationTimeline([deltaEvent("d0", 0, "Hel", false), deltaEvent("d1", 1, "lo", true)]);
    const [entry] = timeline;
    if (entry.kind !== "message") throw new Error("expected a message entry");
    expect(entry.text).toBe("Hello");
    expect(entry.final).toBe(true);
  });

  it("still lets a later canonical plect.message override a delta stream already closed by final=true", () => {
    const timeline = buildConversationTimeline([
      deltaEvent("d0", 0, "Hel", false),
      deltaEvent("d1", 1, "lo", true),
      messageEvent("m0", "Hello there"),
    ]);
    const [entry] = timeline;
    if (entry.kind !== "message") throw new Error("expected a message entry");
    expect(entry.text).toBe("Hello there");
    expect(entry.final).toBe(true);
  });

  it("keeps a message at its first-seen position when later chunks for the same message_id arrive", () => {
    const other: SessionEvent = {
      id: "other",
      sessionName: "team/a",
      time: "2026-01-01T00:00:05Z",
      type: "user.note",
      source: "cli",
      direction: "internal",
      summary: "unrelated",
    };
    const timeline = buildConversationTimeline([deltaEvent("d0", 0, "Hel", false), other, deltaEvent("d1", 1, "lo", true)]);
    expect(timeline).toHaveLength(2);
    const [first, second] = timeline;
    expect(first.kind).toBe("message");
    expect(second.kind).toBe("event");
  });

  it("ignores a reasoning delta's text but still tracks the message", () => {
    const reasoning: SessionEvent = {
      ...deltaEvent("r0", 0, "thinking...", false),
      metadata: { ...deltaEvent("r0", 0, "thinking...", false).metadata, kind: "reasoning" },
    };
    const timeline = buildConversationTimeline([reasoning, deltaEvent("d0", 0, "Hi", true)]);
    expect(timeline).toHaveLength(1);
    const [entry] = timeline;
    if (entry.kind !== "message") throw new Error("expected a message entry");
    expect(entry.text).toBe("Hi");
  });

  it("renders a legacy claude.reply event as an already-finished message", () => {
    const legacy: SessionEvent = {
      id: "legacy-1",
      sessionName: "team/a",
      time: "2026-01-01T00:00:00Z",
      type: "claude.reply",
      source: "claude",
      direction: "outbound",
      summary: "Done.",
      body: "Done.",
    };
    const timeline = buildConversationTimeline([legacy]);
    expect(timeline).toHaveLength(1);
    const [entry] = timeline;
    if (entry.kind !== "message") throw new Error("expected a message entry");
    expect(entry.text).toBe("Done.");
    expect(entry.final).toBe(true);
    expect(entry.legacy).toBe(true);
  });

  it("passes through an unrelated event unchanged", () => {
    const event: SessionEvent = {
      id: "01",
      sessionName: "team/a",
      time: "2026-01-01T00:00:00Z",
      type: "lifecycle.created",
      source: "plect",
      direction: "internal",
      summary: "session created",
    };
    const timeline = buildConversationTimeline([event]);
    expect(timeline).toEqual([{ kind: "event", event }]);
  });
});
