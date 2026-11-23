# scripts

## fault_injection.sh

Proves the GPU-lifecycle test suite actually *catches* regressions rather than
merely exercising code — the "test the tests" requirement for the accelerator
lifecycle work.

For each case the script:

1. copies one production file into a scratch directory,
2. applies a precise, deliberately-broken mutation to it (e.g. swallow the
   first GPU error, re-submit a hard error through the next tier, release the
   same accelerator twice),
3. runs the targeted tests against the mutated file via `go test -overlay`
   (the repository is never modified), and
4. asserts the tests **fail**. A mutated build that still passes means the
   invariant is not covered, and the script exits non-zero.

```bash
./scripts/fault_injection.sh       # run every case
./scripts/fault_injection.sh -v    # also print the failing test output
```

Requires `go` and `python3`. The mutation anchors are asserted: a stale anchor
is reported as a failure instead of silently passing.
