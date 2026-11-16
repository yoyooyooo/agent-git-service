# Route B candidate build

The deployment content baseline is exact source
`f63aa88e0c82b1e17174cf07f8033552457185b1`, tree
`8bf2c52ec3f7c7bc04850791b4e507bbc8261ab4`. It is not a Release. Do not tag
it, publish it, or describe its provenance as a release. The earlier
preparation baseline `3a0c7e4f0ac5150d774efc66168907671061bcc5` (tree
`a6fc9f08d5ca4c8d8fe4860dc8e551263455426f`) is history; this page was
rebased onto the later tip.

`scripts/build-exact-source.sh` injects the checkout HEAD that it verifies,
not a different revision. A binary built after this preparation change
therefore reports that later full HEAD and its tree. The manifest records
`baseline_revision=f63aa88e0c82b1e17174cf07f8033552457185b1` separately so the
content baseline is not confused with the built source identity.

`scripts/build-exact-source.sh binary` injects all of:

- `internal/buildinfo.Version=source-<40-hex revision>`
- `internal/buildinfo.Revision=<40-hex revision>`
- `internal/buildinfo.Tree=<40-hex tree>`
- `server.gitSHA=<40-hex revision>`

`gh-server --version` prints those three fields plus `goos`, `goarch` and
`go_version`. `development` means the buildinfo linker flags were not applied.

The Docker path in the same script passes `GIT_SHA` and `GIT_TREE`. The image
is not the native production candidate. Native production builds use
`CGO_ENABLED=1`. The Dockerfile remains `CGO_ENABLED=0` and is not a substitute
for the Darwin or Linux binary.

## mini Darwin/arm64

Build the reviewed checkout, not the content baseline, on Darwin/arm64
with `GOTOOLCHAIN=go1.26.8` and `CGO_ENABLED=1`. Export `GIT_SHA` from that
checkout and pass the same value to the script:

```sh
export GIT_SHA=$(git rev-parse HEAD)
export GIT_TREE=$(git rev-parse 'HEAD^{tree}')
test "$GIT_SHA" = "$(git rev-parse HEAD)"
GIT_SHA="$GIT_SHA" GIT_TREE="$GIT_TREE" \
  GOTOOLCHAIN=go1.26.8 CGO_ENABLED=1 \
  scripts/build-exact-source.sh binary --output /path/outside/repo/gh-server
```

Accept the binary only when `--version` reports `version=source-$GIT_SHA`,
`revision=$GIT_SHA` and `tree=$GIT_TREE`. `f63aa88e0c82b1e17174cf07f8033552457185b1`
is the content baseline, not the built revision. Record `gh-server`, sha256,
manifest, version JSON, `go version`, `otool -L`, and those two values. Keep
the output outside the source checkout. Do not start it against `~/.ags`.

`scripts/routeb_candidate.py` reads `ags.routeb-candidate-manifest.v1` only.
A relative `binary` is resolved against the manifest parent, never the
process cwd. The rehearsal schema `ags.routeb-candidate.v1` is not accepted.
`not_installed` and `not_a_release` are history flags, not permission to
install. Parsing a manifest is not an install and does not start a process.

## imile-win Linux/amd64

Not built by the Darwin preparation task. Cross-compiling on Darwin is not a
native Linux artifact. The Linux build uses the same script, on Linux
x86_64, with `GOTOOLCHAIN=go1.26.8`, `CGO_ENABLED=1`, and the host `cc`.
After the build, record `file` and `ldd`. Inputs that must travel with that
build are this repository revision, the script, and this page.

See [mini](routeb-runbook-mini.md) and [imile-win](routeb-runbook-imile-win.md).
