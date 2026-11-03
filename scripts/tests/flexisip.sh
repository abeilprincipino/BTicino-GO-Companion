#!/usr/bin/env sh
# Exercise the real init entrypoints with device paths and Companion operations
# sandboxed. No device, privileges, network access or Go toolchain required.
set -u

HERE="$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)"
ROOT="$(mktemp -d)"
trap 'rm -rf "${ROOT}"' EXIT HUP INT TERM
PASSED=0
FAILED=0

for scenario in ready-start missing-start ready-watchdog missing-watchdog; do
	network="${scenario%-*}"
	action="${scenario#*-}"
	for proxy in absent local tls; do
		for credential in missing fifo; do
			R="${ROOT}/${scenario}-${proxy}-${credential}"
			export R
			mkdir -p "${R}/etc/init.d" "${R}/var/run" "${R}/var/tmp" "${R}/shim"
			[ "${credential}" != fifo ] || mkfifo "${R}/var/tmp/bt_answering_machine.fifo"
			# Keep definitions, excluding only the final command dispatcher.
			sed -e '/^case "${1:-}" in/,$d' \
				-e "s|/etc/|${R}/etc/|g" \
				-e "s|/var/run/|${R}/var/run/|g" \
				-e "s|/var/tmp/|${R}/var/tmp/|g" \
				-e 's|^PATH=.*|PATH="'"${R}"'/shim:${PATH}"|' \
				"${HERE}/../init.d/companion" > "${R}/init.sh"
			# Log any start/stop/restart. A FIFO pathname alone is not proof that
			# firmware can supply a passphrase: deliberately provide no writer.
			printf '#!/bin/sh\nprintf "%%s\\n" "$*" >> "${R}/flexisip.calls"\n' > "${R}/etc/init.d/flexisipsh"
			printf '#!/bin/sh\ncat "${R}/listeners"\n' > "${R}/shim/netstat"
			if [ "${network}" = ready ]; then
				printf '#!/bin/sh\necho "    inet 192.0.2.10/24"\n' > "${R}/shim/ip"
			else
				printf '#!/bin/sh\nexit 1\n' > "${R}/shim/ip"
			fi
			chmod +x "${R}/etc/init.d/flexisipsh" "${R}/shim/netstat" "${R}/shim/ip"
			: > "${R}/listeners"
			if [ "${proxy}" != absent ]; then
				printf 'tcp 0 0 127.0.0.1:5060 0.0.0.0:* LISTEN\n' >> "${R}/listeners"
			fi
			if [ "${proxy}" = tls ]; then
				printf 'tcp 0 0 0.0.0.0:5061 0.0.0.0:* LISTEN\n' >> "${R}/listeners"
			fi
			cp "${R}/listeners" "${R}/listeners.before"
			# Fake only Companion lifecycle operations; leave the actual startup
			# and watchdog control flow (including Flexisip decisions) intact.
			cat > "${R}/exercise.sh" <<'SH'
. "${R}/init.sh"
wait_for_binary() { return 0; }
wait_for_lan_ip() { echo lan-wait >> "${R}/events"; }
with_lifecycle_lock() { echo "$*" >> "${R}/events"; }
start_watchdog() { echo watchdog-start >> "${R}/events"; }
companion_healthy() { return 1; }
sleep() { rm -f "${WATCHDOG_ENABLE_FILE}"; }
if [ "$1" = start ]; then
	do_start
else
	: > "${WATCHDOG_ENABLE_FILE}"
	do_watchdog
fi
SH
			# A regression that opens an unserviced FIFO must fail boundedly,
			# rather than hanging the regression suite indefinitely.
			if timeout -k 1 3 sh "${R}/exercise.sh" "${action}" > "${R}/out" 2>&1 &&
				[ ! -s "${R}/flexisip.calls" ] &&
				! grep -q '^lan-wait$' "${R}/events" &&
				cmp -s "${R}/listeners.before" "${R}/listeners" &&
				{ [ "${action}" != start ] || { grep -qx 'start true' "${R}/events" && grep -qx watchdog-start "${R}/events"; }; } &&
				{ [ "${action}" != watchdog ] || grep -qx recover "${R}/events"; }; then
				PASSED=$((PASSED + 1))
			else
				FAILED=$((FAILED + 1))
				printf 'not ok - %s defers to firmware (IP=%s, proxy=%s, credential=%s)\n' "${action}" "${network}" "${proxy}" "${credential}"
				[ ! -f "${R}/flexisip.calls" ] || cat "${R}/flexisip.calls"
				cat "${R}/out"
			fi
		done
	done
done

printf 'Flexisip lifecycle: %s passed, %s failed\n' "${PASSED}" "${FAILED}"
[ "${FAILED}" -eq 0 ]
