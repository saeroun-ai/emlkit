# ROB-020-002 reference observations

Codex executed these 31 synthetic finite inputs before implementation, on
`5b7086c`, on 2026-11-10. These are baseline observations, not a verdict that
ROB-020-002 has passed review or QA.

- `fixtures.json` contains named exact base64 inputs, at most 2 KiB each.
- `java-results.json` contains 124 observations: Angus/Jakarta defaults and
  Mime4j default/permissive/strict modes. Traces record raw bytes and semantic
  name/value or name/body fields; errors may leave an incomplete trace.
- `python-results.json` contains 93 observations: default/compat32/strict
  email policies, with headers, bodies, defects and errors.
- `manifest.json` pins versions, dependency and snapshot SHA256 hashes and limits.
  MimeKit was not executed.

Observed differences include Python treating a colonless header candidate and
following lines as body, Mime4j omitting many malformed fields while continuing,
and Angus exposing malformed raw and semantic headers. These observations inform
an explicit emlkit policy; they do not establish overall compatibility or a
robustness ranking. All content is synthetic; no private mail or credentials.

The reference probes and independent emlkit baseline are session artifacts under
`/tmp/emlkit-orphan/`. This archive records measured results and does not install
reference engines or claim a portable differential harness. Every engine was run
sequentially under `scripts/qa-bounded.py`; Java also used a 256 MiB heap cap.

Final status belongs to the [roadmap](../../../robustness-roadmap.md) and
[issue #20](https://github.com/saeroun-ai/emlkit/issues/20).
