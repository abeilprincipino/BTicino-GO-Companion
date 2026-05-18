#!/usr/bin/env sh
set -eu

SERVICE_NAME="companion"
BASE_DIR="/home/bticino/cfg/extra/companion"
BIN_PATH="${BASE_DIR}/companion"
INIT_SCRIPT="/etc/init.d/${SERVICE_NAME}"

SCRIPT_DIR="$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)"
LOCAL_INIT_TEMPLATE="${SCRIPT_DIR}/init.d/companion"

REPO="${COMPANION_RELEASE_REPO:-}"
BUNDLE_ASSET="${COMPANION_RELEASE_BUNDLE_ASSET:-companion.tar.gz}"
CHECKSUM_ASSET="${COMPANION_RELEASE_CHECKSUM_ASSET:-companion.sha256}"
BINARY_NAME="${COMPANION_RELEASE_BINARY_ASSET:-companion}"

BASE_URL="${COMPANION_RELEASE_BASE_URL:-}"
if [ -z "${BASE_URL}" ] && [ -n "${REPO}" ]; then
	BASE_URL="https://github.com/${REPO}/releases/latest/download"
fi

INIT_TEMPLATE_URL="${COMPANION_INIT_TEMPLATE_URL:-}"
if [ -z "${INIT_TEMPLATE_URL}" ] && [ -n "${REPO}" ]; then
	INIT_TEMPLATE_URL="https://raw.githubusercontent.com/${REPO}/main/scripts/init.d/companion"
fi

ROOT=""
ROOT_WAS_REMOUNTED=0
FAILURES=0
SELECTED_BINARY_PATH=""
SELECTED_INIT_TEMPLATE=""

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

cleanup_download_dir() {
	if [ -n "${ROOT}" ] && [ -d "${ROOT}" ]; then
		rm -rf "${ROOT}" || true
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
	cleanup_download_dir
	restore_root_ro
}

trap on_exit EXIT INT TERM

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

fetch() {
	url="$1"
	dst="$2"
	if command -v wget >/dev/null 2>&1; then
		wget -qO "${dst}" "${url}"
		return 0
	fi
	if command -v curl >/dev/null 2>&1; then
		curl -fsSL -o "${dst}" "${url}"
		return 0
	fi
	log "Neither wget nor curl is available."
	exit 1
}

sha256_file() {
	target="$1"
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "${target}" | awk '{print $1}'
		return 0
	fi
	if command -v busybox >/dev/null 2>&1; then
		busybox sha256sum "${target}" | awk '{print $1}'
		return 0
	fi
	if command -v openssl >/dev/null 2>&1; then
		openssl dgst -sha256 "${target}" | awk '{print $NF}'
		return 0
	fi
	log "No SHA256 tool found (sha256sum/busybox/openssl)."
	exit 1
}

parse_expected_sha() {
	checksum_file="$1"
	awk '{
		for (i = 1; i <= NF; i++) {
			v = tolower($i)
			if (length(v) != 64) {
				continue
			}
			x = v
			gsub(/[0-9a-f]/, "", x)
			if (x == "") {
				print v
				exit
			}
		}
	}' "${checksum_file}"
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
	init_template="$1"
	if [ ! -f "${init_template}" ]; then
		log "Missing init template: ${init_template}"
		exit 1
	fi
	cp -f "${init_template}" "${INIT_SCRIPT}"
	chmod 755 "${INIT_SCRIPT}"
}

register_service() {
	init_template="$1"
	remount_root_rw
	install_init_script "${init_template}"

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

resolve_local_install_inputs() {
	binary_path="$1"
	init_template="$2"
	if [ -z "${binary_path}" ] || [ ! -f "${binary_path}" ]; then
		log "Missing companion binary for install."
		exit 1
	fi
	if [ -z "${init_template}" ] || [ ! -f "${init_template}" ]; then
		log "Missing init template for install."
		exit 1
	fi
	SELECTED_BINARY_PATH="${binary_path}"
	SELECTED_INIT_TEMPLATE="${init_template}"
}

download_latest_artifacts() {
	if [ -z "${BASE_URL}" ]; then
		log "Set COMPANION_RELEASE_REPO or COMPANION_RELEASE_BASE_URL."
		exit 1
	fi

	ROOT="/tmp/companion-install.$$"
	mkdir -p "${ROOT}"
	log "Downloading latest release bundle..."

	bundle_dir="${ROOT}/companion"
	binary_path=""
	init_template_path=""

	if fetch "${BASE_URL}/${BUNDLE_ASSET}" "${ROOT}/${BUNDLE_ASSET}" 2>/dev/null && fetch "${BASE_URL}/${CHECKSUM_ASSET}" "${ROOT}/${CHECKSUM_ASSET}" && tar -xzf "${ROOT}/${BUNDLE_ASSET}" -C "${ROOT}" >/dev/null 2>&1; then
		candidate_binary="${bundle_dir}/${BINARY_NAME}"
		candidate_init="${bundle_dir}/init.d/companion"
		if [ -f "${candidate_binary}" ] && [ -f "${candidate_init}" ]; then
			binary_path="${candidate_binary}"
			init_template_path="${candidate_init}"
		else
			log "Bundle is incomplete, falling back to binary+template download."
		fi
	fi

	if [ -z "${binary_path}" ] || [ -z "${init_template_path}" ]; then
		log "Bundle asset not found on latest release yet, falling back to binary+template download."
		if [ -z "${INIT_TEMPLATE_URL}" ]; then
			log "Fallback requires COMPANION_INIT_TEMPLATE_URL (or COMPANION_RELEASE_REPO)."
			exit 1
		fi
		mkdir -p "${bundle_dir}/init.d"
		fetch "${BASE_URL}/${BINARY_NAME}" "${bundle_dir}/${BINARY_NAME}"
		fetch "${BASE_URL}/${CHECKSUM_ASSET}" "${ROOT}/${CHECKSUM_ASSET}"
		fetch "${INIT_TEMPLATE_URL}" "${bundle_dir}/init.d/companion"
		binary_path="${bundle_dir}/${BINARY_NAME}"
		init_template_path="${bundle_dir}/init.d/companion"
	fi

	expected_sha="$(parse_expected_sha "${ROOT}/${CHECKSUM_ASSET}")"
	if [ -z "${expected_sha}" ]; then
		log "Could not parse expected SHA256 from ${CHECKSUM_ASSET}."
		exit 1
	fi
	actual_sha="$(sha256_file "${binary_path}")"
	if [ "${actual_sha}" != "${expected_sha}" ]; then
		log "SHA256 mismatch for ${BINARY_NAME}."
		log "Expected: ${expected_sha}"
		log "Actual:   ${actual_sha}"
		exit 1
	fi

	chmod 755 "${binary_path}" "${init_template_path}"
	SELECTED_BINARY_PATH="${binary_path}"
	SELECTED_INIT_TEMPLATE="${init_template_path}"
}

main() {
	require_root

	input_binary="${1:-}"
	if [ -n "${input_binary}" ]; then
		resolve_local_install_inputs "${input_binary}" "${LOCAL_INIT_TEMPLATE}"
	else
		download_latest_artifacts
	fi

	install_binary "${SELECTED_BINARY_PATH}"
	register_service "${SELECTED_INIT_TEMPLATE}"
	start_service
	restore_root_ro
	post_install_checks
	log "Installation complete."
}

main "$@"
