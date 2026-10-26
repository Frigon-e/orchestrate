# TOON encoder reference fixtures

Unmodified `tests/fixtures/encode/*.json` from https://github.com/toon-format/spec,
tag `v4.4.0`, commit `d09c3d33e37389345a74bd96981eb96dd9be6c5e`.
Fetched into a temporary checkout outside the tool directory. MIT license is
included. `UPSTREAM.md` is the upstream test-suite documentation.

`TestToonSpecFixtures` runs all nine encode-category files, including primitive
escaping, numeric boundaries, objects, primitive/mixed/nested/tabular arrays,
keyed objects, whitespace and delimiter options. No cases are dropped. This is
an encoder integration, not a claim that orch implements a public TOON decoder.
Reference cases use the encoder's numeric domain directly; application-level
normalization is pinned separately in `TestAgentOutputNormalizedRoundTrip`.

The shared boundary first uses encoding/json, including its custom marshalers,
then decodes with UseNumber into ordered toon.Object values. Interoperable
integers are limited to +/-(2^53-1). Larger integral values become decimal
strings, even if expressed with decimal points or exponents. Nonintegral
numbers outside float64's lossless shortest-decimal domain also become strings.
Numbers longer than 4096 bytes or with exponent magnitude above 4096 retain
their original spelling as strings, without expanding unbounded big integers.
Machine JSON retains its original numbers. Equality tests ignore object order
and compare TOON with JSON after this documented normalization; strings and
array order are unchanged. IDs and hashes are always strings.

Dependency: github.com/toon-format/toon-go v1.0.1 (MIT), requiring Go 1.23;
orch's go.mod declares Go 1.23.0.
