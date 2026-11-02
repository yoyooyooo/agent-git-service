# Linux native build input

The Darwin preparation does not produce the imile-win binary. A cross-compiled
Linux binary is not a native artifact and must not be copied into
`imile-linux-amd64/`.

Run on Linux x86_64, from a clean checkout of
`3a0c7e4f0ac5150d774efc66168907671061bcc5` after this preparation merges, or
from the preparation branch once its head is the reviewed source:

```sh
git fetch origin
git checkout --detach <reviewed-head>
GIT_SHA=$(git rev-parse HEAD)
test "$(git status --porcelain=v1 --untracked-files=all)" = ""
mkdir -p "$HOME/ags-routeb-linux-candidate"
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
- source revision and `git rev-parse HEAD^{tree}`

Refuse the binary unless `goos` is `linux`, `goarch` is `amd64`, `version` is
`source-<revision>`, and `revision` and `tree` match the checkout. Do not start
it with a real `~/.ags`. This file is an input list, not evidence that the
Linux build has run.
