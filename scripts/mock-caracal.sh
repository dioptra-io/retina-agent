#!/usr/bin/env bash
#
# mock-caracal.sh imitates caracal v0.15.4 without sending any packet, so
# that the agent's caracal prober can be run and tested anywhere.
#
# Like caracal it reads probes from the standard input, one per line, as
#
#   dst_addr,src_port,dst_port,ttl,protocol[,flow_label[,wait_us]]
#
# with protocol one of icmp, icmp6, udp. It writes the CSV header and then one
# line per reply to the standard output, and logs to the standard error. It
# takes caracal's options, so it can be started with the same arguments.
#
# What it imitates:
#   - the header is written at start, before any probe is read;
#   - probes are sent at --probing-rate, checked every --batch-size packets
#     (caracal's defaults: 100 packets per second, batches of 128);
#   - --n-packets, --max-probes, --filter-min-ttl, --filter-max-ttl and
#     --meta-round;
#   - an invalid line is logged and skipped;
#   - a reply repeats the probe's destination, source port and TTL, and its
#     addresses are written as IPv6 (IPv4 as ::ffff:a.b.c.d);
#   - the destination port of an ICMP or ICMPv6 probe comes back as 0;
#   - capture_timestamp is in microseconds, rtt in tenths of a millisecond;
#   - at the end of the input it waits --sniffer-wait-time seconds and exits;
#   - sending does not wait for the output: when the replies are not read,
#     the probes go on being sent, and their replies keep the time they were
#     captured at.
#
# What it does not imitate:
#   - caracal never flushes its output, so its replies may reach the reader
#     in blocks; here every reply is written at once;
#   - echo replies and destination unreachable: every reply is a time
#     exceeded from a made up router;
#   - replies to probes that are not its own, duplicate replies, loss in the
#     sniffer;
#   - destinations given as an integer, the prefix filters (accepted, not
#     applied), and the statistics logged every 5 seconds.
#
# The replies are configured with environment variables, which the agent
# passes on to the process it starts:
#
#   MOCK_CARACAL_REPLY_PERCENT  share of the packets that get a reply (default 100)
#   MOCK_CARACAL_RTT_MS         time before the reply of a packet (default 20)
#   MOCK_CARACAL_SOURCE_ADDR    IPv4 address the probes are sent from (default 192.0.2.100)
#
# It needs bash 5 or later.

set -euo pipefail

export LC_ALL=C

log() {
	# caracal logs with spdlog: [date time.millis] [level] message
	local now=${EPOCHREALTIME} millis
	millis=${now#*.}
	printf '[%(%Y-%m-%d %H:%M:%S)T.%s] [%s] %s\n' "${now%.*}" "${millis:0:3}" "$1" "$2" >&2
}

usage() {
	cat <<'EOF'
Usage:
  mock-caracal.sh [OPTION...]

  -h, --help                    Show this message
  -r, --probing-rate arg        Probing rate in packets per second (default: 100)
  -z, --interface arg           Interface from which to send the packets (ignored)
  -B, --batch-size arg          Number of probes to send before calling the rate limiter (default: 128)
  -L, --log-level arg           Minimum log level (ignored)
  -N, --n-packets arg           Number of packets to send per probe (default: 1)
  -P, --max-probes arg          Maximum number of probes to send (unlimited by default)
      --source-address-v4 arg   (ignored)
      --source-address-v6 arg   (ignored)
  -W, --sniffer-wait-time arg   Time in seconds to wait after sending the probes (default: 1)
      --rate-limiting-method arg  (ignored)
      --filter-from-prefix-file-excl arg  (ignored)
      --filter-from-prefix-file-incl arg  (ignored)
      --filter-min-ttl arg      Do not send probes with ttl < min_ttl
      --filter-max-ttl arg      Do not send probes with ttl > max_ttl
      --caracal-id arg          (ignored)
      --meta-round arg          Value of the round column in the CSV output (default: 1)
      --no-integrity-check      (ignored)

Environment: MOCK_CARACAL_REPLY_PERCENT, MOCK_CARACAL_RTT_MS, MOCK_CARACAL_SOURCE_ADDR.
EOF
}

if [[ -z ${EPOCHREALTIME:-} ]]; then
	echo "mock-caracal.sh needs bash 5 or later: this is bash ${BASH_VERSION}" >&2
	exit 1
fi

probing_rate=100
batch_size=128
n_packets=1
max_probes=0
sniffer_wait_time=1
filter_min_ttl=0
filter_max_ttl=255
meta_round=1

reply_percent=${MOCK_CARACAL_REPLY_PERCENT:-100}
rtt_ms=${MOCK_CARACAL_RTT_MS:-20}
source_addr=${MOCK_CARACAL_SOURCE_ADDR:-192.0.2.100}

while (($# > 0)); do
	option=$1
	shift
	# Both "--option value" and "--option=value" are accepted, as by caracal.
	value=
	has_value=false
	if [[ ${option} == --*=* ]]; then
		value=${option#*=}
		option=${option%%=*}
		has_value=true
	fi
	case ${option} in
	-h | --help)
		usage
		exit 0
		;;
	--no-integrity-check)
		continue
		;;
	-r | --probing-rate | -z | --interface | -B | --batch-size | -L | --log-level) ;;
	-N | --n-packets | -P | --max-probes | -W | --sniffer-wait-time) ;;
	--source-address-v4 | --source-address-v6 | --rate-limiting-method) ;;
	--filter-from-prefix-file-excl | --filter-from-prefix-file-incl) ;;
	--filter-min-ttl | --filter-max-ttl | --caracal-id | --meta-round) ;;
	*)
		echo "Option '${option}' does not exist" >&2
		exit 1
		;;
	esac
	if ! ${has_value}; then
		if (($# == 0)); then
			echo "Option '${option}' is missing an argument" >&2
			exit 1
		fi
		value=$1
		shift
	fi
	case ${option} in
	-r | --probing-rate) probing_rate=${value} ;;
	-B | --batch-size) batch_size=${value} ;;
	-N | --n-packets) n_packets=${value} ;;
	-P | --max-probes) max_probes=${value} ;;
	-W | --sniffer-wait-time) sniffer_wait_time=${value} ;;
	--filter-min-ttl) filter_min_ttl=${value} ;;
	--filter-max-ttl) filter_max_ttl=${value} ;;
	--meta-round) meta_round=${value} ;;
	*) ;; # Accepted and not imitated.
	esac
done

for setting in probing_rate batch_size n_packets max_probes sniffer_wait_time \
	filter_min_ttl filter_max_ttl reply_percent rtt_ms; do
	if [[ ! ${!setting} =~ ^[0-9]+$ ]]; then
		echo "${setting} must be a non-negative integer: got '${!setting}'" >&2
		exit 1
	fi
done
if ((probing_rate == 0 || batch_size == 0 || n_packets == 0)); then
	echo "probing_rate, batch_size and n_packets must be positive" >&2
	exit 1
fi
if ((rtt_ms > 6553)); then
	echo "MOCK_CARACAL_RTT_MS cannot exceed 6553: caracal's rtt is 16 bits of tenths of a millisecond" >&2
	exit 1
fi

# A FIFO that nothing writes to: reading it with a timeout is a sleep that
# does not start a process.
work_dir=$(mktemp -d)
mkfifo "${work_dir}/sleep"
exec {sleep_fd}<>"${work_dir}/sleep"
rm -r "${work_dir}"

# sleep_micros waits for the given number of microseconds.
sleep_micros() {
	local seconds
	printf -v seconds '%d.%06d' $(($1 / 1000000)) $(($1 % 1000000))
	read -r -t "${seconds}" -u "${sleep_fd}" _ || true
}

# send_probes reads the probes and writes the replies they will get, each
# with the time it is captured at.
send_probes() {
	local line dst_addr src_port dst_port ttl protocol _flow_label wait_us extra
	local protocol_number reply_protocol icmp_type probe_dst reply_src probe_src
	local read_count=0 sent=0 filtered_lo=0 filtered_hi=0
	local rtt=$((rtt_ms * 10))
	# The time a batch of packets must take, in microseconds.
	local batch_micros=$((batch_size * 1000000 / probing_rate))
	local batch_start=${EPOCHREALTIME/./} now elapsed packet

	while IFS=, read -r dst_addr src_port dst_port ttl protocol _flow_label wait_us extra; do
		# Only used in the warnings: an invalid line is logged without its
		# optional columns.
		line=${dst_addr}${src_port:+,${src_port}}${dst_port:+,${dst_port}}${ttl:+,${ttl}}${protocol:+,${protocol}}
		if [[ -n ${extra} || -z ${protocol} ]]; then
			log warning "line=${line} error=Invalid CSV line: ${line}"
			continue
		fi
		if [[ ! ${src_port} =~ ^[0-9]{1,5}$ || ! ${dst_port} =~ ^[0-9]{1,5}$ || ! ${ttl} =~ ^[0-9]{1,3}$ ]] ||
			((src_port > 65535 || dst_port > 65535 || ttl > 255)); then
			log warning "line=${line} error=Invalid numeric value"
			continue
		fi
		case ${protocol} in
		icmp) protocol_number=1 ;;
		icmp6) protocol_number=58 ;;
		udp) protocol_number=17 ;;
		*)
			log warning "line=${line} error=Invalid protocol: ${protocol}"
			continue
			;;
		esac
		# An address with a colon is IPv6, one with a dot IPv4.
		if [[ ${dst_addr} == *:* ]]; then
			probe_dst=${dst_addr}
			probe_src=2001:db8::100
			reply_src=2001:db8::${ttl}
			reply_protocol=58
			icmp_type=3
		elif [[ ${dst_addr} =~ ^[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}$ ]]; then
			probe_dst=::ffff:${dst_addr}
			probe_src=::ffff:${source_addr}
			reply_src=::ffff:10.0.0.${ttl}
			reply_protocol=1
			icmp_type=11
		else
			log warning "line=${line} error=Invalid IPv4 address: ${dst_addr}"
			continue
		fi
		read_count=$((read_count + 1))

		if ((ttl < filter_min_ttl)); then
			filtered_lo=$((filtered_lo + 1))
			continue
		fi
		if ((ttl > filter_max_ttl)); then
			filtered_hi=$((filtered_hi + 1))
			continue
		fi
		# The destination port is not encoded in ICMP probes.
		if ((protocol_number != 17)); then
			dst_port=0
		fi

		for ((packet = 0; packet < n_packets; packet++)); do
			sent=$((sent + 1))
			if ((RANDOM % 100 < reply_percent)); then
				now=${EPOCHREALTIME/./}
				printf '%s,%s,%s,%s,%s,%s,%s,1,%s,%s,%s,0,250,56,"[]",%s,%s\n' \
					$((now + rtt * 100)) "${protocol_number}" "${probe_src}" "${probe_dst}" \
					"${src_port}" "${dst_port}" "${ttl}" "${reply_src}" "${reply_protocol}" \
					"${icmp_type}" "${rtt}" "${meta_round}"
			fi
			if [[ -n ${wait_us} ]] && ((wait_us > 0)); then
				sleep_micros "${wait_us}"
			fi
			# The rate is checked every batch_size packets.
			if ((sent % batch_size == 0)); then
				now=${EPOCHREALTIME/./}
				elapsed=$((now - batch_start))
				if ((elapsed < batch_micros)); then
					sleep_micros $((batch_micros - elapsed))
				fi
				batch_start=${EPOCHREALTIME/./}
			fi
		done

		if ((max_probes > 0 && sent >= max_probes)); then
			break
		fi
	done

	log info "Waiting ${sniffer_wait_time}s to allow the sniffer to get the last flying responses..."
	sleep_micros $((sniffer_wait_time * 1000000))
	log info "probes_read=${read_count} packets_sent=${sent} packets_failed=0 filtered_low_ttl=${filtered_lo} filtered_high_ttl=${filtered_hi} filtered_prefix_excl=0 filtered_prefix_not_incl=0"
}

# capture_replies writes each reply of the spool once its capture time has
# come, until the sender is gone and the spool is read to its end. The round
# trip time is the same for all, so they arrive in order.
capture_replies() {
	local reply partial='' now capture
	while true; do
		if ! IFS= read -r -u "${spool_fd}" reply; then
			# The end of the spool for now. What was read is the start of a
			# line the sender is still writing.
			partial+=${reply}
			if ! kill -0 "${sender_pid}" 2>/dev/null && [[ -z ${reply} ]]; then
				break
			fi
			sleep_micros 1000
			continue
		fi
		reply=${partial}${reply}
		partial=''
		capture=${reply%%,*}
		now=${EPOCHREALTIME/./}
		if ((capture > now)); then
			sleep_micros $((capture - now))
		fi
		printf '%s\n' "${reply}"
	done
}

echo "caracal v0.15.4-mock (mock build)" >&2
log info "caracal_id=0 n_packets=${n_packets} probing_rate=${probing_rate} sniffer_wait_time=${sniffer_wait_time} integrity_check=1 interface=mock rate_limiting_method=auto round=${meta_round}"
echo "capture_timestamp,probe_protocol,probe_src_addr,probe_dst_addr,probe_src_port,probe_dst_port,probe_ttl,quoted_ttl,reply_src_addr,reply_protocol,reply_icmp_type,reply_icmp_code,reply_ttl,reply_size,reply_mpls_labels,rtt,round"
log info "Reading from stdin, press CTRL+D to stop..."

# The sender writes the replies to a spool file, not to a pipe, so that it
# never waits for them to be read: like caracal's sender, which does not wait
# for its sniffer.
spool=$(mktemp)
exec {spool_fd}<"${spool}"
send_probes <&0 >"${spool}" &
sender_pid=$!
rm "${spool}"
capture_replies
wait "${sender_pid}"
