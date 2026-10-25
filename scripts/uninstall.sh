#!/usr/bin/env sh
set -eu

# Removes what install.sh put on the intercom. It is fetched and run on its own,
# like the installer, so it carries its own copy of the installer's helpers.

SERVICE_NAME="companion"
BASE_DIR="/home/bticino/cfg/extra/companion"
INIT_SCRIPT="/etc/init.d/${SERVICE_NAME}"
FIREWALL_HOOK="/etc/network/if-pre-up.d/iptables"
FIREWALL_ADDED="${BASE_DIR}/firewall-added"
COMPANION_TCP_PORTS="8080 80 8554 51826"
FLEXISIP_USERS_DB="/etc/flexisip/users/users.db.txt"
FLEXISIP_ROUTE="/etc/flexisip/users/route.conf"
FLEXISIP_ROUTE_INT="/etc/flexisip/users/route_int.conf"
FLEXISIP_ROUTE_EXT="/etc/flexisip/users/route_ext.conf"
SIP_USER="companion"
# The installer's lock, so that a removal cannot run during an installation.
LOCK_DIR="/var/run/${SERVICE_NAME}-install.lock"

ROOT_WAS_REMOUNTED=0
LOCK_HELD=0
FAILURES=0
CURRENT_STEP="starting"
ABORT_REPORTED=0
RUN_FINISHED=0

# Writes are best-effort and the run carries on if the session that started it
# goes away, as in install.sh.
log() { printf 'INFO: %s\n' "$*" 2>/dev/null || true; }
ok() { printf 'OK: %s\n' "$*" 2>/dev/null || true; }
warn() { printf 'WARN: %s\n' "$*" 2>/dev/null || true; }
fail() { printf 'FAIL: %s\n' "$*" 2>/dev/null || true; FAILURES=$((FAILURES + 1)); }

die() {
	printf 'FAIL: %s\n' "$*" >&2 2>/dev/null || true
	ABORT_REPORTED=1
	exit 1
}

restore_root_ro() {
	if [ "${ROOT_WAS_REMOUNTED}" -eq 1 ]; then
		mount -o remount,ro / || true
		ROOT_WAS_REMOUNTED=0
		log "Restored / as read-only."
	fi
}

remount_root_rw() {
	if [ "${ROOT_WAS_REMOUNTED}" -eq 0 ]; then
		mount -o remount,rw /
		ROOT_WAS_REMOUNTED=1
		log "Remounted / as read-write."
	fi
}

acquire_install_lock() {
	if ! mkdir "${LOCK_DIR}" 2>/dev/null; then
		holder="$(cat "${LOCK_DIR}/pid" 2>/dev/null || true)"
		if [ -n "${holder}" ] && [ -d "/proc/${holder}" ]; then
			die "Another installation or removal is already running (pid ${holder})."
		fi
		rm -rf "${LOCK_DIR}"
		mkdir "${LOCK_DIR}" || die "Could not create the lock ${LOCK_DIR}."
	fi
	echo "$$" > "${LOCK_DIR}/pid"
	LOCK_HELD=1
}

release_install_lock() {
	[ "${LOCK_HELD}" -eq 1 ] || return 0
	rm -rf "${LOCK_DIR}"
	LOCK_HELD=0
}

on_exit() {
	rc=$?
	set +e
	restore_root_ro
	release_install_lock
	if [ "${rc}" -ne 0 ] && [ "${RUN_FINISHED}" -eq 0 ] && [ "${ABORT_REPORTED}" -eq 0 ]; then
		printf 'FAIL: Removal aborted while %s (exit %s).\n' "${CURRENT_STEP}" "${rc}" >&2 2>/dev/null
	fi
}
trap on_exit EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
trap '' HUP PIPE

require_root() {
	[ "$(id -u)" -eq 0 ] || die "This script must run as root."
}

stop_service() {
	if [ ! -x "${INIT_SCRIPT}" ]; then
		log "No init script at ${INIT_SCRIPT}; nothing to stop."
		return 0
	fi
	if "${INIT_SCRIPT}" stop; then ok "Stopped ${SERVICE_NAME}."; else fail "Could not stop ${SERVICE_NAME}."; fi
}

remove_service_files() {
	before="${FAILURES}"
	# install.sh links both S45 (before the BTicino stack) and S99zz.
	for runlevel in 2 3 4 5; do
		for link in "/etc/rc${runlevel}.d/S99zz${SERVICE_NAME}" "/etc/rc${runlevel}.d/S45${SERVICE_NAME}"; do
			rm -f "${link}" || fail "Could not remove ${link}."
		done
	done
	for runlevel in 0 1 6; do rm -f "/etc/rc${runlevel}.d/K55${SERVICE_NAME}" || fail "Could not remove /etc/rc${runlevel}.d/K55${SERVICE_NAME}."; done
	rm -f "${INIT_SCRIPT}" || fail "Could not remove ${INIT_SCRIPT}."
	[ "${FAILURES}" -ne "${before}" ] || ok "Removed the init script and its boot links."
}

remove_firewall_tcp_ports() {
	if [ ! -f "${FIREWALL_ADDED}" ]; then
		warn "No record of the TCP ports the installer added; ports ${COMPANION_TCP_PORTS} may still be open in ${FIREWALL_HOOK}."
		return 0
	fi
	ports="$(tr '\n' ' ' < "${FIREWALL_ADDED}")"
	ports="${ports% }"
	if [ -z "${ports}" ]; then
		log "No TCP ports were recorded as added; nothing to remove from ${FIREWALL_HOOK}."
		return 0
	fi
	tmp="${FIREWALL_HOOK}.tmp.$$"
	rc=0
	awk -v ports="${ports}" '
		BEGIN { n = split(ports, p, /[[:space:]]+/); for (i = 1; i <= n; i++) if (p[i] != "") drop[p[i]] = 1 }
		/# ssh \& sip enabled/ { inblock = 1; print; next }
		inblock && /^[[:space:]]*for i in .*; do[[:space:]]*$/ {
			lead = $0; sub(/for i in.*/, "", lead)
			line = $0; sub(/^[[:space:]]*for i in[[:space:]]*/, "", line); sub(/[[:space:]]*; do[[:space:]]*$/, "", line)
			m = split(line, a, /[[:space:]]+/); out = lead "for i in"; kept = 0
			for (i = 1; i <= m; i++) if (a[i] != "" && !(a[i] in drop)) { out = out " " a[i]; kept++ }
			if (kept == 0) { emptied = 1; exit }
			print out "; do"; patched = 1; inblock = 0; next
		}
		{ print }
		END { if (emptied) exit 43; if (!patched) exit 42 }
	' "${FIREWALL_HOOK}" > "${tmp}" || rc=$?
	case "${rc}" in
	0) if cp "${tmp}" "${FIREWALL_HOOK}"; then ok "Removed TCP ports ${ports} from ${FIREWALL_HOOK}."; else fail "Could not write ${FIREWALL_HOOK}."; fi ;;
	42) warn "Could not find the SSH/SIP firewall block in ${FIREWALL_HOOK}; TCP ports left unchanged." ;;
	43) warn "Removing ports ${ports} would empty the SSH/SIP port list in ${FIREWALL_HOOK}; left unchanged." ;;
	*) fail "Could not edit ${FIREWALL_HOOK}." ;;
	esac
	rm -f "${tmp}"
}

remove_firewall_udp_blocks() {
	if ! grep -q '^# companion udp service$' "${FIREWALL_HOOK}"; then
		log "No companion UDP rules in ${FIREWALL_HOOK}."
		return 0
	fi
	tmp="${FIREWALL_HOOK}.tmp.$$"
	# Each block is the marker, its iptables line and the blank line install.sh
	# puts after it.
	if awk '
		/^# companion udp service$/ { skip = 1; next }
		skip == 1 { skip = 2; next }
		skip == 2 { skip = 0; if ($0 ~ /^[[:space:]]*$/) next }
		{ print }
	' "${FIREWALL_HOOK}" > "${tmp}" && cp "${tmp}" "${FIREWALL_HOOK}"; then
		ok "Removed the companion UDP rules from ${FIREWALL_HOOK}."
	else
		fail "Could not remove the companion UDP rules from ${FIREWALL_HOOK}."
	fi
	rm -f "${tmp}"
}

remove_firewall_entries() {
	if [ ! -f "${FIREWALL_HOOK}" ]; then
		log "${FIREWALL_HOOK} not found; no firewall entries to remove."
		return 0
	fi
	remove_firewall_tcp_ports
	remove_firewall_udp_blocks
}

remove_from_route() {
	route_file="$1"
	grep -q "sip:${SIP_USER}@" "${route_file}" || return 0
	tmp="${route_file}.companion.tmp"
	# Besides the alluser contact, installers before the static record was dropped
	# wrote a whole `<sip:USER@domain> <sip:127.0.0.1>` line; that line goes too.
	sed -e "s|, <sip:${SIP_USER}@[^>]*>||g" -e "/^<sip:${SIP_USER}@[^>]*>[[:space:]]*<sip:127\.0\.0\.1>[[:space:]]*$/d" "${route_file}" > "${tmp}"
	# A route file must stay one line per group: a mangled file breaks every
	# call on the intercom, so the result is checked before it is used.
	groups_before="$(grep -c 'sip:alluser@' "${route_file}" || true)"
	groups_after="$(grep -c 'sip:alluser@' "${tmp}" || true)"
	if [ "${groups_before}" = "${groups_after}" ] && ! grep -q "sip:${SIP_USER}@" "${tmp}" && cp "${tmp}" "${route_file}"; then
		log "Removed ${SIP_USER} from ${route_file}."
	else
		fail "${route_file} rewrite failed validation; original left untouched."
	fi
	rm -f "${tmp}"
}

remove_sip_user() {
	if [ ! -f "${FLEXISIP_USERS_DB}" ] || [ ! -f "${FLEXISIP_ROUTE}" ]; then
		log "Flexisip user files not found; no SIP user to remove."
		return 0
	fi
	before="${FAILURES}"

	if grep -q "^${SIP_USER}@" "${FLEXISIP_USERS_DB}"; then
		tmp="${FLEXISIP_USERS_DB}.companion.tmp"
		if awk -v user="${SIP_USER}@" 'index($0, user) != 1' "${FLEXISIP_USERS_DB}" > "${tmp}" && cp "${tmp}" "${FLEXISIP_USERS_DB}"; then
			log "Removed ${SIP_USER} from ${FLEXISIP_USERS_DB}."
		else
			fail "Could not remove ${SIP_USER} from ${FLEXISIP_USERS_DB}."
		fi
		rm -f "${tmp}"
	fi

	# install.sh writes route.conf and route_int.conf; older installers also left
	# the static record, which on some intercoms sits in route_ext.conf.
	for route_file in "${FLEXISIP_ROUTE}" "${FLEXISIP_ROUTE_INT}" "${FLEXISIP_ROUTE_EXT}"; do
		[ ! -f "${route_file}" ] || remove_from_route "${route_file}"
	done

	# The backups predate anything BTicino changed since the installation, so
	# restoring them could undo it; they are dropped instead.
	rm -f "${FLEXISIP_USERS_DB}.companion.bak" "${FLEXISIP_ROUTE}.companion.bak" "${FLEXISIP_ROUTE_INT}.companion.bak"
	[ "${FAILURES}" -ne "${before}" ] || ok "Removed the ${SIP_USER} SIP user."
}

remove_files() {
	if rm -rf "${BASE_DIR}"; then ok "Removed ${BASE_DIR}."; else fail "Could not remove ${BASE_DIR}."; fi
}

main() {
	require_root
	acquire_install_lock
	log "Starting companion removal"

	CURRENT_STEP="stopping the service"
	stop_service
	CURRENT_STEP="remounting / read-write"
	remount_root_rw
	CURRENT_STEP="removing the init service"
	remove_service_files
	CURRENT_STEP="removing the firewall entries"
	remove_firewall_entries
	CURRENT_STEP="removing the SIP user"
	remove_sip_user
	restore_root_ro
	CURRENT_STEP="removing ${BASE_DIR}"
	remove_files

	RUN_FINISHED=1
	[ "${FAILURES}" -eq 0 ] || { log "Removal finished with ${FAILURES} failure(s)."; exit 1; }
	ok "Companion removed. Restart Flexisip or reboot the intercom."
}

main "$@"
