# ROB-020-001 reference observations

These are Codex QA observations captured on 2026-11-10 before the first-line
continuation repair. All inputs are synthetic. They are evidence for the chosen
policy, not a general compatibility or robustness ranking.

- `fixtures.json` and `fixtures-extra.json` contain 16 and 9 named inputs. Decode
  `base64` to get the exact bytes, including line endings and NUL.
- `java-results.json` and `java-extra-results.json` contain 64 and 36 observations:
  Angus/Jakarta defaults and Mime4j default, permissive and strict configurations.
  Java `result` strings preserve the observed event/part traces and base64 payloads.
  The initial Mime4j probe observes raw fields; the extra probe also records
  semantic `name` and `body`. An error can accompany an incomplete trace.
- `python-results.json` and `python-extra-results.json` contain 48 and 27
  observations under the default, compat32 and strict email policies. They retain
  the observed headers, part bodies, defects and errors.
- `manifest.json` pins versions, JAR hashes, snapshot hashes and execution limits.
  MimeKit was not executed.

The matching emlkit fixtures and intentional differences are documented in
[`framing_firstline_test.go`](../../../../framing_firstline_test.go). Its 24 root
base64 inputs and separate middle-child input were compared byte for byte with
these 25 reference inputs. The implementation's independent QA and final status
belong to the [roadmap ledger](../../../robustness-roadmap.md) and
[issue #20](https://github.com/saeroun-ai/emlkit/issues/20).

These files archive the recorded results rather than install or invoke reference
engines during `go test`. Reference probes ran sequentially within the enforced
host limits; the Java heap was additionally capped at 256 MiB. No private mail,
credentials or local dependency paths are included.
