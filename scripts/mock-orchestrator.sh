#!/usr/bin/env bash
#
# mock-orchestrator.sh imitates the orchestrator for one agent, so that the
# agent can be tried without one.
#
# It listens for an agent, answers its handshake, sends it a set of PDs, and
# writes the FIEs the agent returns to the standard output, one per line as
# received. Logs go to the standard error. It exits with status 0 when every
# PD got exactly one FIE, and 1 otherwise.
#
# With --drop-after it drops the connection once, part of the way through the
# PDs, and waits for the agent to connect again before it sends the rest. The
# connection is dropped as soon as the PDs sent so far have their FIEs, or
# after one second: FIEs that come later are expected on the next connection.
# FIEs the agent was sending at the time of the drop are lost.
#
# The PDs are sent as fast as the agent takes them, while the FIEs are read:
# like the orchestrator, it never waits for FIEs before it sends more PDs.
# With --read-delay it puts pressure on the agent: it does not read any FIE
# for a while after it starts sending, so the agent has to hold its FIEs, and
# then its PDs, without losing any. With --read-rate it is a slow reader for
# the whole run: it reads no more than that many FIEs per second. With
# --garbage it sends lines that are
# not PDs among the first PDs, which the agent must skip.
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
  -d, --drop-after arg
                      Drop the connection after this many PDs and wait for the
                      agent to connect again (default: 0, never)
  -g, --garbage arg   Number of lines that are not PDs to send, one before each
                      of the first PDs (default: 0)
  -q, --quiet         Do not log each PD
  -r, --read-delay arg
                      Seconds during which no FIE is read after the PDs start
                      to be sent on the last connection (default: 0)
  -R, --read-rate arg
                      Most FIEs read per second (default: 0, no limit)
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
drop_after=0
garbage=0
quiet=0
read_delay=0
read_rate=0
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
	-q | --quiet)
		quiet=1
		continue
		;;
	-a | --address | -n | --count | -d | --drop-after | -g | --garbage | -r | --read-delay | -R | --read-rate | -s | --seed | -w | --wait) ;;
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
	-d | --drop-after) drop_after=$1 ;;
	-g | --garbage) garbage=$1 ;;
	-r | --read-delay) read_delay=$1 ;;
	-R | --read-rate) read_rate=$1 ;;
	-s | --seed) seed=$1 ;;
	-w | --wait) wait_seconds=$1 ;;
	esac
	shift
done

for setting in count drop_after garbage read_delay read_rate seed wait_seconds; do
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

# connect waits for an agent to connect and answers its handshake, which is
# one JSON line each way.
connect() {
	log "Listening for an agent on ${address}"
	coproc NC { nc -l "${host}" "${port}"; }
	# shellcheck disable=SC2153 # NC_PID is set by coproc.
	nc_pid=${NC_PID}
	exec {from_agent}<&"${NC[0]}" {to_agent}>&"${NC[1]}"

	local request agent_id='' agent_secret=''
	if ! IFS= read -r -u "${from_agent}" request; then
		log "No agent connected"
		exit 1
	fi
	if [[ ${request} =~ \"agent_id\":\"([^\"]*)\" ]]; then
		agent_id=${BASH_REMATCH[1]}
	fi
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
}

# send_pds sends the PDs $1 to $2 to the agent, as fast as it takes them.
send_pds() {
	local id
	for ((id = $1; id <= $2; id++)); do
		if ((id <= garbage)); then
			printf 'this is not a PD (%d)\n' "${id}"
		fi
		if ((!quiet)); then
			log "PD  ${pds[${id}]}"
		fi
		printf '%s\n' "${pds[${id}]}"
	done >&"${to_agent}"
	log "Sent the PDs $1 to $2"
}

# start_sending sends the PDs $1 to $2 in the background, so that FIEs can be
# read meanwhile.
sender_pid=
start_sending() {
	send_pds "$1" "$2" &
	sender_pid=$!
}

# disconnect drops the agent's connection.
disconnect() {
	kill "${sender_pid}" 2>/dev/null || true
	wait "${sender_pid}" 2>/dev/null || true
	exec {from_agent}<&- {to_agent}>&-
	kill "${nc_pid}" 2>/dev/null || true
	wait "${nc_pid}" 2>/dev/null || true
}

declare -A answered=()
received=0
unexpected=0

# pace_reading waits when the FIEs read so far are ahead of the read rate.
lines_read=0
reading_since=
pace_reading() {
	local ahead
	if ((read_rate == 0)); then
		return
	fi
	if [[ -z ${reading_since} ]]; then
		reading_since=${EPOCHREALTIME/./}
	fi
	lines_read=$((lines_read + 1))
	ahead=$((reading_since + lines_read * 1000000 / read_rate - ${EPOCHREALTIME/./}))
	# Waiting starts a process: it is done for 5 ms or more at a time.
	if ((ahead >= 5000)); then
		printf -v ahead '%d.%06d' $((ahead / 1000000)) $((ahead % 1000000))
		sleep "${ahead}"
	fi
}

# read_fies reads the agent's FIEs until $1 PDs have their FIE, for at most $2
# seconds, or until the agent closes the connection.
read_fies() {
	local expected=$1 fie id now left timeout
	local deadline=$((${EPOCHREALTIME/./} + $2 * 1000000))
	while ((received < expected)); do
		now=${EPOCHREALTIME/./}
		left=$((deadline - now))
		if ((left <= 0)); then
			break
		fi
		printf -v timeout '%d.%06d' $((left / 1000000)) $((left % 1000000))
		if ! IFS= read -r -t "${timeout}" -u "${from_agent}" fie; then
			# The wait is over, or the agent closed the connection.
			break
		fi
		printf '%s\n' "${fie}"
		pace_reading
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
}

trap 'kill "${sender_pid:-}" "${nc_pid:-}" 2>/dev/null || true' EXIT
connect

next=1
if ((drop_after > 0 && drop_after < count)); then
	start_sending 1 "${drop_after}"
	wait "${sender_pid}"
	read_fies "${drop_after}" 1
	log "Dropping the connection after ${drop_after} PDs, ${received} FIEs received"
	disconnect
	connect
	next=$((drop_after + 1))
fi

start_sending "${next}" "${count}"
if ((read_delay > 0)); then
	log "Not reading FIEs for ${read_delay}s"
	sleep "${read_delay}"
fi
log "Waiting up to ${wait_seconds}s for the FIEs of the ${count} PDs"
read_fies "${count}" "${wait_seconds}"

if ((received == count && unexpected == 0)); then
	log "OK: all ${count} PDs got their FIE"
	exit 0
fi
log "FAILED: ${received} of ${count} PDs got their FIE, ${unexpected} unexpected lines"
exit 1
