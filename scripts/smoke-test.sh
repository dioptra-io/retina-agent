#!/usr/bin/env bash
#
# smoke-test.sh runs the agent against the mock orchestrator: the mock
# orchestrator sends its PDs and the test passes when every PD comes back as
# a FIE.
#
# The FIEs are written to the standard output; the logs of the mock
# orchestrator and of the agent go to the standard error. Arguments are passed
# on to mock-orchestrator.sh, for example: smoke-test.sh --count 100 --seed 7

set -euo pipefail

log() {
	printf '%(%Y-%m-%d %H:%M:%S)T smoke-test: %s\n' -1 "$*" >&2
}

usage() {
	cat <<'EOF'
Usage:
  smoke-test.sh [MOCK ORCHESTRATOR OPTION...]

Environment:
  RETINA_AGENT   the agent binary (default: ./retina-agent)
  SMOKE_ADDRESS  the address the two meet on (default: 127.0.0.1:50977)
EOF
}

if [[ ${1:-} == -h || ${1:-} == --help ]]; then
	usage
	exit 0
fi

scripts_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
agent=${RETINA_AGENT:-./retina-agent}
address=${SMOKE_ADDRESS:-127.0.0.1:50977}

if [[ ! -x ${agent} ]]; then
	echo "agent binary not found: ${agent} (build it with 'go build -o retina-agent .')" >&2
	exit 1
fi

export RETINA_SECRET=smoke-test

"${scripts_dir}/mock-orchestrator.sh" --address "${address}" "$@" &
orchestrator_pid=$!

# The agent logs to its standard output: keep it out of the FIEs.
"${agent}" -id smoke-agent -address "${address}" >&2 &
agent_pid=$!
trap 'kill "${agent_pid}" "${orchestrator_pid}" 2>/dev/null || true' EXIT

# The mock orchestrator ends the test. An agent that stops before it has
# failed, and would leave the mock orchestrator waiting.
status=0
wait -n "${orchestrator_pid}" "${agent_pid}" || status=$?
if kill -0 "${orchestrator_pid}" 2>/dev/null; then
	log "The agent stopped before the mock orchestrator was done"
	status=1
else
	wait "${orchestrator_pid}" || status=$?
fi

kill "${agent_pid}" 2>/dev/null || true
wait "${agent_pid}" 2>/dev/null || true

if ((status == 0)); then
	log "PASSED"
else
	log "FAILED"
fi
exit "${status}"
