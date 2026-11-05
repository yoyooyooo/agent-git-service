# Route B native observation capture

`scripts/routeb_native_capture.py` records a window after the host coordinator
has validated its one permitted start. It does not control services, install
binaries, change the journal, or accept a deployment.

```sh
python3 scripts/routeb_native_capture.py \
  --receipt /private/transaction/execution.json \
  --profile /private/transaction/native-profile.json \
  --output /private/transaction/new-capture
```

The output directory must be new. Its existing parent must belong to the
operator and must not be writable by another user. Artifacts are private;
interrupted captures remain on disk. Do not publish these files with source.

## Identity and authority

The input receipt is `ags.routeb-host-execution.v1`, in `awaiting_observation`,
with an exact-source candidate. The coordinator binds the entire native
profile in `profile_sha256`. Its `capture` member supplies the manager,
runtime files, binary, health URLs, logs and timing. This is not a substitute
for host admission.

Runtime and installation paths must be the receipt root's `state/runtime.json`
and `state/installation.json`; the binary and logs also stay inside the root.
The manager definition has a separate frozen SHA-256. `isolated-native` only
admits launchd labels starting with `com.jackie.ags-hostpkg-test.` or user
systemd units starting with `ags-hostpkg-test-`. `production` is a distinct
mode and does not prove that an isolated native experiment passed.

The start anchor's `manager_sha256` is
`sha(canonical(read_manager(profile['capture']['manager'])[0]))`. Each sample
checks that identity, OS boot identity, runtime generation, supervisor/child
relation, executable inode and bytes, installation bytes and verified journal
head. Unknown effects or changed input interrupt the capture. Process start
time has second precision and is combined with the generation, PID, parent
and executable checks.

Both health URLs are exact `/readyz` URLs without credentials, query or
fragment. HTTP is restricted to loopback. Redirects and proxies are disabled.
Both endpoints must return ready with the expected full revision. Provider,
Git, pull-request, Wiki, restore metadata, old-binary start and cleanup proof
remain separate evidence. Health reads cannot supply those facts.

## Time and retained evidence

The interval is 1–30 seconds, with a profile-bound maximum gap of at most
60 seconds. A complete window covers the maximum of 600 seconds, the requested
duration and the lease. Time starts after the first sample finishes and ends
before the last starts. A clock jump, reboot, restart, log truncation or log
rotation interrupts the capture; no unreadable interval is silently skipped.

The first sample must begin within that same maximum gap after the validated
start. `validated_start.log_baseline` binds each log's name, path, device, inode
and offset; the collector reads from that offset, including bytes written
before it opened the log. The coordinator separately retains startup logs
before validation. Starting a new collector later cannot erase a failed
startup or substitute a new log baseline.

Samples retain runtime/installation JSON, verified journal content, ready
responses and new log bytes. Log ranges include device, inode, byte offsets
and SHA-256. systemd returns selected non-secret properties. launchctl output
contains environment values, so only its top-level identity fields are kept,
explicitly marked `filtered_launchctl_identity`; this is not complete raw
launchctl output.

`capture.json` contains artifact hashes and actual monotonic sample times.
Its `observed` status is not acceptance: `accepted` and `installable` remain
false. The M6 consumer must combine observations with the admitted stage
chain, independent restore comparison, business/provider reads and cleanup.
The older native-evidence v1 booleans do not replace that chain.

Injected test clocks or probes always produce `kind=fixture`, including a
synthetic 600-second case. The CLI exposes no fixture switch.
