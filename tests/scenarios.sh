#!/usr/bin/env bash
#
# scenarios.sh runs the agent through a set of scenarios, between the mock
# orchestrator and the mock caracal of scripts/, and checks what it does in
# each. No packet is sent.
#
# Most scenarios are a run of scripts/smoke-test.sh: the mock orchestrator
# sends PDs in some way, the mock caracal answers in some way, and the
# scenario passes when every PD got exactly one FIE and the counters of the
# agent's last Stats log line have the expected values. The others check how
# the agent stops, or does not stop.
#
# The logs of each scenario are kept in a directory that is printed at the
# end, and removed when every scenario passed. It needs bash 5 or later and
# what the scripts of scripts/ need.

# The scenarios are functions called by name.
# shellcheck disable=SC2329

set -euo pipefail

export LC_ALL=C

log() {
	printf '%(%Y-%m-%d %H:%M:%S)T scenarios: %s\n' -1 "$*" >&2
}

usage() {
	cat <<'EOF_USAGE'
Usage:
  scenarios.sh [OPTION...] [SCENARIO...]

  -h, --help   Show this message
  -l, --list   List the scenarios

Without a scenario, all of them are run.

Environment:
  RETINA_AGENT  the agent binary (default: ./retina-agent)
EOF_USAGE
}

if ((BASH_VERSINFO[0] < 5)); then
	echo "scenarios.sh needs bash 5 or later: this is bash ${BASH_VERSION}" >&2
	exit 1
fi

tests_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
scripts_dir=${tests_dir}/../scripts
agent=${RETINA_AGENT:-./retina-agent}
# The address of the scenarios that do not go through smoke-test.sh.
address=127.0.0.1:50978

# The scenarios, in the order they run, and what each is about.
names=()
declare -A about=()
scenario() {
	names+=("$1")
	about[$1]=$2
}

# stat prints the value of a counter in the last Stats line of a log.
stat() {
	local line
	line=$(grep '"msg":"Stats"' "$1" | tail -n 1)
	if [[ ${line} =~ \"$2\":([0-9]+) ]]; then
		echo "${BASH_REMATCH[1]}"
	else
		echo missing
	fi
}

# expect_stats checks counters of the last Stats line of a log, given as
# name=value arguments.
expect_stats() {
	local file=$1 pair got status=0
	shift
	for pair in "$@"; do
		got=$(stat "${file}" "${pair%%=*}")
		if [[ ${got} != "${pair#*=}" ]]; then
			log "  ${pair%%=*}: got ${got}, want ${pair#*=}"
			status=1
		fi
	done
	return "${status}"
}

# smoke runs smoke-test.sh. The arguments up to -- are the expected counters,
# the rest goes to the mock orchestrator. The MOCK_CARACAL_* variables of the
# caller configure the mock caracal, and SMOKE_AGENT_ARGS gives the agent
# more arguments.
smoke() {
	local expected=()
	while [[ $1 != -- ]]; do
		expected+=("$1")
		shift
	done
	shift
	if ! RETINA_AGENT=${agent} "${scripts_dir}/smoke-test.sh" --quiet "$@" >"${case_dir}/fies.csv" 2>"${case_dir}/log"; then
		log "  smoke test failed: $(grep 'mock-orchestrator: FAILED' "${case_dir}/log" || echo 'see the log')"
		return 1
	fi
	expect_stats "${case_dir}/log" "${expected[@]}"
}

# start_agent starts the agent in the background, with the mock caracal as
# its caracal, and sets agent_pid.
start_agent() {
	PATH=${bin_dir}:${PATH} RETINA_SECRET=$1 "${agent}" -id scenario-agent -address "${address}" >"${case_dir}/log" 2>&1 &
	agent_pid=$!
}

# wait_exit waits up to $2 seconds for process $1 to exit, and sets
# exit_status. It fails if the process is still running.
wait_exit() {
	local tries=$(($2 * 10))
	while kill -0 "$1" 2>/dev/null; do
		if ((tries-- == 0)); then
			return 1
		fi
		sleep 0.1
	done
	exit_status=0
	wait "$1" || exit_status=$?
}

# wait_log waits up to $2 seconds for the pattern $1 in the scenario's log.
wait_log() {
	local tries=$(($2 * 10))
	until grep -q "$1" "${case_dir}/log"; do
		if ((tries-- == 0)); then
			log "  not in the log after $2s: $1"
			return 1
		fi
		sleep 0.1
	done
}

scenario baseline "10 PDs, all answered"
run_baseline() {
	smoke connections=1 pds_received=10 fies_sent=10 fies_complete=10 replies_unmatched=0 -- --count 10
}

scenario burst "5000 PDs at once: more than the queues hold, so the agent slows the orchestrator down"
run_burst() {
	smoke connections=1 pds_received=5000 pds_probed=5000 fies_sent=5000 pds_in_flight=0 -- --count 5000 --wait 60
}

scenario stalled_reader "20000 PDs while the orchestrator reads no FIE for 5 seconds: no PD and no reply may be lost"
run_stalled_reader() {
	smoke connections=1 pds_received=20000 fies_sent=20000 fies_complete=20000 fies_incomplete=0 replies_unmatched=0 -- --count 20000 --read-delay 5 --wait 120
}

# At a low rate, what caracal's input holds takes longer to send than the
# probe timeout: without the limiter, half of these PDs time out unsent.
scenario sustained_overload "2000 PDs at once to an agent limited to 250 PDs per second: none may time out waiting to be sent"
run_sustained_overload() {
	SMOKE_AGENT_ARGS="-max-pd-rate 250" smoke pds_received=2000 fies_sent=2000 fies_complete=2000 fies_incomplete=0 replies_unmatched=0 -- --count 2000 --wait 60
}

scenario slow_orchestrator "30000 PDs to an orchestrator that reads 2000 FIEs per second: the agent stops at its limit of 3000 PDs in flight"
run_slow_orchestrator() {
	SMOKE_AGENT_ARGS="-max-pd-rate 5000 -max-in-flight-pds 3000" smoke pds_received=30000 fies_sent=30000 fies_complete=30000 fies_incomplete=0 in_flight=0 in_flight_max=3000 -- --count 30000 --read-rate 2000 --wait 120
}

scenario no_replies "200 PDs, none answered: every FIE comes at the timeout, empty"
run_no_replies() {
	MOCK_CARACAL_REPLY_PERCENT=0 smoke pds_received=200 fies_sent=200 fies_incomplete=200 replies_matched=0 -- --count 200
}

scenario half_replies "500 PDs, half of the packets answered: complete and incomplete FIEs mixed"
run_half_replies() {
	MOCK_CARACAL_REPLY_PERCENT=50 smoke pds_received=500 fies_sent=500 pds_in_flight=0 replies_unmatched=0 -- --count 500
}

scenario slow_replies "100 PDs answered after 1.5 s, inside the 2 s timeout: all complete"
run_slow_replies() {
	MOCK_CARACAL_RTT_MS=1500 smoke fies_sent=100 fies_complete=100 fies_incomplete=0 -- --count 100
}

scenario late_replies "100 PDs answered after 3 s, past the timeout: all incomplete, each PD still has one FIE"
run_late_replies() {
	MOCK_CARACAL_RTT_MS=3000 smoke fies_sent=100 fies_complete=0 fies_incomplete=100 -- --count 100
}

scenario malformed "50 PDs with 20 lines that are not PDs among them: skipped on the same connection"
run_malformed() {
	smoke connections=1 pds_received=50 pds_malformed=20 fies_sent=50 -- --count 50 --garbage 20
}

scenario reconnect "connection dropped after 50 of 100 PDs: the rest is done on the next one"
run_reconnect() {
	smoke connections=2 pds_received=100 fies_sent=100 -- --count 100 --drop-after 50
}

scenario reconnect_in_flight "connection dropped while 50 PDs are in flight: their FIEs come on the next one"
run_reconnect_in_flight() {
	MOCK_CARACAL_REPLY_PERCENT=0 smoke connections=2 pds_received=100 fies_sent=100 fies_incomplete=100 -- --count 100 --drop-after 50
}

scenario stop_in_flight "SIGTERM while 200 PDs are in flight: the agent stops at once, with status 0"
run_stop_in_flight() {
	local orchestrator_pid exit_status
	RETINA_SECRET=scenario MOCK_CARACAL_REPLY_PERCENT=0 "${scripts_dir}/mock-orchestrator.sh" \
		--address "${address}" --count 200 --quiet >/dev/null 2>"${case_dir}/orchestrator.log" &
	orchestrator_pid=$!
	MOCK_CARACAL_REPLY_PERCENT=0 start_agent scenario
	wait_log '"msg":"Connected to orchestrator"' 10 || return 1
	sleep 0.5
	kill -TERM "${agent_pid}"
	if ! wait_exit "${agent_pid}" 5; then
		log "  the agent is still running 5s after SIGTERM"
		kill -KILL "${agent_pid}" "${orchestrator_pid}" 2>/dev/null || true
		return 1
	fi
	kill "${orchestrator_pid}" 2>/dev/null || true
	wait "${orchestrator_pid}" 2>/dev/null || true
	if ((exit_status != 0)); then
		log "  the agent exited with status ${exit_status}, want 0"
		return 1
	fi
	expect_stats "${case_dir}/log" pds_received=200 pds_probed=200
}

scenario caracal_dies "caracal is killed: the agent stops with an error"
run_caracal_dies() {
	local caracal_pid caracal_children exit_status
	start_agent scenario
	wait_log '"msg":"Caracal started"' 10 || return 1
	caracal_pid=$(pgrep -P "${agent_pid}" | head -n 1)
	# The mock caracal has processes of its own, which would keep its output
	# open. The real caracal is one process: all of them are killed.
	mapfile -t caracal_children < <(pgrep -P "${caracal_pid}" || true)
	kill -KILL "${caracal_pid}" "${caracal_children[@]}" 2>/dev/null || true
	if ! wait_exit "${agent_pid}" 5; then
		log "  the agent is still running 5s after caracal was killed"
		kill -KILL "${agent_pid}" 2>/dev/null || true
		return 1
	fi
	if ((exit_status == 0)); then
		log "  the agent exited with status 0, want an error"
		return 1
	fi
}

scenario wrong_secret "the orchestrator rejects the secret: the agent keeps running and tries again"
run_wrong_secret() {
	local status=0
	RETINA_SECRET=right "${scripts_dir}/mock-orchestrator.sh" --address "${address}" >/dev/null 2>"${case_dir}/orchestrator.log" || true &
	start_agent wrong
	wait_log 'not authenticated' 10 || status=1
	# It retries: the second attempt finds nobody listening.
	wait_log 'cannot connect to orchestrator' 10 || status=1
	if ! kill -0 "${agent_pid}" 2>/dev/null; then
		log "  the agent stopped"
		status=1
	fi
	kill -TERM "${agent_pid}" 2>/dev/null || true
	wait "${agent_pid}" 2>/dev/null || true
	wait
	return "${status}"
}

selected=()
while (($# > 0)); do
	case $1 in
	-h | --help)
		usage
		exit 0
		;;
	-l | --list)
		for name in "${names[@]}"; do
			printf '%-20s %s\n' "${name}" "${about[${name}]}"
		done
		exit 0
		;;
	*)
		if [[ -z ${about[$1]:-} ]]; then
			echo "Scenario '$1' does not exist" >&2
			usage >&2
			exit 1
		fi
		selected+=("$1")
		;;
	esac
	shift
done
if ((${#selected[@]} == 0)); then
	selected=("${names[@]}")
fi

if [[ ! -x ${agent} ]]; then
	echo "agent binary not found: ${agent} (build it with 'go build -o retina-agent .')" >&2
	exit 1
fi

work_dir=$(mktemp -d)
bin_dir=${work_dir}/bin
mkdir "${bin_dir}"
ln -s "${scripts_dir}/mock-caracal.sh" "${bin_dir}/caracal"

failed=()
for name in "${selected[@]}"; do
	case_dir=${work_dir}/${name}
	mkdir "${case_dir}"
	log "RUN  ${name}: ${about[${name}]}"
	start=${SECONDS}
	if "run_${name}"; then
		log "PASS ${name} ($((SECONDS - start))s)"
	else
		log "FAIL ${name} ($((SECONDS - start))s), logs in ${case_dir}"
		failed+=("${name}")
	fi
done

if ((${#failed[@]} == 0)); then
	log "All ${#selected[@]} scenarios passed"
	rm -r "${work_dir}"
	exit 0
fi
log "${#failed[@]} of ${#selected[@]} scenarios failed: ${failed[*]}"
exit 1
