# Security

## What denju verifies, and what it does not

denju replaces a running program's own binary. That makes it a component worth being
precise about, so this section is deliberately blunt about where its responsibility
ends.

**denju verifies exactly two things:**

1. The SHA-256 of the bytes it writes to disk matches `Request.SHA256`.
2. `Request.TargetOS` and `Request.TargetArch` match the running process, when set.

**denju does not:**

- check any signature, of the binary or of anything else
- validate Authenticode, codesigning, notarization, or any platform equivalent
- verify TLS beyond whatever your `Source` implementation does
- authenticate the peer it is talking to — it never talks to one
- decide that a URL, host, or digest is trustworthy

A digest proves the bytes arrived intact. It proves nothing whatsoever about where they
came from. **Establishing that a digest is authentic is the caller's responsibility**,
and it is the part that carries the security weight: anyone who can choose the digest
denju is given can choose the binary that replaces your program.

Deliver the digest over a channel you already trust — a signed manifest verified against
a pinned key, an authenticated RPC over mutual TLS, a release feed with a detached
signature you check yourself — and denju will make sure what lands on disk is what you
were promised. Deliver it over plain HTTP from a host you do not control and denju will
faithfully install whatever that host says.

## Other things worth knowing

**The staged download lands in your binary's directory.** It has to: `os.Rename` is only
atomic within one filesystem. That directory therefore needs to be writable by the
program, and anything else that can write there can stage a file — though it cannot make
denju install one, because the digest is checked against the value you supplied.

**Set a size limit on your `Source`.** denju does not impose one, because it does not
know what is reasonable for your binary. Without a limit a misconfigured or hostile
endpoint can fill the filesystem your program is installed on, which takes down
considerably more than the update. `httpsource.WithMaxBytes` does this for the provided
HTTP source.

**Version numbers are not ordered.** denju will move a program to a lower version as
readily as a higher one, because a deliberate downgrade is a legitimate operation. If
replaying an old, still-validly-signed update is a threat in your deployment, reject it
before calling `Update` — denju has no notion of "newer".

**The journal and the outcome record are plain, unauthenticated JSON** beside your
binary. Anything that can write them can influence what startup repair concludes. This
is the same trust boundary as the binary itself: a party that can rewrite files in your
install directory can already replace the program.

**The process environment is never written to disk.** It routinely carries credentials,
and a journal outlives the process that wrote it. The environment reaches the successor
by inheritance only — the exec on Unix, the detached spawn's explicit environment on
Windows.

## Reporting a vulnerability

Please report security issues privately rather than opening a public issue.

Open a [draft security advisory](https://github.com/qunulabs/denju/security/advisories/new)
on this repository. If that is not available to you, contact the maintainers through the
repository's listed contact address.

Please include the version or commit, the platform, and enough detail to reproduce.
We aim to acknowledge within a week.

## Supported versions

denju is pre-1.0. Fixes land on the latest minor release only.
