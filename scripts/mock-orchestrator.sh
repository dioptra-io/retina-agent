#!/usr/bin/env bash
#
# mock-orchestrator.sh imitates the orchestrator for one agent connection, so
# that the agent can be tried without one.
#
# It listens for an agent, answers its handshake, sends it a set of PDs, and
# writes the FIEs the agent returns to the standard output, one per line as
# received. Logs go to the standard error. It exits with status 0 when every
# PD got exactly one FIE, and 1 otherwise.
#
# The PDs are made up from a seed: the same seed and count always give the
# same PDs. Their destinations are in the documentation ranges (192.0.2.0/24,
# 198.51.100.0/24, 203.0.113.0/24 and 2001:db8::/32), which are not routed,
# so nothing is probed for real if the agent runs a real prober.
#
# The protocol is the orchestrator's: one JSON line each way for the
# handshake, then PDs as the CSV lines
#   id,"destination",near_ttl,protocol,first_half_word,second_half_word
# and FIEs as the CSV lines
#   id,capture_unix,"near_address",near_delta,"far_address",far_delta
#
# The secret agents must present is read from RETINA_SECRET, as the
# orchestrator does. It needs bash 5 or later and an OpenBSD style nc (the
# one of macOS, and of the netcat-openbsd package).

set -euo pipefail

export LC_ALL=C

log() {
	printf '%(%Y-%m-%d %H:%M:%S)T mock-orchestrator: %s\n' -1 "$*" >&2
}

usage() {
	cat <<'EOF'
Usage:
  mock-orchestrator.sh [OPTION...]

  -h, --help          Show this message
  -a, --address arg   Listening address for the agent connection (default: 127.0.0.1:50050)
  -n, --count arg     Number of PDs to send (default: 10)
  -s, --seed arg      Seed the PDs are made up from (default: 42)
  -w, --wait arg      Seconds to wait for the FIEs after the last PD is sent (default: 10)

Environment: RETINA_SECRET, the secret the agent must present.
EOF
}

if ((BASH_VERSINFO[0] < 5)); then
	echo "mock-orchestrator.sh needs bash 5 or later: this is bash ${BASH_VERSION}" >&2
	exit 1
fi

address=127.0.0.1:50050
count=10
seed=42
wait_seconds=10
secret=${RETINA_SECRET:-}

while (($# > 0)); do
	option=$1
	shift
	case ${option} in
	-h | --help)
		usage
		exit 0
		;;
	-a | --address | -n | --count | -s | --seed | -w | --wait) ;;
	*)
		echo "Option '${option}' does not exist" >&2
		usage >&2
		exit 1
		;;
	esac
	if (($# == 0)); then
		echo "Option '${option}' is missing an argument" >&2
		exit 1
	fi
	case ${option} in
	-a | --address) address=$1 ;;
	-n | --count) count=$1 ;;
	-s | --seed) seed=$1 ;;
	-w | --wait) wait_seconds=$1 ;;
	esac
	shift
done

for setting in count seed wait_seconds; do
	if [[ ! ${!setting} =~ ^[0-9]+$ ]]; then
		echo "${setting} must be a non-negative integer: got '${!setting}'" >&2
		exit 1
	fi
done
host=${address%:*}
port=${address##*:}
if [[ ${address} != *:* || -z ${host} || ! ${port} =~ ^[0-9]+$ ]]; then
	echo "address must be in the form host:port: got '${address}'" >&2
	exit 1
fi

# next_random sets `random` to the next number below $1. It is a linear
# congruential generator of its own, since the numbers of bash's RANDOM for a
# given seed differ between bash versions.
random_state=${seed}
next_random() {
	random_state=$(((random_state * 1103515245 + 12345) & 0x7fffffff))
	random=$(((random_state >> 8) % $1))
}

# The PDs, by ID. No two of them make the same probes.
declare -A pds=() probes=()
v4_prefixes=(192.0.2 198.51.100 203.0.113)
id=1
while ((id <= count)); do
	next_random 3
	kind=${random}
	next_random 30
	near_ttl=$((random + 1))
	next_random 1000
	first_half_word=$((24000 + random))
	case ${kind} in
	0) # UDP over IPv4
		next_random 3
		prefix=${v4_prefixes[random]}
		next_random 254
		destination=${prefix}.$((random + 1))
		protocol=17
		second_half_word=33434
		;;
	1) # ICMP over IPv4
		next_random 3
		prefix=${v4_prefixes[random]}
		next_random 254
		destination=${prefix}.$((random + 1))
		protocol=1
		next_random 65536
		second_half_word=${random}
		;;
	*) # ICMPv6
		next_random 65535
		printf -v destination '2001:db8::%x' $((random + 1))
		protocol=58
		next_random 65536
		second_half_word=${random}
		;;
	esac
	# The second half-word of an ICMP probe does not tell probes apart.
	probe=${destination},${near_ttl},${protocol},${first_half_word}
	if ((protocol == 17)); then
		probe+=,${second_half_word}
	fi
	if [[ -n ${probes[${probe}]:-} ]]; then
		continue
	fi
	probes[${probe}]=1
	pds[${id}]="${id},\"${destination}\",${near_ttl},${protocol},${first_half_word},${second_half_word}"
	id=$((id + 1))
done

log "Listening for an agent on ${address}"
coproc NC { nc -l "${host}" "${port}"; }
# shellcheck disable=SC2153 # NC_PID is set by coproc.
nc_pid=${NC_PID}
exec {from_agent}<&"${NC[0]}" {to_agent}>&"${NC[1]}"
trap 'kill "${nc_pid}" 2>/dev/null || true' EXIT

# The handshake: one JSON line each way.
if ! IFS= read -r -u "${from_agent}" request; then
	log "No agent connected"
	exit 1
fi
agent_id=
if [[ ${request} =~ \"agent_id\":\"([^\"]*)\" ]]; then
	agent_id=${BASH_REMATCH[1]}
fi
agent_secret=
if [[ ${request} =~ \"secret\":\"([^\"]*)\" ]]; then
	agent_secret=${BASH_REMATCH[1]}
fi
if [[ -z ${agent_id} ]]; then
	echo '{"authenticated":false,"message":"agent id is empty"}' >&"${to_agent}"
	log "Agent rejected: agent id is empty"
	exit 1
fi
if [[ ${agent_secret} != "${secret}" ]]; then
	echo '{"authenticated":false,"message":"secret is not correct"}' >&"${to_agent}"
	log "Agent ${agent_id} rejected: secret is not correct"
	exit 1
fi
echo '{"authenticated":true,"message":"authenticated"}' >&"${to_agent}"
log "Agent ${agent_id} connected"

for ((id = 1; id <= count; id++)); do
	log "PD  ${pds[${id}]}"
	printf '%s\n' "${pds[${id}]}" >&"${to_agent}"
done
log "Sent ${count} PDs, waiting up to ${wait_seconds}s for their FIEs"

declare -A answered=()
received=0
unexpected=0
deadline=$((SECONDS + wait_seconds))
while ((received < count && SECONDS < deadline)); do
	if ! IFS= read -r -t $((deadline - SECONDS)) -u "${from_agent}" fie; then
		# The wait is over, or the agent closed the connection.
		break
	fi
	printf '%s\n' "${fie}"
	if [[ ! ${fie} =~ ^([0-9]+),[0-9]+,\"[^\"]*\",[0-9]+,\"[^\"]*\",[0-9]+$ ]]; then
		log "Not a FIE: ${fie}"
		unexpected=$((unexpected + 1))
		continue
	fi
	id=${BASH_REMATCH[1]}
	if [[ -z ${pds[${id}]:-} || -n ${answered[${id}]:-} ]]; then
		log "FIE of an unknown or already answered PD: ${fie}"
		unexpected=$((unexpected + 1))
		continue
	fi
	answered[${id}]=1
	received=$((received + 1))
done

if ((received == count && unexpected == 0)); then
	log "OK: all ${count} PDs got their FIE"
	exit 0
fi
log "FAILED: ${received} of ${count} PDs got their FIE, ${unexpected} unexpected lines"
exit 1
