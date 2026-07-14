# Delete Permanently Invalid Refresh Credentials

## Problem

`delete-unauthorized-auth` only deletes credentials when `Manager.MarkResult`
receives an upstream HTTP 401. Credential refresh failures take a different
path: `refreshAuthForRequest` records the failure and reschedules refresh, but
never invokes credential deletion.

xAI exposes the second half of the defect. Its OAuth token endpoint returns
HTTP 400 with an OAuth `invalid_grant` payload when a refresh token is revoked.
The xAI auth client currently converts that response into an unstructured
string error. The conductor therefore cannot distinguish a permanently revoked
refresh token from a transient refresh failure.

## Required Behavior

During credential refresh, both of these failures represent a permanently
invalid credential:

- HTTP 401 unauthorized errors.
- HTTP 400 OAuth errors whose code is `invalid_grant`.

When `delete-unauthorized-auth` is enabled, either failure must remove the
credential from the manager, backing store, scheduler, model registry, cooldown
state, and automatic refresh loop.

When the option is disabled, the credential must remain stored but be marked as
a terminal authentication failure. Automatic refresh must stop until the
credential is explicitly repaired or replaced.

Other HTTP 400 responses and transient refresh failures must retain the current
backoff and retry behavior.

## Design

### Structured xAI OAuth Errors

The xAI auth client will return a small structured error for non-success token
responses. It will retain:

- The actual upstream HTTP status.
- The OAuth error code when the response body contains one.
- The response description used in the human-readable error message.

The error will expose the existing `StatusCode() int` convention used by the
conductor. The user-visible message will keep the current provider and status
context, so existing logs remain useful without exposing tokens.

This classification belongs in the xAI auth client because that layer owns the
OAuth wire response and can parse it without conductor-level JSON or string
guessing.

### Permanent Refresh Failure Classification

The conductor will classify a refresh failure as permanently invalid when it
is either unauthorized or a structured HTTP 400/401 `invalid_grant` error. The
classification is provider-neutral because OAuth `invalid_grant` has the same
credential semantics regardless of provider.

The conductor will preserve the actual HTTP status in `LastError`. An
`invalid_grant` failure will use the stable error code `invalid_grant`; a 401
will continue to use `unauthorized`. This avoids rewriting HTTP 400 as 401 while
still giving lifecycle code a single permanent-invalid decision.

### Deletion and Retention

For a permanent refresh failure with deletion enabled,
`refreshAuthForRequest` will remove the auth entry under the manager lock and
then reuse the existing `removeDeletedAuth` cleanup path outside the lock. This
keeps store I/O and callbacks out of the critical section and guarantees the
same cleanup performed for request-time 401 failures.

With deletion disabled, the refresh path will retain the auth, set it to an
unavailable error state, record the stable terminal error, clear its next
refresh time, update the scheduler snapshot, and remove it from automatic
refresh scheduling. Existing explicit registration or update paths remain the
way to repair the credential.

No changes are made to the management cleanup endpoint. Its xAI probing policy
is unrelated to an actual OAuth refresh response and remains unsupported.

## Tests

Tests will cover public behavior at the relevant boundaries:

1. The xAI token client parses an HTTP 400 `invalid_grant` response into an
   error that retains status 400 and the OAuth code.
2. A refresh-time `invalid_grant` deletes the auth from the manager and store
   when `delete-unauthorized-auth` is enabled.
3. The same failure retains the auth and stops automatic refresh when the
   option is disabled.
4. A non-`invalid_grant` HTTP 400 remains a retryable refresh failure and is not
   deleted.

Existing request-time 401 deletion tests remain unchanged and must continue to
pass. Verification will include focused package tests, the complete affected
auth test suites, formatting, and the required server build.

## Non-Goals

- Re-enabling xAI credential probing in the management cleanup endpoint.
- Deleting credentials for arbitrary HTTP 400 responses.
- Adding provider-specific keyword tables to the conductor.
- Changing request-time `invalid_grant` cooldown behavior.
