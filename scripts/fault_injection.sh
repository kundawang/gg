#!/usr/bin/env bash
#
# fault_injection.sh — prove the GPU-lifecycle tests actually catch regressions.
#
# Each case copies a production source file to a scratch directory, applies a
# precise, deliberately-broken mutation to it, and runs the targeted tests with
# Go's -overlay flag so the repository is never modified. The targeted tests
# MUST fail; if any mutated build passes, the corresponding invariant is not
# covered and the script exits non-zero.
#
# Usage:
#   ./scripts/fault_injection.sh          # run every case
#   ./scripts/fault_injection.sh -v       # also print test output
#
# Requires: go, python3.

set -u

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT" || exit 1

VERBOSE=0
if [ "${1:-}" = "-v" ]; then
	VERBOSE=1
fi

SCRATCH="$(mktemp -d)"
trap 'rm -rf "$SCRATCH"' EXIT

FAILED=0
CASES=0

# mutate <file> <python-expression>
# Copies <file> into SCRATCH and replaces its contents with the result of the
# Python expression, which sees `src` (the file text). The mutation is rejected
# if it leaves the file unchanged, so a stale anchor cannot silently pass.
mutate() {
	local file="$1"
	local expr="$2"
	local out="$SCRATCH/$(basename "$file")"
	MUTATE_SRC="$ROOT/$file" MUTATE_OUT="$out" MUTATE_EXPR="$expr" python3 - <<'PY'
import os, sys
src = open(os.environ["MUTATE_SRC"], encoding="utf-8").read()
expr = os.environ["MUTATE_EXPR"]
mutated = eval(compile(expr, "<mutation>", "eval"), {"src": src})
if mutated == src:
    sys.stderr.write("mutation was a no-op (anchor missing):\n" + expr + "\n")
    sys.exit(2)
open(os.environ["MUTATE_OUT"], "w", encoding="utf-8").write(mutated)
PY
}

# apply_mutation is mutate() but reports failure through the exit status only
# and echoes the mutated path on success.
apply_mutation() {
	local file="$1" expr="$2"
	local out="$SCRATCH/$(basename "$file")"
	if ! mutate "$file" "$expr" >"$SCRATCH/mutate.err" 2>&1; then
		cat "$SCRATCH/mutate.err" >&2
		return 1
	fi
	echo "$out"
}

# expect_failure <case-name> <package> <test-regex> <file> <mutation-code> <why>
expect_failure() {
	local name="$1" pkg="$2" tests="$3" file="$4" code="$5" why="$6"
	CASES=$((CASES + 1))

	local mutated
	if ! mutated="$(apply_mutation "$file" "$code")"; then
		echo "FAIL [$name]: could not apply mutation"
		FAILED=$((FAILED + 1))
		return
	fi

	local overlay="$SCRATCH/overlay-$CASES.json"
	cat >"$overlay" <<JSON
{"Replace": {"$ROOT/$file": "$mutated"}}
JSON

	local output status
	output="$(go test -overlay "$overlay" -count=1 -run "$tests" "$pkg" 2>&1)"
	status=$?

	if [ $status -eq 0 ]; then
		echo "FAIL [$name]: tests PASSED against broken code — invariant not covered ($why)"
		FAILED=$((FAILED + 1))
	else
		echo "ok   [$name]: tests caught the injected fault ($why)"
		if [ $VERBOSE -eq 1 ]; then
			echo "$output" | sed 's/^/       /'
		fi
	fi
}

echo "=== GPU lifecycle fault injection ==="

# 1. Swallow the first GPU error: a later fallback would hide the original.
expect_failure \
	"first-error-preserved" "." \
	'TestContextGPUFallbackOnErrorPolicy|TestContextGPUStrictPolicyPropagatesError' \
	context.go \
	'src.replace(
	    "\tif c.gpuFirstErr == nil {\n\t\tc.gpuFirstErr = err\n\t}\n",
	    "\t_ = c.gpuFirstErr\n")' \
	"Context.GPUStatus().FirstErr must retain the first GPU error"

# 2. Double-submit: retry a hard shape-tier error through the path tier.
expect_failure \
	"no-double-submit" "." \
	'TestContextGPUHardErrorNotResubmitted' \
	context.go \
	'src.replace(
	    "\t\t} else if !errors.Is(err, ErrFallbackToCPU) {\n\t\t\treturn c.gpuFailure(op+\"-shape\", err)\n\t\t}\n",
	    "\t\t}\n")' \
	"a hard GPU error must not be re-submitted through the next tier"

# 3. Handle leak: Close fails to release the per-context GPU render context.
expect_failure \
	"context-gpu-handle-released" "." \
	'TestContextCloseIsConcurrencySafeAndIdempotent' \
	context.go \
	'src.replace(
	    "\tif rc != nil {\n\t\ttype gpuCtxCloser interface {\n\t\t\tClose()\n\t\t}\n\t\tif closer, ok := rc.(gpuCtxCloser); ok {\n\t\t\tcloser.Close()\n\t\t}\n\t}",
	    "\t_ = rc")' \
	"Close must release the per-context GPU render context exactly once"

# 4. Double-close accelerator: release the same accelerator on every call.
expect_failure \
	"accelerator-close-once" "." \
	'TestAcceleratorCloseIsIdempotentAndReleasesOnce' \
	accelerator.go \
	'src.replace(
	    "\tif a == nil || retainedClosed {\n\t\taccelMu.Unlock()\n\t\treturn\n\t}\n",
	    "\tif a == nil {\n\t\taccelMu.Unlock()\n\t\treturn\n\t}\n")' \
	"CloseAccelerator must release each accelerator exactly once"

# 5. Device loss still dispatched: expose the lost accelerator to callers.
expect_failure \
	"device-loss-excluded" "." \
	'TestAcceleratorDeviceLossExcludesDispatch' \
	accelerator.go \
	'src.replace(
	    "\taccel = nil\n\taccelStatus.State = AcceleratorDeviceLost\n\taccelStatus.Transitions++",
	    "\taccelStatus.State = AcceleratorDeviceLost\n\taccelStatus.Transitions++")' \
	"a lost accelerator must be excluded from dispatch"

# 6. Nil provider reaches the backend instead of being rejected up front.
expect_failure \
	"nil-provider-rejected" "." \
	'TestAcceleratorProviderNilAndReapply' \
	accelerator.go \
	'src.replace(
	    "\tif isNilDeviceProvider(provider) {\n\t\treturn ErrNilDeviceProvider\n\t}\n",
	    "\tif false {\n\t\treturn ErrNilDeviceProvider\n\t}\n")' \
	"a nil DeviceProvider must be rejected before the backend is called"

# 7. Scene renderer swallows the first draw error.
expect_failure \
	"scene-error-surfaced" "./scene" \
	'TestSceneGPUStrictPolicyPropagatesFailure' \
	scene/gpu_renderer.go \
	'src.replace(
	    "\tif err != nil && r.firstErr == nil {\n\t\tr.firstErr = err\n\t}\n",
	    "\t_ = err\n")' \
	"scene rendering must surface the first draw error under strict policy"

# 8. Renderer renders after Close instead of returning ErrRendererClosed.
expect_failure \
	"renderer-closed-guard" "./scene" \
	'TestSceneRenderAfterCloseReturnsError' \
	scene/renderer.go \
	'src.replace(
	    "\tif r.IsClosed() {\n\t\treturn ErrRendererClosed\n\t}\n\treturn nil\n}",
	    "\treturn nil\n}")' \
	"a closed scene.Renderer must reject further renders"

# 9. Cancellation is ignored by the scene GPU renderer.
expect_failure \
	"scene-cancellation" "./scene" \
	'TestSceneRenderCancellation' \
	scene/gpu_renderer.go \
	'src.replace(
	    "\t\t\tcase <-ctx.Done():\n\t\t\t\tdc.SetTransform(baseMatrix)\n\t\t\t\treturn ctx.Err()",
	    "\t\t\tcase <-ctx.Done():\n\t\t\t\t_ = baseMatrix")' \
	"a cancelled context must stop scene rendering and return ctx.Err()"

echo
if [ $FAILED -ne 0 ]; then
	echo "fault injection: $FAILED of $CASES mutations were NOT caught"
	exit 1
fi
echo "fault injection: all $CASES injected faults were caught"
