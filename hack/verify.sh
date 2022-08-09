#!/usr/bin/env bash

set -Eeuo pipefail

verify_gofmt() {
	declare gofmt_diff=''

	gofmt_diff="$(gofmt -l cmd test)"

	if [[ -n "$gofmt_diff" ]]; then
		echo 'Files needing gofmt:'
		echo "$gofmt_diff"
		echo ''
		echo 'Run "go fmt ./..."'
		return 1
	fi
	return 0
}

verify_govet() {
	go vet ./...
}

verify_generated() {
	declare old
	old="$(mktemp)"
	declare new='test/extended/util/annotate/generated/zz_generated.annotations.go'
	cp "$new" "$old"
	go generate ./test/extended
	diff "$old" "$new"
}

declare run=0 failed=0 junit_testcases=''
declare suite_start suite_stop
suite_start="$(date +%s)"

for tc in verify_gofmt verify_govet verify_generated; do
	((run+=1))
	declare tc_start tc_out tc_exit
	tc_start="$(date +%s)"

	if tc_out="$($tc)"; then
		tc_exit=0
	else
		tc_exit=1
	fi

	declare tc_stop
	tc_stop="$(date +%s)"

	if [[ "$tc_exit" -eq 0 ]]; then
		junit_testcases+="$(cat <<-EOF
			<testcase name="${tc}" time="$((tc_stop-tc_start))"/>
		EOF
		)"
	else
		junit_testcases+="$(cat <<-EOF
			<testcase name="${tc}" time="$((tc_stop-tc_start))">
				<failure message="">${tc_out}</failure>
			</testcase>
		EOF
		)"
		((failed+=1))
	fi
	>&2 echo "$tc_out"
done
suite_stop="$(date +%s)"

if [[ -n "${ARTIFACT_DIR:-}" ]]; then
	cat > "${ARTIFACT_DIR}/junit_verify.xml" <<-EOF
		<testsuite name="openstack-tests-verify" tests="${run}" skipped="0" failures="${failed}" time="$((suite_stop-suite_start))">
		${junit_testcases}
		</testsuite>
	EOF
fi

exit "$failed"
