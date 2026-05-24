#!/usr/bin/env sh
set -eu

SERVICE_NAME="companion"
BASE_DIR="/home/bticino/cfg/extra/companion"
BIN_PATH="${BASE_DIR}/companion"
INIT_SCRIPT="/etc/init.d/${SERVICE_NAME}"

SCRIPT_DIR="$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)"
LOCAL_INIT_TEMPLATE="${SCRIPT_DIR}/init.d/companion"

DEFAULT_RELEASE_REPO="r0bb10/BTicino-GO-Companion"
REPO="${COMPANION_RELEASE_REPO:-${DEFAULT_RELEASE_REPO}}"
BUNDLE_ASSET="${COMPANION_RELEASE_BUNDLE_ASSET:-companion.tar.gz}"
HEALTHCHECK_TIMEOUT_SEC="${COMPANION_HEALTHCHECK_TIMEOUT_SEC:-45}"

BASE_URL="${COMPANION_RELEASE_BASE_URL:-}"
if [ -z "${BASE_URL}" ] && [ -n "${REPO}" ]; then
	BASE_URL="https://github.com/${REPO}/releases/latest/download"
fi
RELEASE_API="${COMPANION_RELEASE_API:-}"
if [ -z "${RELEASE_API}" ] && [ -n "${REPO}" ]; then
	RELEASE_API="https://api.github.com/repos/${REPO}/releases/latest"
fi

INIT_TEMPLATE_URL="${COMPANION_INIT_TEMPLATE_URL:-}"
if [ -z "${INIT_TEMPLATE_URL}" ] && [ -n "${REPO}" ]; then
	INIT_TEMPLATE_URL="https://raw.githubusercontent.com/${REPO}/main/scripts/init.d/companion"
fi

ROOT=""
ROOT_WAS_REMOUNTED=0
FAILURES=0
POST_CHECK_FAILURES=0
SELECTED_BINARY_PATH=""
SELECTED_INIT_TEMPLATE=""
SELECTED_GST_DIR=""

log() {
	printf 'INFO: %s\n' "$*"
}

ok() {
	printf 'OK: %s\n' "$*"
}

warn() {
	printf 'WARN: %s\n' "$*"
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

parse_release_asset_digest() {
	release_json="$1"
	asset_name="$2"
	awk -v asset="${asset_name}" '
		BEGIN { want = 0 }
		{
			if ($0 ~ /"name"[[:space:]]*:/ && index($0, "\"" asset "\"") > 0) {
				want = 1
			}
			if (want && $0 ~ /"digest"[[:space:]]*:/) {
				line = $0
				sub(/.*"digest"[[:space:]]*:[[:space:]]*"sha256:/, "", line)
				sub(/".*/, "", line)
				if (line != "") {
					print tolower(line)
					exit
				}
			}
		}
	' "${release_json}"
}

resolve_bundle_sha() {
	override_sha="${COMPANION_RELEASE_BUNDLE_SHA256:-}"
	if [ -n "${override_sha}" ]; then
		printf '%s' "${override_sha}" | tr 'A-F' 'a-f'
		return 0
	fi
	if [ -z "${RELEASE_API}" ]; then
		return 1
	fi
	release_json="${ROOT}/release.json"
	if ! fetch "${RELEASE_API}" "${release_json}" 2>/dev/null; then
		return 1
	fi
	parse_release_asset_digest "${release_json}" "${BUNDLE_ASSET}"
}

companion_firewall_ports() {
	printf '%s\n' "8080 8554"
}

ensure_persistent_firewall_port_value() {
	hook="/etc/network/if-pre-up.d/iptables"
	port="$1"

	if [ ! -f "${hook}" ]; then
		warn "${hook} not found, skipping persistent firewall patch."
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
		log "Persistent firewall already allows companion port ${port}."
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
		warn "could not find SSH/SIP firewall block in ${hook}; no persistent patch applied."
		return 0
	fi
	warn "failed to patch ${hook} for companion port ${port}."
	return 0
}

ensure_persistent_firewall_udp_port_value() {
	hook="/etc/network/if-pre-up.d/iptables"
	port="$1"

	if [ ! -f "${hook}" ]; then
		warn "${hook} not found, skipping persistent firewall patch."
		return 0
	fi

	if grep -Eq "udp.*--dport[[:space:]]+${port}.*-j[[:space:]]+ACCEPT" "${hook}"; then
		log "Persistent firewall already allows UDP ${port}."
		return 0
	fi

	tmp="${hook}.tmp.$$"
	if awk -v port="${port}" '
		BEGIN { patched=0 }
		/^#disable all other stuff/ && !patched {
			print "# companion mdns discovery"
			print "iptables -A INPUT -p udp -m udp --dport " port " -j ACCEPT"
			print ""
			patched=1
		}
		{ print }
		END { if (!patched) exit 42 }
	' "${hook}" > "${tmp}"; then
		cp "${tmp}" "${hook}"
		rm -f "${tmp}"
		log "Persisted companion mDNS firewall port ${port} in ${hook}."
		return 0
	fi

	rc=$?
	rm -f "${tmp}"
	if [ "${rc}" -eq 42 ]; then
		warn "could not find firewall policy marker in ${hook}; no mDNS persistent patch applied."
		return 0
	fi
	warn "failed to patch ${hook} for companion mDNS port ${port}."
	return 0
}

ensure_persistent_firewall_ports() {
	for port in $(companion_firewall_ports); do
		ensure_persistent_firewall_port_value "${port}"
	done
	ensure_persistent_firewall_udp_port_value 5353
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

install_gst_runtime() {
	src_dir="$1"
	if [ -z "${src_dir}" ] || [ ! -d "${src_dir}" ]; then
		log "No bundled gst runtime provided; keeping existing runtime."
		return 0
	fi

	mkdir -p "${BASE_DIR}"
	candidate="${BASE_DIR}/gst.candidate.$$"
	previous="${BASE_DIR}/gst.previous"

	rm -rf "${candidate}"
	mkdir -p "${candidate}"
	cp -a "${src_dir}/." "${candidate}/"

	rm -rf "${previous}"
	if [ -d "${BASE_DIR}/gst" ]; then
		mv -f "${BASE_DIR}/gst" "${previous}"
	fi
	mv -f "${candidate}" "${BASE_DIR}/gst"
	log "Installed gst runtime to ${BASE_DIR}/gst"
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

health_endpoint_reachable() {
	url="$1"
	if command -v curl >/dev/null 2>&1; then
		curl -fsS --max-time 3 "${url}" >/dev/null 2>&1
		return $?
	fi
	if command -v wget >/dev/null 2>&1; then
		wget -q -T 3 -O /dev/null "${url}" >/dev/null 2>&1
		return $?
	fi
	return 127
}

wait_for_health() {
	url="$1"
	max_wait_sec="$2"
	elapsed=0
	while [ "${elapsed}" -lt "${max_wait_sec}" ]; do
		if health_endpoint_reachable "${url}"; then
			return 0
		fi
		sleep 1
		elapsed=$((elapsed + 1))
	done
	return 1
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

	if [ -x "${BASE_DIR}/gst/bin/gst-launch-1.0" ] || [ -x "${BASE_DIR}/gst/opt/gst14/bin/gst-launch-1.0" ]; then
		ok "GStreamer launcher is present"
	else
		fail "GStreamer launcher missing in ${BASE_DIR}/gst"
	fi

	url="$(health_url)"
	if command -v curl >/dev/null 2>&1 || command -v wget >/dev/null 2>&1; then
		log "Waiting for health endpoint (up to ${HEALTHCHECK_TIMEOUT_SEC}s): ${url}"
		if wait_for_health "${url}" "${HEALTHCHECK_TIMEOUT_SEC}"; then
			ok "Health endpoint reachable at ${url}"
		else
			fail "Health endpoint not reachable at ${url} after ${HEALTHCHECK_TIMEOUT_SEC}s"
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
	POST_CHECK_FAILURES="${FAILURES}"
}

resolve_local_install_inputs() {
	binary_path="$1"
	init_template="$2"
	local_gst_dir="${SCRIPT_DIR}/../gst"
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
	if [ -d "${local_gst_dir}" ]; then
		SELECTED_GST_DIR="${local_gst_dir}"
	fi
}

download_latest_artifacts() {
	if [ -z "${BASE_URL}" ]; then
		log "Set COMPANION_RELEASE_BASE_URL (or override COMPANION_RELEASE_REPO)."
		exit 1
	fi

	ROOT="/tmp/companion-install.$$"
	mkdir -p "${ROOT}"
	log "Downloading latest release bundle..."

	bundle_dir="${ROOT}/companion"
	binary_path=""
	init_template_path=""
	gst_dir=""

	if fetch "${BASE_URL}/${BUNDLE_ASSET}" "${ROOT}/${BUNDLE_ASSET}" 2>/dev/null && tar -xzf "${ROOT}/${BUNDLE_ASSET}" -C "${ROOT}" >/dev/null 2>&1; then
		candidate_binary="${bundle_dir}/companion"
		candidate_init="${bundle_dir}/init.d/companion"
		candidate_gst="${bundle_dir}/gst"
		if [ -f "${candidate_binary}" ] && [ -f "${candidate_init}" ] && [ -d "${candidate_gst}" ]; then
			binary_path="${candidate_binary}"
			init_template_path="${candidate_init}"
			gst_dir="${candidate_gst}"
		fi
	fi

	if [ -z "${binary_path}" ] || [ -z "${init_template_path}" ]; then
		log "Bundle asset ${BUNDLE_ASSET} not found or incomplete in latest release."
		exit 1
	fi

	expected_sha="$(resolve_bundle_sha)"
	if [ -z "${expected_sha}" ]; then
		log "Could not resolve expected SHA256 digest for ${BUNDLE_ASSET}."
		exit 1
	fi
	actual_sha="$(sha256_file "${ROOT}/${BUNDLE_ASSET}")"
	if [ "${actual_sha}" != "${expected_sha}" ]; then
		log "SHA256 mismatch for ${BUNDLE_ASSET}."
		log "Expected: ${expected_sha}"
		log "Actual:   ${actual_sha}"
		exit 1
	fi

	chmod 755 "${binary_path}" "${init_template_path}"
	SELECTED_BINARY_PATH="${binary_path}"
	SELECTED_INIT_TEMPLATE="${init_template_path}"
	SELECTED_GST_DIR="${gst_dir}"
}

main() {
	require_root
	log "Starting companion installation"

	input_binary="${1:-}"
	if [ -n "${input_binary}" ]; then
		log "Using local binary input: ${input_binary}"
		resolve_local_install_inputs "${input_binary}" "${LOCAL_INIT_TEMPLATE}"
	else
		download_latest_artifacts
	fi

	install_binary "${SELECTED_BINARY_PATH}"
	install_gst_runtime "${SELECTED_GST_DIR}"
	register_service "${SELECTED_INIT_TEMPLATE}"
	start_service
	restore_root_ro
	post_install_checks
	if [ "${POST_CHECK_FAILURES}" -ne 0 ]; then
		log "Installation finished with ${POST_CHECK_FAILURES} failed check(s)."
		exit 1
	fi
	log "Installation complete."
}

main "$@"
