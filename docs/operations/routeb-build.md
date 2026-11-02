# Route B candidate build

The preparation candidate is exact source
`3a0c7e4f0ac5150d774efc66168907671061bcc5`, tree
`a6fc9f08d5ca4c8d8fe4860dc8e551263455426f`. It is not a Release. Do not tag
it, publish it, or describe its provenance as a release.

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

Build on Darwin/arm64 with `GOTOOLCHAIN=go1.26.8` and `CGO_ENABLED=1`:

```sh
GIT_SHA=3a0c7e4f0ac5150d774efc66168907671061bcc5 \
  GOTOOLCHAIN=go1.26.8 CGO_ENABLED=1 \
  scripts/build-exact-source.sh binary --output /path/outside/repo/gh-server
```

Record `gh-server`, sha256, manifest, version JSON, `go version`, `otool -L`,
and the source revision and tree. Keep the output outside the source checkout.
Do not start it against `~/.ags`.

## imile-win Linux/amd64

Not built by the Darwin preparation task. Cross-compiling on Darwin is not a
native Linux artifact. The Linux build uses the same script, on Linux
x86_64, with `GOTOOLCHAIN=go1.26.8`, `CGO_ENABLED=1`, and the host `cc`.
After the build, record `file` and `ldd`. Inputs that must travel with that
build are this repository revision, the script, and this page.

See [mini](routeb-runbook-mini.md) and [imile-win](routeb-runbook-imile-win.md).
