# Frontend technical boundaries

Run `npm run typecheck` before `npm run build`. The checked core consists of
strict TypeScript transport, lifecycle scope, service-form normalization and wire
DTOs, plus `@ts-check` on the API facade and publish-job hook.
The system settings components, monitoring components and their DTOs are also
strictly typed, including their Vue templates.

This is a staged migration, not a claim that every legacy Vue template is typed.
Existing SFCs remain JavaScript; service form construction, mode transitions and
submission normalization use the checked module. Secondary cloud/diagnostic JSON
is opaque `JSONRecord`, and needs endpoint-specific DTOs before migrating those
pages. Keep TypeScript pinned to 5.9.3: TypeScript 7 removed the compiler
subpath currently required by vue-tsc 3.3.11.

All API calls accept final `{ signal, timeoutMs }` options. Reads default to 20 s;
mutations to 90 s. Read cancellation includes read-only POST probes/previews.
A mutation timeout only stops waiting and reports an unknown result; it does not
abort the server request or retry. Publishing reconciles by its existing request
ID. Other writes require inspecting current state before manually resubmitting.

Page scopes abort reads at unmount and suppress stale responses/events. Sidebar
navigation remains available during probes/cloud refreshes. Active synchronous
writes retain their page and write guard; server-owned publish jobs retain only
their existing write guard. Monitoring refreshes GET status every 15 seconds when
enabled, every 2 seconds while running, and stops timers on hide/unmount. Polling
never starts checks or sends notifications.

Binary backup downloads share the transport deadline, CSRF and session handling,
but read successful responses as Blob. Passwords, channel secrets and backup
passphrases are not persisted, included in scope keys, or logged by the UI.

The login version comes from `internal/buildinfo/version.txt`, injected by Vite;
the private npm package deliberately has no product version of its own.
