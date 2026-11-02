# Linux native build input

The Darwin preparation does not produce the imile-win binary. A cross-compiled
Linux binary is not a native artifact and must not be copied into
`imile-linux-amd64/`.

Run on Linux x86_64 from a clean detached checkout of the reviewed built HEAD.
`3a0c7e4f0ac5150d774efc66168907671061bcc5` is only the content baseline. Export
`GIT_SHA` and pass that same value into the build:

```sh
git fetch origin
git checkout --detach "$REVIEWED_HEAD"
export GIT_SHA=$(git rev-parse HEAD)
export GIT_TREE=$(git rev-parse 'HEAD^{tree}')
test "$GIT_SHA" = "$(git rev-parse HEAD)"
test "$(git status --porcelain=v1 --untracked-files=all)" = ""
mkdir -p "$HOME/ags-routeb-linux-candidate"
GIT_SHA="$GIT_SHA" GIT_TREE="$GIT_TREE" \
  GOTOOLCHAIN=go1.26.8 CGO_ENABLED=1 \
  scripts/build-exact-source.sh binary \
  --output "$HOME/ags-routeb-linux-candidate/gh-server"
```

Then record, next to that binary and outside the git checkout:

- `sha256sum gh-server`
- `./gh-server --version`
- `go version` (`go1.26.8`)
- `file gh-server`
- `ldd gh-server`
- built revision `$GIT_SHA` and built tree `$GIT_TREE`

Refuse the binary unless `goos` is `linux`, `goarch` is `amd64`, `version` is
`source-$GIT_SHA`, and `revision` and `tree` equal `$GIT_SHA` and `$GIT_TREE`.
Do not start it with a real `~/.ags`. This file is an input list, not evidence
that the Linux build has run.
