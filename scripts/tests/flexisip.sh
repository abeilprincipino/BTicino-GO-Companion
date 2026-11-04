#!/usr/bin/env sh
# Flexisip belongs to the firmware: it alone can hand over the TLS passphrase.
# The companion starts it only as a one-shot, last-resort fallback. These cases
# run the real init script with its device paths sandboxed and a simulated clock:
# sleep advances /proc/uptime, and the ip and netstat shims answer from that clock.
# No device, privileges, network access or Go toolchain required.
set -u

HERE="$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)"
ROOT="$(mktemp -d)"
trap 'rm -rf "${ROOT}"' EXIT HUP INT TERM
PASSED=0
FAILED=0
NEVER=999999
CLOCK_START=1000

cat > "${ROOT}/exercise.sh" <<'SH'
. "${R}/init.sh"
# Pinned so that tuning the production value does not rewrite these scenarios.
FLEXISIP_GRACE_SECONDS=60
wait_for_binary() { return 0; }
with_lifecycle_lock() { echo "$* at $(cut -d. -f1 "${R}/proc-uptime")" >> "${R}/events"; }
start_watchdog() { echo watchdog-start >> "${R}/events"; }
companion_healthy() { [ ! -f "${R}/unhealthy" ]; }
sleep() {
	printf '%s.50 1.00\n' "$(($(cut -d. -f1 "${R}/proc-uptime") + ${1:-1}))" > "${R}/proc-uptime"
	[ "$(cut -d. -f1 "${R}/proc-uptime")" -lt "$(cat "${R}/stop_at")" ] || rm -f "${WATCHDOG_ENABLE_FILE}"
}
if [ "$1" = start ]; then
	do_start
else
	: > "${WATCHDOG_ENABLE_FILE}"
	do_watchdog
fi
SH

# new_device <ip_from> <listen_from> <listen_until>: the uptimes at which the LAN
# address appears and at which something listens on the local SIP port. The FIFO
# exists with no writer, so a start that opens it hangs and hits the timeout.
new_device() {
	CASE=$((${CASE:-0} + 1))
	R="${ROOT}/${CASE}"
	export R
	mkdir -p "${R}/etc/init.d" "${R}/var/run" "${R}/var/tmp" "${R}/shim"
	mkfifo "${R}/var/tmp/bt_answering_machine.fifo"
	sed -e '/^case "${1:-}" in/,$d' \
		-e "s|/etc/|${R}/etc/|g" \
		-e "s|/var/run/|${R}/var/run/|g" \
		-e "s|/var/tmp/|${R}/var/tmp/|g" \
		-e "s|/proc/uptime|${R}/proc-uptime|g" \
		-e 's|^PATH=.*|PATH="'"${R}"'/shim:${PATH}"|' \
		"${HERE}/../init.d/companion" > "${R}/init.sh"
	printf '%s.50 1.00\n' "${CLOCK_START}" > "${R}/proc-uptime"
	printf '%s\n' "$1" > "${R}/ip_from"
	printf '%s\n' "$2" > "${R}/listen_from"
	printf '%s\n' "$3" > "${R}/listen_until"
	cat > "${R}/shim/ip" <<'SH'
#!/bin/sh
[ "$(cut -d. -f1 "${R}/proc-uptime")" -ge "$(cat "${R}/ip_from")" ] || exit 1
echo "    inet 192.0.2.10/24"
SH
	cat > "${R}/shim/netstat" <<'SH'
#!/bin/sh
now="$(cut -d. -f1 "${R}/proc-uptime")"
[ "${now}" -ge "$(cat "${R}/listen_from")" ] && [ "${now}" -lt "$(cat "${R}/listen_until")" ] || exit 0
echo "tcp 0 0 127.0.0.1:5060 0.0.0.0:* LISTEN"
SH
	printf '#!/bin/sh\nprintf "%%s\\n" "$*" >> "${R}/flexisip.calls"\n' > "${R}/etc/init.d/flexisipsh"
	chmod +x "${R}/etc/init.d/flexisipsh" "${R}/shim/ip" "${R}/shim/netstat"
}

# run_init <start|watchdog> [seconds]: bounded, so a hang fails instead of stalling.
run_init() {
	now="$(cut -d. -f1 "${R}/proc-uptime")"
	printf '%s\n' "$((now + ${2:-0}))" > "${R}/stop_at"
	timeout -k 1 60 sh "${ROOT}/exercise.sh" "$1" >> "${R}/out" 2>&1
}

check() {
	label="$1"
	shift
	if "$@"; then
		PASSED=$((PASSED + 1))
	else
		FAILED=$((FAILED + 1))
		printf 'not ok - %s\n' "${label}"
		[ ! -f "${R}/flexisip.calls" ] || sed 's/^/  flexisipsh: /' "${R}/flexisip.calls"
		[ ! -f "${R}/events" ] || sed 's/^/  event: /' "${R}/events"
		[ ! -f "${R}/out" ] || sed 's/^/  output: /' "${R}/out"
	fi
}

no_flexisip_call() { [ ! -s "${R}/flexisip.calls" ]; }
# The exact arguments matter: a third one would block on the passphrase FIFO.
one_call_without_fifo() { [ "$(cat "${R}/flexisip.calls" 2>/dev/null)" = "start 192.0.2.10" ]; }
decision_is() { [ "$(cat "${R}/var/run/companion-flexisip-decision" 2>/dev/null)" = "$1" ]; }
event_is() { grep -qx "$1" "${R}/events" 2>/dev/null; }

new_device 0 "${NEVER}" "${NEVER}"
run_init start
check "start does not start flexisip" no_flexisip_call
check "start starts the companion" event_is "start true at ${CLOCK_START}"
check "start starts the watchdog" event_is watchdog-start

new_device 1010 "${NEVER}" "${NEVER}"
run_init start
check "start waits for the LAN address before starting the companion" event_is "start true at 1010"

new_device "${NEVER}" "${NEVER}" "${NEVER}"
run_init start
check "start gives up on the LAN address after 30 s" event_is "start true at 1030"

new_device 0 "${NEVER}" "${NEVER}"
run_init watchdog 45
check "no fallback inside the grace period" no_flexisip_call

new_device 0 "${NEVER}" "${NEVER}"
run_init watchdog 90
check "fallback once the grace period ends, without the FIFO" one_call_without_fifo
check "the fallback is recorded" decision_is fallback

new_device 0 "${NEVER}" "${NEVER}"
run_init watchdog 600
check "a fallback that brings nothing up is not retried" one_call_without_fifo

new_device 1300 "${NEVER}" "${NEVER}"
run_init watchdog 330
check "the grace period counts from the LAN address, not from boot" no_flexisip_call
run_init watchdog 70
check "the grace period ends relative to the LAN address" one_call_without_fifo

new_device "${NEVER}" "${NEVER}" "${NEVER}"
run_init watchdog 600
check "no fallback without a LAN address" no_flexisip_call

new_device 0 1050 "${NEVER}"
run_init watchdog 300
check "no fallback when flexisip came up during the grace period" no_flexisip_call
check "a proxy found up is recorded" decision_is firmware

new_device 0 1010 1200
run_init watchdog 600
check "flexisip going down after the grace period is left to the firmware" no_flexisip_call

new_device 0 "${NEVER}" "${NEVER}"
run_init watchdog 30
run_init watchdog 45
check "a watchdog restart does not reset the grace period" one_call_without_fifo
run_init watchdog 300
check "a watchdog restart does not repeat the fallback" one_call_without_fifo

new_device 0 "${NEVER}" "${NEVER}"
rm -f "${R}/etc/init.d/flexisipsh"
run_init watchdog 90
check "a device without flexisipsh is left alone" decision_is ""

new_device "${NEVER}" "${NEVER}" "${NEVER}"
: > "${R}/unhealthy"
run_init watchdog 15
check "the watchdog still recovers the companion" event_is "recover at ${CLOCK_START}"

printf 'Flexisip lifecycle: %s passed, %s failed\n' "${PASSED}" "${FAILED}"
[ "${FAILED}" -eq 0 ]
