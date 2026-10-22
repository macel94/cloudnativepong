# Exact-version OpenPGP absence review

Reviewed for the October 2026 security rebuild. This rotates the existing
GO-2026-5932 assessment to **only** `pkg:golang/golang.org/x/crypto@v0.57.0`;
the 0.52.0/0.55.0 scopes are retired. No additional advisory, version range,
severity exemption, or known-unfixed bypass is authorized.

## Evidence

- Publisher source `ba990344df686a8f829c76770d9f2b84a94117f4`:
  [scan run](https://github.com/macel94/cloudnativepong/actions/runs/39748409999).
  Each API/room/static/gateway report contains exactly one finding:
  GO-2026-5932, x/crypto v0.57.0, UNKNOWN, no fixed version. Reports are retained;
  this is not a claim that the unmaintained OpenPGP package has been fixed.
- With the final Go 1.27.3 builder, `CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go list -deps .`
  produces 443 production imports in the application module and 1,029 in
  `build/caddy`. Neither graph contains `golang.org/x/crypto/openpgp` or any
  subpackage. Both resolve x/crypto exactly to v0.57.0.
- All four Dockerfiles reject any OpenPGP package in the production import graph
  before compiling. Go binaries cannot dynamically import an omitted Go package.
- Application lock SHA-256: go.mod
  `640f41d420a5adad445628724ddd5773b086f7cc4704d2d6f6bf2f80a97327b9`;
  go.sum `70d7b431c6d337f66fe69cb1fdab7ef530a9003f002e3ffaa5010c1e549b0b21`.
- Caddy lock SHA-256: go.mod
  `ad7ba0627fc78e59b4ea80587833432e649810bad340a529407b84d2600747c0`;
  go.sum `6af3a164233778b8a67386362356bfb312d26888ccee645b7180f07fc27e8574`.

This meets OpenVEX `vulnerable_code_not_present`: the component metadata is
included, but the vulnerable package is excluded from these compiled products.
It is not a universal safety statement about x/crypto v0.57.0. The publisher's
signed decision binds the assessment to the exact image digest. A changed
version needs another review; adding an OpenPGP import makes the build fail.
The decision now requires the exact package PURL, not merely a version string.
Negative tests retain blocking behavior for retired/future versions, missing or
wrong package identities, and other unfixed findings. HIGH/CRITICAL findings
remain blocking even when a fixed version is available; patch availability is
not evidence that an installed package is fixed. Go 1.27.3 unit/race/vet checks
passed after the concurrent Dependabot 1.27.2 update was advanced to its patch.

References: [Go advisory](https://pkg.go.dev/vuln/GO-2026-5932),
[OpenVEX justification definitions](https://github.com/openvex/spec/blob/main/OPENVEX-SPEC.md#status-justifications).
