#!/usr/bin/env bash
# together.sh NAME COMMAND [NAME COMMAND]... runs each COMMAND with bash at
# the same time, each into a log of its own, then prints each log in a group
# of its own, in the order given, and fails if any of them failed. CI's long
# jobs run their independent checks so: most of their tests take one core,
# and a runner has three or four.
set -u
dir=$(mktemp -d)
names=() pids=()
while [ $# -ge 2 ]; do
	log="$dir/${#names[@]}.log"
	bash -c "$2" >"$log" 2>&1 &
	names+=("$1") pids+=($!)
	shift 2
done
failed=()
for i in "${!pids[@]}"; do
	wait "${pids[$i]}" || failed+=("${names[$i]}")
done
for i in "${!names[@]}"; do
	echo "::group::${names[$i]}"
	cat "$dir/$i.log"
	echo "::endgroup::"
done
if [ ${#failed[@]} -ne 0 ]; then
	for n in "${failed[@]}"; do
		echo "::error::failed: $n"
	done
	exit 1
fi
