# Retire workspace configuration layers

## Context

Definition discovery includes plugin and global roots, ancestor `.plect/`
overlays, and a workspace-directory layer. [Discovery](../../app/internal/config/discover.go)
and [workflow root calculation](../../app/internal/config/workflow.go) are the
present authorities for those layers.

## Decision

The workspace-directory and ancestor cascade layers are retired. Only plugin
and machine-owned global configuration layers participate in definition
discovery. A checkout's `.plect/` directory contributes no definitions.

Resource-specific customization is a workflow defined in the machine-owned
global layer. For example, a deployment variant can add a teardown node:

```toml
[widgets_review]
kind     = "workflow"
resource = "official.github.issue"

[[widgets_review.nodes]]
uses = "official.github.worktree"

[[widgets_review.nodes]]
uses = "widgets_teardown"
```

The caller selects that variant explicitly with `--workflow widgets_review`.
Automatic dispatch continues to require exactly one matching workflow and
errors otherwise. No regex routing table is part of the language.

## Consequences

The migration inventories checkout `.plect/` content, copies intended policy
to global configuration, and makes a backup before removing migrated content.

## Alternatives considered

### Keep ancestor overlays but remove the workspace-directory layer

Rejected. A path-derived layer retains checkout-dependent configuration and a
separate trust model.

### Add pattern-keyed global workflow routing

Rejected. No concrete consumer requires routing between global workflow
variants. Explicit `--workflow` selection preserves automatic dispatch's
single-match rule without adding a second selection mechanism.

### Permit checkout configuration after a signature check

Rejected. It adds a trust-distribution mechanism solely to preserve retired
layers.
