# ADR 020 — Image signature verification

- Status: accepted (v0.6)
- Date: 2026-09-21

## Context

Images were trusted: the runner pulled and unpacked whatever reference it was
given, which is the supply-chain gap the README called out. Signature
verification has to happen **before** an image is pulled and unpacked, and it
must be possible to turn off for local development without that being silent.

## Decision

**1. A policy, off by default, enforced on request.**
`VORA_IMAGE_POLICY` selects the mode:
- `off` (default): every image is trusted. The runner logs a warning at startup
  ("image signature verification is disabled; images are trusted") so trusting
  images is a visible choice, not an accident.
- `enforce`: an image whose signature cannot be verified is refused with a
  clear error, before any pull.

The default is `off` for v0.6 because the mini-pc/CI images are not yet signed;
turning enforcement on by default would break every existing deployment. The
gap is closed by configuration, and is stated in the README.

**2. Verification via cosign, key-based.**
In `enforce` mode the runner verifies with `cosign verify --key <pubkey> <ref>`
and treats a non-zero exit as untrusted; the command output is included in the
error. `VORA_COSIGN_KEY` (a cosign public key path) is **required** in enforce
mode — without it the runner refuses every image, so enforcement is never
silently absent. `VORA_COSIGN_PATH` overrides the cosign binary.

Shelling out to cosign (rather than linking the sigstore libraries) keeps the
dependency and the trust material explicit: the operator installs cosign and
pins a public key, the same way they already provide runsc/containerd.

**3. Enforced in `images.Resolver.Resolve`, before any source.**
The check runs before the containerd pull and before the local cache is
consulted, so an unverified image is refused no matter where it would have come
from. A misconfigured policy (enforce without a key) refuses every image rather
than failing open.

## Consequences

- With enforcement on, an unsigned or tampered image fails `vora sandbox
  create`/start with a signature error and is never unpacked.
- Enforcement needs a registry round-trip and the cosign binary on the runner;
  it is off by default so this is opt-in.
- Keyless (Fulcio/Rekor) verification and notation are not implemented; the
  policy interface (`images.Verifier`) leaves room for them.
- The end-to-end signed/tampered test lives in `tests/integration` and is
  skipped unless cosign plus signed/unsigned image references are provided,
  because it needs a registry and a signing key.

## Refs

- Issue vorash/vora#26; `internal/images/policy.go`, `internal/config`.