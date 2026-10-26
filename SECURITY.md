# Security Policy

Command Center stores and serves software packages for other machines to install. Treat every
uploaded file, package name, version string, archive, metadata document, proxied response and
LDAP or SMTP reply as potentially malicious.

## Reporting a vulnerability

Please give the maintainers a reasonable chance to investigate and ship a fix before you publish
exploitable details, proof-of-concept code or weaponized files.

Use the repository host's private vulnerability reporting (for GitHub: the **Security** tab,
"Report a vulnerability") when it is enabled. If it is not, open a public issue that says only
that you have a security report and ask a maintainer for a private channel. Do not put a working
exploit, a secret, a sensitive log or a malicious attachment in a public issue.

## What to include

- the Command Center version: the output of `commandcenter version`, for example
  `docker compose exec commandcenter commandcenter version`, `docker run --rm <image> version` or
  `kubectl exec <pod> -- commandcenter version` (signed-in users also see it in the web UI's
  sidebar). A local build prints `dev`; then give the commit you built from
  (`git rev-parse --short HEAD`);
- how you run it (Docker image, compose file, flags);
- the affected format or component (Maven, npm, PyPI, Go, Cargo, web UI, auth, LDAP, SMTP, blob
  store, tasks);
- the security impact and the access an attacker needs (anonymous, signed-in user, token, admin);
- exact reproduction steps, expected and actual behaviour;
- logs or resource measurements when relevant, with credentials, tokens, hostnames and private
  package contents removed.

## Malicious test files

Do not attach a potentially harmful package or archive to a public issue. First describe its
format, size, hash, observed effect and the command that reproduces it, and arrange a private
transfer with a maintainer. A fixed defect gets a permanent regression test with a minimal,
redistributable sample whenever that is safe.

## Scope

- package uploads and parsing for every format: archives (zip, zip64, tar, gzip), metadata
  documents, version strings and file names; decompression bombs, oversized declarations,
  path traversal and resource exhaustion in the 128 MiB container;
- proxy and group behaviour: upstream responses, caching, loops, and dependency confusion between
  hosted and proxied packages;
- authentication and authorization: passwords, sessions, API tokens, roles and privileges, realm
  order, LDAP login and group mapping, lockout protection;
- the web UI and its JSON API: CSRF, XSS, access to other users' data;
- secrets: stored passwords, LDAP bind and SMTP passwords, tokens; anything that leaks them into
  responses, logs or events;
- the container image, dependencies and build scripts.

General bugs without a security impact belong in the normal issue tracker.

## Supported versions

Security fixes go to the latest release on the default branch. There are no other editions to
patch: Command Center has only the Human Edition. No Pro tier gets its fixes first.
