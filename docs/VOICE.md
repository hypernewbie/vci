# Vci voice

Vci speaks like an operations console during a live mission. It reports exact
state without theatrics.

- Name the run, project, machine, phase, and resource when known.
- Lead with the action or fact.
- Use short sentences and strong verbs.
- Keep JSON codes stable; prose may improve without breaking agents.
- Put diagnostics on stderr. Keep stdout parseable.
- Do not joke, apologize theatrically, use fake military jargon, or add drama.

Good:

```text
Run run_01 staged on mac-local. Source snapshot verified.
Build failed in compile phase. Exit code: 1.
Cleanup incomplete. Lease retained for reaper pass.
```
