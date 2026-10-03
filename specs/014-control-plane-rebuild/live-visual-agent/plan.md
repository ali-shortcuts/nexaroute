# Live Visual Agent Plan

1. **Transport:** retain the existing single SSE reader and publish each parsed event to the Visual Agent tracker.
2. **Correlation:** initialize from authoritative snapshot events, deduplicate by sequence, and group live events by request ID.
3. **Static topology:** replace the existing Overview topology contents with Client → NexaRoute → actual event deployment/result state.
4. **Presentation:** add one restrained geometric agent while a live execution is active; no idle traffic animation.
5. **Accessibility:** expose a live status equivalent; honor reduced motion and static semantic states.
6. **Verification:** run JavaScript syntax checks, Go tests, existing browser E2E, deterministic tracker tests when available, and inspect diff/secrets.
7. **Follow-up gate:** only add backend attempt-start/epoch metadata if end-to-end acceptance proves the current event contract cannot represent a trustworthy journey.

The initial change deliberately does not add a graphics dependency, a second stream, a new production server, or routing logic in JavaScript.
