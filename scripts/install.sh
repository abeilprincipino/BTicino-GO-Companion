#!/usr/bin/env sh
set -eu

SERVICE_NAME="companion"
BASE_DIR="/home/bticino/cfg/extra/companion"
BIN_PATH="${BASE_DIR}/companion"
INIT_SCRIPT="/etc/init.d/${SERVICE_NAME}"

SCRIPT_DIR="$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)"
LOCAL_INIT_TEMPLATE="${SCRIPT_DIR}/init.d/companion"

DEFAULT_RELEASE_REPO="owner/BTicino-GO-Companion"
REPO="${COMPANION_RELEASE_REPO:-${DEFAULT_RELEASE_REPO}}"
BUNDLE_ASSET="${COMPANION_RELEASE_BUNDLE_ASSET:-companion.tar.gz}"
HEALTHCHECK_TIMEOUT_SEC="${COMPANION_HEALTHCHECK_TIMEOUT_SEC:-45}"
BASE_URL="${COMPANION_RELEASE_BASE_URL:-https://github.com/${REPO}/releases/latest/download}"
RELEASE_API="${COMPANION_RELEASE_API:-https://api.github.com/repos/${REPO}/releases/latest}"

ROOT=""
ROOT_WAS_REMOUNTED=0
FAILURES=0
POST_CHECK_FAILURES=0
SELECTED_BINARY_PATH=""
SELECTED_INIT_TEMPLATE=""
SELECTED_GST_DIR=""

log() { printf 'INFO: %s\n' "$*"; }
ok() { printf 'OK: %s\n' "$*"; }
warn() { printf 'WARN: %s\n' "$*"; }
fail() { printf 'FAIL: %s\n' "$*"; FAILURES=$((FAILURES + 1)); }

cleanup_download_dir() {
	[ -z "${ROOT}" ] || [ ! -d "${ROOT}" ] || rm -rf "${ROOT}" || true
}

restore_root_ro() {
	if [ "${ROOT_WAS_REMOUNTED}" -eq 1 ]; then
		mount -o remount,ro / || true
		ROOT_WAS_REMOUNTED=0
		log "Restored / as read-only."
	fi
}

on_exit() { cleanup_download_dir; restore_root_ro; }
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
	if command -v wget >/dev/null 2>&1; then wget -qO "${dst}" "${url}"; return; fi
	if command -v curl >/dev/null 2>&1; then curl -fsSL -o "${dst}" "${url}"; return; fi
	log "Neither wget nor curl is available."
	exit 1
}

sha256_file() {
	target="$1"
	if command -v sha256sum >/dev/null 2>&1; then sha256sum "${target}" | awk '{print $1}'; return; fi
	if command -v busybox >/dev/null 2>&1; then busybox sha256sum "${target}" | awk '{print $1}'; return; fi
	if command -v openssl >/dev/null 2>&1; then openssl dgst -sha256 "${target}" | awk '{print $NF}'; return; fi
	log "No SHA256 tool found (sha256sum/busybox/openssl)."
	exit 1
}

parse_release_asset_digest() {
	awk -v asset="$2" '
		BEGIN { want = 0 }
		$0 ~ /"name"[[:space:]]*:/ && index($0, "\"" asset "\"") > 0 { want = 1 }
		want && $0 ~ /"digest"[[:space:]]*:/ {
			line = $0; sub(/.*"digest"[[:space:]]*:[[:space:]]*"sha256:/, "", line); sub(/".*/, "", line)
			if (line != "") { print tolower(line); exit }
		}
	' "$1"
}

resolve_bundle_sha() {
	if [ -n "${COMPANION_RELEASE_BUNDLE_SHA256:-}" ]; then
		printf '%s' "${COMPANION_RELEASE_BUNDLE_SHA256}" | tr 'A-F' 'a-f'
		return
	fi
	release_json="${ROOT}/release.json"
	fetch "${RELEASE_API}" "${release_json}" 2>/dev/null || return 1
	parse_release_asset_digest "${release_json}" "${BUNDLE_ASSET}"
}

companion_firewall_ports() { printf '%s\n' "8080 80 8554 51826"; }
companion_firewall_udp_ports() { printf '%s\n' "5353 8555"; }

ensure_persistent_firewall_port_value() {
	hook="/etc/network/if-pre-up.d/iptables"
	port="$1"
	if [ ! -f "${hook}" ]; then warn "${hook} not found, skipping persistent firewall patch."; return; fi
	if awk -v port="${port}" '
		/# ssh \& sip enabled/ { inblock=1; next }
		inblock && /^[[:space:]]*for i in .*; do[[:space:]]*$/ { n=split($0,a,/[^0-9]+/); for(i=1;i<=n;i++) if(a[i]==port) found=1; inblock=0 }
		END { exit(found ? 0 : 1) }
	' "${hook}"; then log "Persistent firewall already allows companion port ${port}."; return; fi
	tmp="${hook}.tmp.$$"
	if awk -v port="${port}" '
		BEGIN { patched=0; inblock=0 }
		/# ssh \& sip enabled/ { inblock=1; print; next }
		inblock && /^[[:space:]]*for i in .*; do[[:space:]]*$/ {
			line=$0; sub(/^[[:space:]]*for i in[[:space:]]*/, "", line); sub(/[[:space:]]*; do[[:space:]]*$/, "", line)
			n=split(line,a,/[[:space:]]+/); out="for i in"; has=0
			for(i=1;i<=n;i++) if(a[i]!="") { if(a[i]==port) has=1; out=out " " a[i] }
			if(!has) out=out " " port; print out "; do"; patched=1; inblock=0; next
		}
		{ print }
		END { if(!patched) exit 42 }
	' "${hook}" > "${tmp}"; then
		cp "${tmp}" "${hook}"; rm -f "${tmp}"; log "Persisted companion firewall port ${port} in ${hook}."; return
	fi
	rc=$?; rm -f "${tmp}"
	[ "${rc}" -eq 42 ] && warn "could not find SSH/SIP firewall block in ${hook}; no persistent patch applied." || warn "failed to patch ${hook} for companion port ${port}."
}

ensure_persistent_firewall_udp_port_value() {
	hook="/etc/network/if-pre-up.d/iptables"
	port="$1"
	if [ ! -f "${hook}" ]; then warn "${hook} not found, skipping persistent firewall patch."; return; fi
	if grep -Eq "udp.*--dport[[:space:]]+${port}.*-j[[:space:]]+ACCEPT" "${hook}"; then log "Persistent firewall already allows UDP ${port}."; return; fi
	tmp="${hook}.tmp.$$"
	if awk -v port="${port}" '
		BEGIN { patched=0 }
		/^#disable all other stuff/ && !patched { print "# companion udp service"; print "iptables -A INPUT -p udp -m udp --dport " port " -j ACCEPT"; print ""; patched=1 }
		{ print }
		END { if(!patched) exit 42 }
	' "${hook}" > "${tmp}"; then
		cp "${tmp}" "${hook}"; rm -f "${tmp}"; log "Persisted companion UDP firewall port ${port} in ${hook}."; return
	fi
	rc=$?; rm -f "${tmp}"
	[ "${rc}" -eq 42 ] && warn "could not find firewall policy marker in ${hook}; no UDP persistent patch applied." || warn "failed to patch ${hook} for companion UDP port ${port}."
}

ensure_persistent_firewall_ports() {
	for port in $(companion_firewall_ports); do ensure_persistent_firewall_port_value "${port}"; done
	for port in $(companion_firewall_udp_ports); do ensure_persistent_firewall_udp_port_value "${port}"; done
}

install_binary() {
	mkdir -p "${BASE_DIR}"
	candidate="${BASE_DIR}/companion.candidate.$$"
	cp "$1" "${candidate}"; chmod 755 "${candidate}"
	if [ -f "${BIN_PATH}" ]; then cp -f "${BIN_PATH}" "${BASE_DIR}/companion.previous"; chmod 755 "${BASE_DIR}/companion.previous" || true; fi
	mv -f "${candidate}" "${BIN_PATH}"
	log "Installed binary to ${BIN_PATH}"
}

install_gst_runtime() {
	src_dir="$1"
	if [ -z "${src_dir}" ] || [ ! -d "${src_dir}" ]; then log "No bundled gst runtime provided; keeping existing runtime."; return; fi
	mkdir -p "${BASE_DIR}"
	candidate="${BASE_DIR}/gst.candidate.$$"; previous="${BASE_DIR}/gst.previous"
	rm -rf "${candidate}"; mkdir -p "${candidate}"; cp -a "${src_dir}/." "${candidate}/"
	rm -rf "${previous}"; [ ! -d "${BASE_DIR}/gst" ] || mv -f "${BASE_DIR}/gst" "${previous}"
	mv -f "${candidate}" "${BASE_DIR}/gst"
	log "Installed gst runtime to ${BASE_DIR}/gst"
}

install_init_script() { cp -f "$1" "${INIT_SCRIPT}"; chmod 755 "${INIT_SCRIPT}"; }

register_service() {
	remount_root_rw
	install_init_script "$1"
	for runlevel in 2 3 4 5; do dir="/etc/rc${runlevel}.d"; link="${dir}/S45${SERVICE_NAME}"; [ ! -d "${dir}" ] || { rm -f "${link}"; ln -s "../init.d/${SERVICE_NAME}" "${link}"; }; done
	for runlevel in 0 1 6; do dir="/etc/rc${runlevel}.d"; link="${dir}/K55${SERVICE_NAME}"; [ ! -d "${dir}" ] || { rm -f "${link}"; ln -s "../init.d/${SERVICE_NAME}" "${link}"; }; done
	ensure_persistent_firewall_ports
	log "Registered init service ${SERVICE_NAME}"
}

start_service() { [ ! -x "${INIT_SCRIPT}" ] || "${INIT_SCRIPT}" restart || "${INIT_SCRIPT}" start; }
health_url() { printf '%s\n' "http://127.0.0.1:8080/api/v3/health"; }
health_endpoint_reachable() {
	if command -v curl >/dev/null 2>&1; then curl -fsS --max-time 3 "$1" >/dev/null 2>&1; return; fi
	if command -v wget >/dev/null 2>&1; then wget -q -T 3 -O /dev/null "$1" >/dev/null 2>&1; return; fi
	return 127
}
wait_for_health() { elapsed=0; while [ "${elapsed}" -lt "$2" ]; do health_endpoint_reachable "$1" && return; sleep 1; elapsed=$((elapsed + 1)); done; return 1; }

post_install_checks() {
	FAILURES=0; pidfile="/var/run/${SERVICE_NAME}.pid"
	[ -x "${BIN_PATH}" ] && ok "Binary exists: ${BIN_PATH}" || fail "Binary missing or not executable: ${BIN_PATH}"
	[ -x "${INIT_SCRIPT}" ] && ok "Init script present: ${INIT_SCRIPT}" || fail "Init script missing: ${INIT_SCRIPT}"
	[ -L "/etc/rc5.d/S45${SERVICE_NAME}" ] && ok "Boot symlink present: /etc/rc5.d/S45${SERVICE_NAME}" || fail "Boot symlink missing: /etc/rc5.d/S45${SERVICE_NAME}"
	if [ -x "${INIT_SCRIPT}" ] && "${INIT_SCRIPT}" status >/dev/null 2>&1; then ok "Service is running"
	elif [ -f "${pidfile}" ] && pid="$(cat "${pidfile}" 2>/dev/null || true)" && [ -n "${pid}" ] && [ -d "/proc/${pid}" ]; then ok "Service process exists via pidfile ${pidfile} (pid ${pid})"
	else fail "Service not running"; fi
	if [ -x "${BASE_DIR}/gst/bin/gst-launch-1.0" ] || [ -x "${BASE_DIR}/gst/opt/gst14/bin/gst-launch-1.0" ]; then ok "GStreamer launcher is present"; else fail "GStreamer launcher missing in ${BASE_DIR}/gst"; fi
	url="$(health_url)"
	if command -v curl >/dev/null 2>&1 || command -v wget >/dev/null 2>&1; then
		log "Waiting for health endpoint (up to ${HEALTHCHECK_TIMEOUT_SEC}s): ${url}"
		wait_for_health "${url}" "${HEALTHCHECK_TIMEOUT_SEC}" && ok "Health endpoint reachable at ${url}" || fail "Health endpoint not reachable at ${url} after ${HEALTHCHECK_TIMEOUT_SEC}s"
	else fail "Neither curl nor wget available for health check"; fi
	if awk '$2=="/"{print $4}' /proc/mounts | grep -Eq '(^|,)ro(,|$)'; then ok "Root filesystem is read-only"; else fail "Root filesystem is not read-only"; fi
	POST_CHECK_FAILURES="${FAILURES}"
	[ "${FAILURES}" -eq 0 ] && log "Post-install checks passed." || log "Post-install checks completed with ${FAILURES} failure(s)."
}

resolve_local_install_inputs() {
	[ -n "$1" ] && [ -f "$1" ] || { log "Missing companion binary for install."; exit 1; }
	[ -f "$2" ] || { log "Missing init template for install."; exit 1; }
	SELECTED_BINARY_PATH="$1"; SELECTED_INIT_TEMPLATE="$2"
	[ ! -d "${SCRIPT_DIR}/../gst" ] || SELECTED_GST_DIR="${SCRIPT_DIR}/../gst"
}

download_latest_artifacts() {
	ROOT="/tmp/companion-install.$$"; mkdir -p "${ROOT}"
	log "Downloading latest release bundle..."
	bundle="${ROOT}/${BUNDLE_ASSET}"
	fetch "${BASE_URL}/${BUNDLE_ASSET}" "${bundle}"
	expected_sha="$(resolve_bundle_sha)"
	[ -n "${expected_sha}" ] || { log "Could not resolve expected SHA256 digest for ${BUNDLE_ASSET}."; exit 1; }
	actual_sha="$(sha256_file "${bundle}")"
	if [ "${actual_sha}" != "${expected_sha}" ]; then log "SHA256 mismatch for ${BUNDLE_ASSET}. Expected: ${expected_sha}; Actual: ${actual_sha}"; exit 1; fi
	tar -xzf "${bundle}" -C "${ROOT}"
	bundle_dir="${ROOT}/companion"
	[ -f "${bundle_dir}/companion" ] && [ -f "${bundle_dir}/init.d/companion" ] && [ -d "${bundle_dir}/gst" ] || { log "Bundle asset ${BUNDLE_ASSET} not found or incomplete in latest release."; exit 1; }
	chmod 755 "${bundle_dir}/companion" "${bundle_dir}/init.d/companion"
	SELECTED_BINARY_PATH="${bundle_dir}/companion"; SELECTED_INIT_TEMPLATE="${bundle_dir}/init.d/companion"; SELECTED_GST_DIR="${bundle_dir}/gst"
}

main() {
	require_root; log "Starting companion installation"
	if [ -n "${1:-}" ]; then log "Using local binary input: $1"; resolve_local_install_inputs "$1" "${LOCAL_INIT_TEMPLATE}"; else download_latest_artifacts; fi
	install_binary "${SELECTED_BINARY_PATH}"; install_gst_runtime "${SELECTED_GST_DIR}"; register_service "${SELECTED_INIT_TEMPLATE}"
	start_service; restore_root_ro; post_install_checks
	[ "${POST_CHECK_FAILURES}" -eq 0 ] || { log "Installation finished with ${POST_CHECK_FAILURES} failed check(s)."; exit 1; }
	log "Installation complete."
}

main "$@"
