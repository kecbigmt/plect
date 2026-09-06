# Publish native release archives and build the Plecture Web UI at release time

## Context

Plecture has no tags or release artifacts. The README offers
`go install github.com/kecbigmt/plecture/app/cmd/plect@latest`, and
`app/internal/version.Current` is a development placeholder. The downstream
`kecbigmt/devbox` package builds a pinned source revision with
`buildGoModule`, a vendor hash, and the repository integration-test flags.

The Plecture Web UI is built by Vite in `web/app` into
`app/internal/webui/webapp/dist`. That directory is committed because
`go:embed` can only see files in the Go build input. The `web-app-build` CI
job rebuilds it and compares it with Git. This makes every Web UI change
carry generated, hash-named files and makes the normal source installation
of `plect-web` depend on an artifact that should not be source controlled.

The durable-storage decision selects `github.com/mattn/go-sqlite3` for core
persistence. Its cgo requirement means a release binary must be built and
tested with a target-native C compiler. A generic `GOOS`/`GOARCH`
cross-compile does not establish that property.

## Decision

### Release surface and mechanism

Publish a GitHub Release for each annotated tag named `vMAJOR.MINOR.PATCH`.
Tags are the only release trigger; merging to `main` produces neither a
release nor a pre-release. The release workflow rejects a tag that is not a
strict SemVer tag or whose tagged commit is not reachable from `main`.

The first supported artifact targets are `linux/amd64`, `linux/arm64`, and
`darwin/arm64`. Each target builds on a native GitHub-hosted runner: an
Ubuntu x86_64 runner, an Ubuntu ARM64 runner, and the ARM64 macOS runner.
Adding a target requires a native runner and a passing artifact smoke test;
it is not a promise made by a successful cross-compile.

Each `plect_VERSION_GOOS_GOARCH.tar.gz` archive has one top-level directory
and contains these executables in `bin/`:

- `plect`
- `plect-web`

`channel-server`, `slack-adapter`, and `github-watcher` are plugin
executables, not core release artifacts. Their
[`plugin.toml`](../../plugins/claude/plugin.toml),
[`plugin.toml`](../../plugins/slack/plugin.toml), and
[`plugin.toml`](../../plugins/github/plugin.toml) `[[executables]]` entries
declare Go `build` commands. When a catalog add or update locks an enabled
plugin, `lockPluginAtPath` calls [`RunBuilds`](../../app/internal/plugins/build.go)
in that plugin's resolved catalog directory. For a pinned Git catalog, that
directory is below `~/.cache/plect/catalogs/<source-digest>/<revision>/`; the
build output stays in the plugin's `bin/` path there.

Consequently, a host needs a Go toolchain wherever an enabled catalog plugin
declares a Go build step. The release archive does not change that requirement.
The downstream `config/plect/plect.lock` remains the catalog revision and
content-hash authority for those builds. Prebuilt plugin executables are
catalog-level artifacts, not core binaries; the
[plugin boundary contracts](2026-08-17-plugin-boundary-contracts.md) decision
and its plugin-packaging design carry that follow-up. This ADR does not design
or bundle them.

Use a plain GitHub Actions workflow, shell packaging, and `gh release
create`; do not introduce GoReleaser or a repository Nix build as the
release mechanism. The workflow builds Vite once per native target, copies
its output into the ignored embed path, then builds `plect-web`. It injects
the tag version through a `-ldflags -X` variable, after changing
`version.Current` from a constant to a variable in the implementation PR.

The following is the complete introduced workflow. The source-test job in
ordinary CI remains the behavioral authority. This workflow verifies the tag
and native release inputs, runs `go test ./...` for `app`, smoke-tests every
released executable with `--help`, publishes a checksum manifest, and
publishes only when every matrix entry succeeds.

```yaml
# .github/workflows/release.yml
name: Release

on:
  push:
    tags: ["v*"]
  workflow_dispatch:
    inputs:
      ref:
        description: Main commit or branch to rehearse
        required: true
        type: string
      tag:
        description: SemVer tag to place in rehearsal archives
        required: true
        type: string

permissions:
  contents: write

env:
  GO_VERSION: "1.25.6"
  NODE_VERSION: "22.13"

jobs:
  verify-tag:
    runs-on: ubuntu-24.04
    steps:
      - uses: actions/checkout@11d5960a326750d5838078e36cf38b85af677262 # v4.4.0
        with:
          fetch-depth: 0
          ref: ${{ inputs.ref || github.ref }}
      - name: Verify the release tag
        env:
          TAG: ${{ inputs.tag || github.ref_name }}
          EVENT: ${{ github.event_name }}
        run: |
          [[ "$TAG" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]]
          if [ "$EVENT" = push ]; then
            [ "$(git cat-file -t "$TAG")" = tag ]
          fi
          git fetch origin main
          git merge-base --is-ancestor HEAD origin/main

  build:
    needs: verify-tag
    strategy:
      fail-fast: false
      matrix:
        include:
          - id: linux-amd64
            runner: ubuntu-24.04
            goos: linux
            goarch: amd64
          - id: linux-arm64
            runner: ubuntu-24.04-arm
            goos: linux
            goarch: arm64
          - id: darwin-arm64
            runner: macos-14
            goos: darwin
            goarch: arm64
    runs-on: ${{ matrix.runner }}
    steps:
      - uses: actions/checkout@11d5960a326750d5838078e36cf38b85af677262 # v4.4.0
        with:
          ref: ${{ inputs.ref || github.ref }}
      - uses: actions/setup-go@40f1582b2485089dde7abd97c1529aa768e1baff # v5.6.0
        with:
          go-version: ${{ env.GO_VERSION }}
      - uses: actions/setup-node@49933ea5288caeca8642d1e84afbd3f7d6820020 # v4.4.0
        with:
          node-version: ${{ env.NODE_VERSION }}
      - name: Install the pnpm version declared by web/package.json
        run: npm install -g "pnpm@$(node -p "require('./package.json').packageManager.split('@')[1]")"
        working-directory: web
      - name: Install and build the Web UI
        run: |
          pnpm install --frozen-lockfile
          pnpm --dir app test
          pnpm --dir app build
        working-directory: web
      - name: Test the app module with cgo enabled
        run: go test ./...
        working-directory: app
      - name: Build the released executables
        env:
          GOOS: ${{ matrix.goos }}
          GOARCH: ${{ matrix.goarch }}
          CGO_ENABLED: "1"
          VERSION: ${{ inputs.tag || github.ref_name }}
        run: |
          set -eu
          version="${VERSION#v}"
          root="plect_${version}_${GOOS}_${GOARCH}"
          out="$PWD/dist/$root/bin"
          mkdir -p "$out"
          flags="-X github.com/kecbigmt/plecture/app/internal/version.Current=$version"
          go build -C app -ldflags "$flags" -o "$out/plect" ./cmd/plect
          go build -C app -ldflags "$flags" -o "$out/plect-web" ./cmd/plect-web
          "$out/plect" --help
          "$out/plect-web" --help
          tar -C dist -czf "dist/${root}.tar.gz" "$root"
          rm -rf "dist/$root"
      - uses: actions/upload-artifact@ea165f8d65b6e75b540449e92b4886f43607fa02 # v4.6.2
        with:
          name: ${{ matrix.id }}
          path: dist/*.tar.gz
          if-no-files-found: error

  publish:
    needs: build
    if: github.event_name == 'push'
    runs-on: ubuntu-24.04
    steps:
      - uses: actions/download-artifact@d3f86a106a0bac45b974a628896c90dbdf5c8093 # v4.3.0
        with:
          path: dist
          merge-multiple: true
      - name: Create the checksum manifest and GitHub Release
        env:
          GH_TOKEN: ${{ github.token }}
          TAG: ${{ github.ref_name }}
        run: |
          cd dist
          sha256sum *.tar.gz > SHA256SUMS
          gh release create "$TAG" *.tar.gz SHA256SUMS --title "$TAG" --generate-notes
```

The pinned third-party actions in that workflow are deliberately limited to
the five entries shown above: `actions/checkout` at
`11d5960a326750d5838078e36cf38b85af677262`, `actions/setup-go` at
`40f1582b2485089dde7abd97c1529aa768e1baff`, `actions/setup-node` at
`49933ea5288caeca8642d1e84afbd3f7d6820020`,
`actions/upload-artifact` at `ea165f8d65b6e75b540449e92b4886f43607fa02`,
and `actions/download-artifact` at
`d3f86a106a0bac45b974a628896c90dbdf5c8093`. `gh` is provided by the
GitHub-hosted runner, so the workflow does not add a release-publishing
action. The implementation PR ratifies each pin before adding it.

The recommendation introduces no release-tool configuration file: there is
intentionally no `.goreleaser.yaml` and no Nix expression in this repository.
The workflow is the release configuration; the shown Nix expression belongs
to the downstream `kecbigmt/devbox` change.

The archive name includes the release version rather than a commit revision.
Consumers pin both that URL and its SHA-256. A release tag is immutable once
published. Correcting a release requires a new patch tag, which preserves the
meaning of an existing Nix hash and of an installed binary's version.

### Source installation and frontend output

Remove `app/internal/webui/webapp/dist/` from Git in the same PR that enables
the release workflow. Delete `.gitattributes`, whose only rule marks that
committed directory as generated, and add the output directory to the
existing ignore file:

```gitignore
# app/internal/webui/.gitignore
node_modules/
webapp/dist/
```

The migration removes the tracked files with `git rm -r --cached
app/internal/webui/webapp/dist`; the release job recreates the directory
before compiling `plect-web`. The `web-app-build` drift comparison is
replaced by a Web UI build-and-test job that runs `pnpm install
--frozen-lockfile`, `pnpm test`, and `pnpm build` without comparing its output
to Git. Its invariant is that the pinned frontend dependency graph can build
and test from source; it does not preserve a generated artifact as a second
authority. The release build itself additionally establishes that this output
can be embedded by `plect-web`.

After this cutover, `go install github.com/kecbigmt/plecture/app/cmd/plect@latest`
remains a supported source installation for the `plect` CLI, subject to the C
toolchain required by the SQLite driver. It does not install `plect-web`:
the absent Vite output makes that package intentionally unavailable from a
plain source module. `plect-web` is installed from a matching release archive.
The README Install section therefore presents a release-archive command first
and labels the `go install` command as the CLI-only, compiler-equipped source
option. It does not promise a Node-free `plect-web` source install.

### Downstream Nix consumption

`kecbigmt/devbox` consumes the Linux release archive, not a Plecture source
flake input. Its package expression fetches the versioned archive by hash and
exposes the archive's two `bin/` entries. The following replaces the source
`buildGoModule` expression there; the `sha256` is the value printed for the
selected `SHA256SUMS` entry, converted with `nix store prefetch-file` before
committing.

```nix
# nixos/packages/plect.nix in kecbigmt/devbox
{ pkgs }:

let
  version = "0.1.0";
  src = pkgs.fetchurl {
    url = "https://github.com/kecbigmt/plecture/releases/download/v${version}/plect_${version}_linux_amd64.tar.gz";
    sha256 = "sha256-REPLACE-WITH-NIX-STORE-HASH";
  };
in
pkgs.stdenvNoCC.mkDerivation {
  pname = "plect";
  inherit version src;
  nativeBuildInputs = [ pkgs.gnutar ];
  unpackPhase = "tar -xzf $src";
  installPhase = ''
    mkdir -p "$out/bin"
    cp plect_${version}_linux_amd64/bin/* "$out/bin/"
  '';
  meta.mainProgram = "plect";
}
```

The downstream flake removes its `plect` source input, `plectSrc` special
argument, `vendorHash`, `overrideModAttrs`, and Plecture's `checkFlags`. It
also removes the three stale plugin `subPackages` entries and the leftover
`github-watcher` user unit that points at an old Nix store path. The
[downstream module](https://github.com/kecbigmt/devbox/blob/main/nixos/modules/plect.nix)
states that `plect-bus` supervises catalog-declared services, so the PR does
not replace that unit. The downstream catalog configuration and lock remain
because they pin and build the enabled plugins on the host.

Plecture source integration tests continue to run in Plecture pull-request CI
and must pass before the tagged commit is released. The downstream repository
runs its own Nix evaluation and service/configuration smoke tests against the
release package; it does not claim that a package archive can reproduce Go
source integration tests.

`fetchurl` is preferred to `fetchzip`: it fetches the exact release asset
whose checksum is recorded in the release, while the derivation explicitly
controls extraction. It supports reproducible binary consumption without
requiring Nix users to build the cgo core or frontend. It does not remove the
Go toolchain needed by catalog plugins with Go build steps.

### Cutover and rollback

Do the cutover after the first Plecture Web UI milestone closes: #397, #398,
#399, and #408. The release-enabling PR contains the workflow, version
injection, README update, ignored frontend output, removal of the committed
directory, and the CI job replacement as one change. Before the first final
tag, a maintainer runs `workflow_dispatch` against the merged `main` commit
with the intended final tag as its archive version. That rehearsal exercises
the same native matrix but skips `publish`. The first final `v0.1.0` tag then
publishes the supported archives; no tag-triggered pre-release exists.

Before merging, preserve the last commit that contains `dist/`, retain its
source archive, and record the current downstream flake lock. Rollback before
a release is a normal PR revert. Rollback after a release restores the
previous release version and hash in `kecbigmt/devbox`; it does not overwrite
or delete the published release. If a source rollback must restore the
committed frontend build, regenerate it from the preserved commit and restore
the old embed and CI contract in the same revert. Subsequent SQLite data
rollback follows its own backup and migration procedure; changing an executable
does not downgrade durable data.

## Consequences

- Plecture gains explicit, immutable versions and native cgo artifact
  verification for its supported targets.
- The repository stops accumulating generated frontend files and Web UI
  changes no longer need to commit hashes produced by Vite.
- `plect-web` is a release artifact rather than a general `go install`
  target. The CLI's source installation remains narrow and requires cgo's C
  compiler once SQLite lands.
- Release maintenance includes runner availability, target smoke tests,
  action-pin updates, checksums, and coordinated downstream hash updates.
- No standing check is added beyond the frontend build/test check and native
  release smoke tests. The former guards reproducible frontend inputs; the
  latter guards the actual cgo artifacts users receive.

## Alternatives considered

### GoReleaser

GoReleaser provides archive naming, checksums, and GitHub Release publishing
from a `.goreleaser.yaml`, and its GitHub Action is available at
`e435ccd777264be153ace6237001ef4d979d3a7a` for v6. It is credible for a
pure-Go project. Here, its normal cross-compilation model still needs
per-target native cgo compilers, custom builder images or runners, and a
separate Vite-before-embed step. That leaves the essential release logic in
workflow hooks while adding GoReleaser configuration and a tool/action pin.
The plain workflow keeps those target-native assumptions visible and needs no
second release authority.

### Nix-built artifacts

Nix can provide a controlled C compiler and a fixed-output frontend
derivation, and keeping `buildGoModule` would continue to test Plecture source
in the downstream check phase. It would require a Plecture flake or
cross-platform Nix build infrastructure, frontend derivation maintenance, and
Nix as the release consumer's prerequisite. It does not by itself publish
portable GitHub Release assets for non-Nix users. Use Nix to consume verified
archives rather than make it the cross-platform release builder.

### Continue committing `dist/`

Committed output preserves the old `go install` behavior for `plect-web` and
makes a normal Go checkout self-contained. It also keeps generated hash churn
in source history, duplicates the frontend build as a checked-in authority,
and does not solve cgo-native distribution. The release pipeline provides a
more appropriate place to materialize the embedded artifact.

### Release every merge to `main`

Automatic pre-releases provide frequent binaries but make downstream version
selection and rollback depend on commits again. Tag-only stable releases give
a version, checksum, release notes, and downstream pin one durable meaning.

## Sources

Official documentation consulted on 2026-09-06:

- [GitHub Actions workflow syntax](https://docs.github.com/actions/writing-workflows/workflow-syntax-for-github-actions)
- [GitHub Actions immutable release guidance](https://docs.github.com/actions/security-for-github-actions/security-guides/security-hardening-for-github-actions#using-third-party-actions)
- [GitHub CLI release creation](https://cli.github.com/manual/gh_release_create)
- [Go cgo documentation](https://pkg.go.dev/cmd/cgo)
- [GoReleaser build customization](https://goreleaser.com/customization/builds/)
- [Nixpkgs `fetchurl`](https://nixos.org/manual/nixpkgs/stable/#fetchurl)
