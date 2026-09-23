# Security reporting

Report suspected security vulnerabilities privately using this repository's
**Security → Report a vulnerability** entry. Do not publish credentials, private
repository contents, operator topology or exploit details in an ordinary Issue.

This fork's current supported source line is the default dated `fork/*` generation.
Only explicitly published immutable releases have artifact provenance and native
package verification; a branch name or successful compilation is not a production
security certification. Pre-releases are intended for controlled acceptance.

When reporting a problem, provide the release/source revision, affected component
(primary, Edge, transport or client), a synthetic reproduction and the relevant
credential-free request ID/error class. Do not include Authorization headers,
private keys, user tokens or a complete production database.

Source hosting, public release distribution and a deployed AGS installation are
separate trust boundaries. Never push an old private source history into this
public repository, and never share an operator's deployment evidence as a public
fixture without an independent disclosure review.
