#!/bin/sh
# Runs every fuzz target of the module, one at a time, for a number of
# executions each: a target that runs out of time in the middle of an input
# can report a failure, so the bound is a count. Minimising is bounded too:
# without a bound one input can take a minute. A failing input is written
# under the package's testdata/fuzz/.
#
#   .github/scripts/fuzz.sh [executions]
set -eu

count=${1:-200000}
failed=0
for pkg in $(go list ./...); do
	for target in $(go test -list '^Fuzz' "$pkg" | grep '^Fuzz' || true); do
		echo "== $pkg $target"
		go test -run='^$' -fuzz="^$target\$" -fuzztime="${count}x" -fuzzminimizetime=1000x "$pkg" || failed=1
	done
done
exit "$failed"
