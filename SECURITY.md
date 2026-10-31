# Security policy

## Reporting a vulnerability

Please do not open a public issue for a security problem. Report it privately through GitHub's "Report a vulnerability" button on the repository's Security tab, or by email to security@autonomousengine.org.

Include what you found, how to reproduce it, and the impact you expect. You will get an acknowledgement within five working days and a status update at least every two weeks until the issue is resolved.

## Supported versions

Security fixes are made on `main` and released in the next version. Older releases are not patched.

## Scope notes

Autonomous Engine executes code-generation agents against repositories you connect. Treat it as a privileged service: run it on infrastructure you control, keep `JWT_SECRET`, `MASTER_ENCRYPTION_KEY` and provider credentials secret, and grant repository tokens only the scopes the engine needs.
