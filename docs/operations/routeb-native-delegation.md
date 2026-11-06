# Isolated native scope grants

An operator may authorize an agent to complete an isolated native validation
before the exact package or recovery snapshot exists. The later byte review
belongs to the agent. Recording that review as a new human decision would
misstate its origin.

The versioned delegation contract records these two facts separately. It only
applies to an isolated native transaction whose live root parent has verified
the fixed registration and scope grant. Existing human approval versions keep
their original rules. Production cannot use the delegated versions.

## Original grant and concrete review

The root provisioner installs one new, immutable `scope-grant.json` beneath
the transaction's fixed registry. Its exact bytes are bound by the original
stage0 registration and rechecked by the native parent. The Python verifier
independently checks that digest, the registration digest, and the fixed host
profile through the authenticated live context.

The grant records the human authorization source, the actual agent that
captured it, original instruction text, and when that record was created.
An unavailable original message timestamp is `null`; a capture time must not
be passed off as the time the human sent the instruction. The grant narrows
the work to one logical host, host instance, transaction, runtime root, agent
and explicit set of purposes. Wildcards and production roots are refused.

Every later approval names its actual `agent:` actor. Its `delegation` member
binds the grant digest and a review with the same actor, exact subject digest,
real review timestamp, and bounded evidence references. Those references are
provenance for the operator to inspect, not signatures or independently
verified native outcomes. The approval bytes remain subject to the existing
root input registration or separately provisioned late recovery registration.

Neither a caller-provided file, a matching digest, a process argument, an
environment variable, nor a serialized context can replace the live native
parent. The fixed root registry and the independently authorized provisioner
remain explicit trust bases. This mechanism does not create a human grant.

| Purpose | Delegated approval schema | Exact review subject |
| --- | --- | --- |
| Initial native provision | `ags.routeb-native-provision-approval.v2` | Existing provision binding digest |
| Ordinary immutable dependencies | `ags.immutable-authority-approval.v2` | Host instance, tool manifest, installer and complete object list |
| First lifecycle activation | `ags.routeb-native-bootstrap-approval.v2` | Root, host instance, transaction, provision, identity and epoch |
| Host transaction | `ags.routeb-window.v3` | Original package bytes |
| Full restore | `ags.routeb-recovery-approval.v2` | Approval ID, purpose, host, actual snapshot and manifest, plus both acknowledgements |
| Old binary start | `ags.routeb-recovery-approval.v2` | A separate approval ID and old-start purpose bound to the actual snapshot |

Structured subjects use the existing canonical JSON fingerprint. They exclude
the review itself, avoiding a circular hash. A restore review cannot be reused
for old-start: both its purpose and approval ID are bound, and the existing
one-use consumption rules still apply. Future snapshot hashes are never filled
in ahead of the real sealed backup and review.

`routeb_native_delegation.py` provides `is_delegated`, `subject_sha256` and
`require_delegation`. The last function accepts no grant path or capability
override. A successful check only establishes the narrow approval relationship;
normal lifecycle, writer closure, complete backup, restoration, observation,
and cleanup evidence remain independently necessary.
