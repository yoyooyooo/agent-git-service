# Route B interpreter authority and native entry

This is the implementation contract for replacing the executable-only
interpreter identity. Isolated Linux entry is conditional on the live native
supervisor and its complete registered authority. Production retains the
separate `native_full_closure_required` gate. Source tests and a successful
cross-build do not constitute native acceptance.
File verification is a prerequisite, not proof that a Python process entered
through a trusted launcher or that a native transaction passed acceptance.

## Caller and authority

The intended caller selects one registered host and a bounded operation:
`bootstrap`, `transaction`, `serve`, `doctor`, `restore`, or `old-start`. It
cannot select an interpreter, verifier, import path, command, environment,
authority file, or substitute an asserted parent PID. The root-owned host
registration selects an interpreter-authority ID and exact manifest digest.

A native supervisor reads that fixed registration before starting Python.
The supervisor keeps UID 0, constructs the exact child command from the
registered operation, and starts one child with the registered runtime
credentials and a fixed environment. It never invokes a caller-supplied
command as root. Root registration remains an independently authorized OS
operation; source code or a receipt cannot confer that permission.

The child must establish an online connection to this supervisor. Both ends
verify kernel credentials; the supervisor accepts only the PID and process
birth identity of the child it actually spawned, with the expected UID/GID
and executable identity. The child verifies the supervisor's kernel-reported
UID and PID against its actual parent. The root service-manager boundary
owns the parent's image, birth and namespace verification; the unprivileged
child does not claim it can read the root parent's `/proc/PID/exe` or namespace
links. Those accesses can legitimately return EACCES. A PID in
JSON, argv, environment, a socket pathname, or an inherited pipe is not this
proof. In particular, credentials recorded when a root process creates a
socket pair must not be mistaken for the credentials of a child after exec.

The supervisor owns the admitted child/operation binding and revalidation
decision. A message only requests a recheck; it does not assert admission.
Direct Python invocation is not a registered spawned child and is refused.
Parent death, image drift, mismatched credentials, an unknown child, or a
failed recheck close the operation. No retry consumes another start.

## File authority

The new `ags.interpreter-authority.v1` contract is separate from ordinary v2
recovery objects and `ags.immutable-authority.v1`. Their file-only roles,
symlink refusal and 16 MiB asset limit remain unchanged.

The authority binds:

- Host instance, platform and architecture, fixed host-profile digest, and
  exact tool source revision, tree and bundle-manifest digest.
- An independently sealed publisher archive, publisher/release/asset
  identity and checksum document. The verifier derives archive members
  itself; a caller-provided inventory cannot omit an archive member.
- The complete installed prefix: every directory, regular file and bounded
  internal symlink, including standard library, extensions and libpython.
  Unknown entries, missing entries, changed bytes, writable ancestors,
  escaping/cyclic/dangling links and unsupported metadata refuse verification.
- Exact external loader/library files, loader configuration, search-path
  absence or inventories, and operating-system data resources. Locale and
  gconv resources are data dependencies, not ignored because they are not ELF.
- Independent recovery assets and provisioning identity. Reconstituting a
  root-owned prefix or OS dependency is a separate root operation; ordinary
  backup/restore must not silently recreate root authority.

Dedicated bounded archive/inventory limits allow the real interpreter's
large files without widening the ordinary recovery contract. Prefix content
must agree with the publisher archive after the single documented mode
normalization (regular files 0644/0755, directories 0755). Symlink targets
remain exact. Additional transformations require a new reviewed format.
On Darwin, the explicitly named `com.apple.provenance` host-generated
attribute is allowed only with its complete value digest pinned and
rechecked. It is not used as trust or approval evidence. Other prefix
attributes and all ACLs remain unsupported. Publisher content bytes are
unchanged by this metadata allowance.

Verification checks a declared closed catalog. It does not discover every
possible future Python file access, prove process provenance, or admit an
unvalidated loader/resource catalog. Static native dependency analysis and
real fixed-operation native probes must establish catalog completeness before
the native entry can admit execution. Unknown native dependencies remain a
blocker; the checked catalog is never relabeled as a full native acceptance.

## Process and platform boundary

The OS kernel, privileged service manager and the explicitly registered
native supervisor's own platform loader are the bootstrap trusted base. The
supervisor does not claim to verify this base before its own first instruction.
Its executable and non-system dependencies must be root protected and pinned;
arbitrary writable dynamic libraries are outside this base.

On Linux, a CGO-disabled supervisor avoids a new user-space dynamic-loader
dependency. It must be started independently by the root service manager,
not as a descendant of an untrusted same-UID caller. Any reliance on Yama
must read and bind the actual `ptrace_scope`, namespace and credential facts.
Setting dumpability before exec is not a permanent protection: exec can
reset it. The implementation must test sibling/ancestor interference on the
actual platform and refuse unsupported policy. It must not change a global
kernel policy to obtain a passing result.

On Darwin, UID 0 ownership alone does not prove resistance to `task_for_pid`,
ptrace, debugger entitlements or dyld injection. A standalone distribution,
its actual signatures/entitlements, hardened-runtime behavior and the sealed
OS image boundary require native evidence. The implementation must retain a
platform-specific refusal until this boundary is established. Copying a
Homebrew tree does not turn its writable dependencies into trusted assets.

## Integration points and retained gates

The fixed manager definitions must start the native supervisor, not Python.
Native bootstrap, host transaction, managed serve, restore, old one-time
start and every subsequent mutating boundary consume the same verified
child/operation context. Recovery also rechecks the immutable interpreter
authority before either current or old startup. Read-only `doctor` reports
missing prerequisites without reporting readiness from a file-only check.

The catalog component is an offline native schema/archive/filesystem verifier.
It has no spawn, manager, provision, installation, permission-changing or
recovery API. Real-file tamper tests cover its refusal behavior. It cannot
remove `interpreter_full_closure_required` by itself. Root registration,
kernel-bound child provenance, process interference controls and native
acceptance are separate completion criteria. Production retains
`native_full_closure_required` through complete native recovery and business
acceptance.

## Linux loader and resource boundary

`ags-routeb-stage0` is a static, CGO-disabled Go image. PID 1 starts a fixed
root systemd service. The native parent verifies the registered publisher
archive, complete installed prefix, independent recovery bytes, exact host
binding, fixed source bundle and both copies of every fixed manager unit.
Only then does it start the registered Python child under the runtime UID.

The Python entry explicitly invokes the canonical registered loader with
`--inhibit-cache`, an empty `--glibc-hwcaps-mask`, and two closed library
directories: publisher `lib` and a separately sealed dependency directory.
All recursive bare dependencies have registered files there. Bounded
`$ORIGIN/` dependencies may resolve only inside the publisher inventory.
The executable's loader view comes from PT_DYNAMIC and uniquely mapped
PT_LOAD ranges, including contiguous ranges spanning multiple segments.
Section headers may corroborate that view but cannot replace it. Audit,
depaudit, filter, auxiliary and unsupported loader flags refuse entry.

The kernel executable identity of this child is the loader. Its Python main
image must independently appear in the registered file-backed mappings.
The parent also rejects unknown mappings, checks exact process births and
credentials, and retains actual Yama and namespace facts. Fixed-operation
native probes must verify the resource catalog, including data such as
locale/gconv and any further resources the operation actually uses. Mapping
inspection alone cannot discover every non-mapped file read or prove future
operations have the same resource requirements. Such a discovery requires a
new reviewed authority and native evidence, not an automatic allowlist.

The fixed entry uses `pwd.getpwuid` for managed-root identity. Its complete
`/etc/passwd` and `/etc/nsswitch.conf` bytes are therefore external catalog
assets. The registered account must uniquely match the first `files` backend,
without NSS action overrides, and `/run/nscd` must be absent. The OS alias
`/var/run` is checked as a root-owned symlink with the exact target `/run`.
These are authorization inputs, not incidental files to omit from a baseline.
The fixed systemd unit enables `NoNewPrivileges`. The root observer must
actually retain the ptrace and DAC capability needed for global writer
visibility; the Python child must have no effective, permitted, inherited or
ambient capabilities. A root UID or an empty `lsof` output alone is insufficient.

The Linux broker observes writer closure through bounded kernel `/proc`
inventories. It enumerates every task because threads may have distinct FD
tables, anchors each read to `/proc/TGID/task/TID`, and rechecks birth identity.
FD access flags and mount IDs identify writable handles without statting
unrelated FUSE filesystems. Writable shared mappings remain writers even
after their FD closes. Each fixed target is held through a non-writing FD;
that descriptor must be visible in the census. Directory membership and
metadata are rechecked, and the original path is reopened with the same
no-symlink policy to reject replacement. Existing SQLite WAL/SHM sidecars are
included. Unknown visibility, device identity, permissions, reuse or budgets
refuse closure. TCP listener observations require the parent's, child's and
PID 1's actual network namespace to agree.

This is a bounded observation after the normal-stop proof and under the
transaction locks. It does not atomically freeze arbitrary external future
I/O. Counts identify observed blocking handles or mappings; shared FD tables
may contribute more than one observation. An unreadable census never becomes
an empty writer set. The separate tagged Linux fixture checks real different-UID
handles and closed-FD shared mappings; those component results are not M6.

The kernel, root service manager and fixed OS inspection tools remain an
explicit platform trust base. The interpreter catalog does not claim to
verify the entire operating system or these tools before their first
instruction. No global ptrace, sudoers, loader or environment policy is
changed to make a fixture pass.

## Separate recovery authority and one-use generations

Initial registration fixes operation IDs, attempts, authorization IDs,
source and host, but cannot contain a future backup digest. After the real
snapshot is sealed and independently reviewed, an authorized root operator
may write one separate `recovery-registration.json`. It binds that snapshot,
its manifest and group digests, and two distinct exact approval files. The
original registration is unchanged. The scope grant describes the original
human authorization; the later exact-byte review identifies the agent that
actually performed it, as specified in the delegation contract.

The restore child obtains a normal candidate stop from the root broker,
holds hostops/install/runtime locks, verifies root-observed writer closure,
and performs the existing complete-group restoration. Each member's intent
is durable before its switch; failures retain incident material and never
start a process. Full Tq and immutable-authority checks run before a result
is sealed by the root parent. An editable result JSON alone is not a restore
completion proof. Old-start requires the root seal, actual successful exits
of both restore processes, an empty control cgroup, the original snapshot,
fresh exact-byte verification and a separate one-use approval.

Initial, candidate and old serve phases have separate registered invocation
slots and root-issued tickets. Old-start never refunds the consumed candidate
handoff or restores the audit journal. Its live generation is bound to a
`serve-old` parent and the late recovery digest. Current and old startup
recheck the same interpreter authority. Normal stop combines the AGS exit
record, Python exit, native parent exit, effective manager result and empty
cgroup; a parent's before-exit receipt cannot prove its own successful exit.

Ordinary full-group restore preserves and rechecks the live root interpreter
authority and its independent recovery material. It does not reconstruct a
root prefix or modify system libraries. A separate root reconstruction in a
new fixture prefix has new inode identities and requires its own manifest.

## Alternatives considered

An environment flag, JSON receipt or parent argv check cannot establish which
program performed verification. Running a candidate Python interpreter to
verify its own standard library starts it before the required check. Both
approaches are rejected.

Embedding CPython into the native verifier would keep one process identity,
but adds a version-specific embedding and native-loader surface. It still
needs protection from local process injection. Moving the entire maintenance
state machine and all filesystem effects into native code would provide a
different enforceable boundary, but is a substantially larger rewrite. The
root supervisor with a fixed child is the smaller proposed boundary; it is
not admitted until its OS prerequisites are demonstrated.

## Primary platform references

- [Linux UNIX socket credentials](https://man7.org/linux/man-pages/man7/unix.7.html)
- [Linux Yama process restrictions](https://kernel.org/doc/html/latest/admin-guide/LSM/Yama.html)
- [Linux dumpability](https://man7.org/linux/man-pages/man2/PR_SET_DUMPABLE.2const.html)
- [Go traversal-resistant file access](https://go.dev/blog/osroot)

The repository's pinned Go toolchain is required. The fixed path validation
also refuses trailing separators and ambiguous paths; it does not assume a
generic rooted filesystem API handles every file type or mount boundary.
