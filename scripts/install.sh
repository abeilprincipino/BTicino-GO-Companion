#!/usr/bin/env sh
set -eu

SERVICE_NAME="companion"
BASE_DIR="/home/bticino/cfg/extra/companion"
BIN_PATH="${BASE_DIR}/companion"
INIT_SCRIPT="/etc/init.d/${SERVICE_NAME}"

SCRIPT_DIR="$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)"
INIT_TEMPLATE="${SCRIPT_DIR}/init.d/companion"

ROOT_WAS_REMOUNTED=0
FAILURES=0

log() {
	printf '%s\n' "$*"
}

ok() {
	printf 'OK: %s\n' "$*"
}

fail() {
	printf 'FAIL: %s\n' "$*"
	FAILURES=$((FAILURES + 1))
}

require_root() {
	if [ "$(id -u)" -ne 0 ]; then
		log "This script must run as root."
		exit 1
	fi
}

remount_root_rw() {
	if [ "${ROOT_WAS_REMOUNTED}" -eq 0 ]; then
		mount -o remount,rw /
		ROOT_WAS_REMOUNTED=1
		log "Remounted / as read-write."
	fi
}

restore_root_ro() {
	if [ "${ROOT_WAS_REMOUNTED}" -eq 1 ]; then
		mount -o remount,ro / || true
		ROOT_WAS_REMOUNTED=0
		log "Restored / as read-only."
	fi
}

on_exit() {
	restore_root_ro
}

trap on_exit EXIT INT TERM

detect_binary() {
	if [ "${1:-}" != "" ] && [ -f "$1" ]; then
		printf '%s\n' "$1"
		return 0
	fi
	return 1
}

companion_firewall_ports() {
	printf '%s\n' "8080 8554"
}

ensure_persistent_firewall_port_value() {
	hook="/etc/network/if-pre-up.d/iptables"
	port="$1"

	if [ ! -f "${hook}" ]; then
		log "Warning: ${hook} not found, skipping persistent firewall patch."
		return 0
	fi

	if awk -v port="${port}" '
		/# ssh \& sip enabled/ { inblock=1; next }
		inblock==1 && /^[[:space:]]*for i in .*; do[[:space:]]*$/ {
			line=$0
			sub(/^[[:space:]]*for i in[[:space:]]*/, "", line)
			sub(/[[:space:]]*; do[[:space:]]*$/, "", line)
			n=split(line, a, /[[:space:]]+/)
			for (i=1; i<=n; i++) if (a[i] == port) found=1
			inblock=0
		}
		END { exit(found ? 0 : 1) }
	' "${hook}"; then
		log "Persistent firewall already allows TCP ${port}."
		return 0
	fi

	tmp="${hook}.tmp.$$"
	if awk -v port="${port}" '
		BEGIN { patched=0; inblock=0 }
		{
			if ($0 ~ /# ssh \& sip enabled/) {
				inblock=1
				print
				next
			}
			if (inblock==1 && $0 ~ /^[[:space:]]*for i in .*; do[[:space:]]*$/) {
				line=$0
				sub(/^[[:space:]]*for i in[[:space:]]*/, "", line)
				sub(/[[:space:]]*; do[[:space:]]*$/, "", line)
				n=split(line, a, /[[:space:]]+/)
				out="for i in"
				has=0
				for (i=1; i<=n; i++) {
					if (a[i] == "") continue
					if (a[i] == port) has=1
					out=out " " a[i]
				}
				if (!has) out=out " " port
				print out "; do"
				patched=1
				inblock=0
				next
			}
			print
		}
		END { if (!patched) exit 42 }
	' "${hook}" > "${tmp}"; then
		cp "${tmp}" "${hook}"
		rm -f "${tmp}"
		log "Persisted companion firewall port ${port} in ${hook}."
		return 0
	fi

	rc=$?
	rm -f "${tmp}"
	if [ "${rc}" -eq 42 ]; then
		log "Warning: could not find SSH/SIP firewall block in ${hook}; no persistent patch applied."
		return 0
	fi
	log "Warning: failed to patch ${hook} for companion port ${port}."
	return 0
}

ensure_persistent_firewall_ports() {
	for port in $(companion_firewall_ports); do
		ensure_persistent_firewall_port_value "${port}"
	done
}

install_binary() {
	src="$1"
	mkdir -p "${BASE_DIR}"
	candidate="${BASE_DIR}/companion.candidate.$$"

	cp "${src}" "${candidate}"
	chmod 755 "${candidate}"

	if [ -f "${BIN_PATH}" ]; then
		cp -f "${BIN_PATH}" "${BASE_DIR}/companion.previous"
		chmod 755 "${BASE_DIR}/companion.previous" || true
	fi

	mv -f "${candidate}" "${BIN_PATH}"
	log "Installed binary to ${BIN_PATH}"
}

install_init_script() {
	if [ ! -f "${INIT_TEMPLATE}" ]; then
		log "Missing init template: ${INIT_TEMPLATE}"
		exit 1
	fi
	cp -f "${INIT_TEMPLATE}" "${INIT_SCRIPT}"
	chmod 755 "${INIT_SCRIPT}"
}

register_service() {
	remount_root_rw
	install_init_script

	for runlevel in 2 3 4 5; do
		dir="/etc/rc${runlevel}.d"
		link="${dir}/S45${SERVICE_NAME}"
		if [ -d "${dir}" ]; then
			rm -f "${link}"
			ln -s "../init.d/${SERVICE_NAME}" "${link}"
		fi
	done

	for runlevel in 0 1 6; do
		dir="/etc/rc${runlevel}.d"
		link="${dir}/K55${SERVICE_NAME}"
		if [ -d "${dir}" ]; then
			rm -f "${link}"
			ln -s "../init.d/${SERVICE_NAME}" "${link}"
		fi
	done

	ensure_persistent_firewall_ports
	log "Registered init service ${SERVICE_NAME}"
}

start_service() {
	if [ -x "${INIT_SCRIPT}" ]; then
		"${INIT_SCRIPT}" restart || "${INIT_SCRIPT}" start
	fi
}

health_url() {
	printf '%s\n' "http://127.0.0.1:8080/api/v2/health"
}

post_install_checks() {
	FAILURES=0
	pidfile="/var/run/${SERVICE_NAME}.pid"

	if [ -x "${BIN_PATH}" ]; then
		ok "Binary exists: ${BIN_PATH}"
	else
		fail "Binary missing or not executable: ${BIN_PATH}"
	fi

	if [ -x "${INIT_SCRIPT}" ]; then
		ok "Init script present: ${INIT_SCRIPT}"
	else
		fail "Init script missing: ${INIT_SCRIPT}"
	fi

	if [ -L "/etc/rc5.d/S45${SERVICE_NAME}" ]; then
		ok "Boot symlink present: /etc/rc5.d/S45${SERVICE_NAME}"
	else
		fail "Boot symlink missing: /etc/rc5.d/S45${SERVICE_NAME}"
	fi

	if [ -x "${INIT_SCRIPT}" ] && "${INIT_SCRIPT}" status >/dev/null 2>&1; then
		ok "Service is running"
	elif [ -f "${pidfile}" ]; then
		pid="$(cat "${pidfile}" 2>/dev/null || true)"
		if [ -n "${pid}" ] && [ -d "/proc/${pid}" ]; then
			ok "Service process exists via pidfile ${pidfile} (pid ${pid})"
		else
			fail "Service pidfile exists but process is not running: ${pidfile}"
		fi
	else
		fail "Service not running"
	fi

	url="$(health_url)"
	if command -v curl >/dev/null 2>&1; then
		if curl -fsS --max-time 3 "${url}" >/dev/null 2>&1; then
			ok "Health endpoint reachable at ${url}"
		else
			fail "Health endpoint not reachable at ${url}"
		fi
	elif command -v wget >/dev/null 2>&1; then
		if wget -q -T 3 -O /dev/null "${url}" >/dev/null 2>&1; then
			ok "Health endpoint reachable at ${url}"
		else
			fail "Health endpoint not reachable at ${url}"
		fi
	else
		fail "Neither curl nor wget available for health check"
	fi

	if awk '$2=="/"{print $4}' /proc/mounts | grep -Eq '(^|,)ro(,|$)'; then
		ok "Root filesystem is read-only"
	else
		fail "Root filesystem is not read-only"
	fi

	if [ "${FAILURES}" -ne 0 ]; then
		log "Post-install checks completed with ${FAILURES} failure(s)."
	else
		log "Post-install checks passed."
	fi
}

main() {
	require_root

	binary_path="$(detect_binary "${1:-}" || true)"
	if [ -z "${binary_path}" ]; then
		log "Usage: $0 /path/to/companion-binary"
		log "No binary path was provided."
		exit 1
	fi

	install_binary "${binary_path}"
	register_service
	start_service
	restore_root_ro
	post_install_checks
	log "Installation complete."
}

main "$@"
