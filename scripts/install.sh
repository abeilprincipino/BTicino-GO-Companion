#!/usr/bin/env sh
set -eu

SERVICE_NAME="companion"
BASE_DIR="/home/bticino/cfg/extra/companion"
BIN_PATH="${BASE_DIR}/companion"
INIT_SCRIPT="/etc/init.d/${SERVICE_NAME}"

SCRIPT_DIR="$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)"
LOCAL_INIT_TEMPLATE="${SCRIPT_DIR}/init.d/companion"

DEFAULT_RELEASE_REPO="abeilprincipino/BTicino-GO-Companion"
REPO="${COMPANION_RELEASE_REPO:-${DEFAULT_RELEASE_REPO}}"
RELEASE_TAG="${COMPANION_RELEASE_TAG:-}"
BUNDLE_ASSET="${COMPANION_RELEASE_BUNDLE_ASSET:-companion.tar.gz}"
HEALTHCHECK_TIMEOUT_SEC="${COMPANION_HEALTHCHECK_TIMEOUT_SEC:-45}"

if [ -n "${RELEASE_TAG}" ]; then
	DEFAULT_BASE_URL="https://github.com/${REPO}/releases/download/${RELEASE_TAG}"
	DEFAULT_RELEASE_API="https://api.github.com/repos/${REPO}/releases/tags/${RELEASE_TAG}"
else
	DEFAULT_BASE_URL="https://github.com/${REPO}/releases/latest/download"
	DEFAULT_RELEASE_API="https://api.github.com/repos/${REPO}/releases/latest"
fi

BASE_URL="${COMPANION_RELEASE_BASE_URL:-${DEFAULT_BASE_URL}}"
RELEASE_API="${COMPANION_RELEASE_API:-${DEFAULT_RELEASE_API}}"

LOCK_DIR="/var/run/${SERVICE_NAME}-install.lock"

ROOT=""
ROOT_WAS_REMOUNTED=0
LOCK_HELD=0
FAILURES=0
POST_CHECK_FAILURES=0
SELECTED_BINARY_PATH=""
SELECTED_INIT_TEMPLATE=""
SELECTED_GST_DIR=""
INSTALLED_VERSION="unknown"
CURRENT_STEP="starting"
ABORT_REPORTED=0
RUN_FINISHED=0

# The installer is often driven over a remote session that can vanish mid-run,
# when the network drops or Home Assistant restarts. Stopping there would leave
# / read-write and the service half-replaced, so writes are best-effort and the
# run carries on to the end whether or not anyone is still reading.
log() { printf 'INFO: %s\n' "$*" 2>/dev/null || true; }
ok() { printf 'OK: %s\n' "$*" 2>/dev/null || true; }
warn() { printf 'WARN: %s\n' "$*" 2>/dev/null || true; }
fail() { printf 'FAIL: %s\n' "$*" 2>/dev/null || true; FAILURES=$((FAILURES + 1)); }

# die goes to stderr so that it is still seen when it fires inside a command
# substitution, where stdout is the caller's variable.
die() {
	printf 'FAIL: %s\n' "$*" >&2 2>/dev/null || true
	ABORT_REPORTED=1
	exit 1
}

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

acquire_install_lock() {
	if ! mkdir "${LOCK_DIR}" 2>/dev/null; then
		holder="$(cat "${LOCK_DIR}/pid" 2>/dev/null || true)"
		if [ -n "${holder}" ] && [ -d "/proc/${holder}" ]; then
			die "Another installation is already running (pid ${holder})."
		fi
		rm -rf "${LOCK_DIR}"
		mkdir "${LOCK_DIR}" || die "Could not create the installation lock ${LOCK_DIR}."
	fi
	echo "$$" > "${LOCK_DIR}/pid"
	LOCK_HELD=1
}

release_install_lock() {
	[ "${LOCK_HELD}" -eq 1 ] || return 0
	rm -rf "${LOCK_DIR}"
	LOCK_HELD=0
}

# An exit that nothing announced (a failed command under set -e, a signal)
# would otherwise end the output on whatever INFO line came before it.
on_exit() {
	rc=$?
	set +e
	cleanup_download_dir
	restore_root_ro
	release_install_lock
	if [ "${rc}" -ne 0 ] && [ "${RUN_FINISHED}" -eq 0 ] && [ "${ABORT_REPORTED}" -eq 0 ]; then
		printf 'FAIL: Installation aborted while %s (exit %s).\n' "${CURRENT_STEP}" "${rc}" >&2 2>/dev/null
	fi
}
trap on_exit EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
trap '' HUP PIPE

require_root() {
	[ "$(id -u)" -eq 0 ] || die "This script must run as root."
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
	die "Neither wget nor curl is available."
}

sha256_file() {
	target="$1"
	if command -v sha256sum >/dev/null 2>&1; then sha256sum "${target}" | awk '{print $1}'; return; fi
	if command -v busybox >/dev/null 2>&1; then busybox sha256sum "${target}" | awk '{print $1}'; return; fi
	if command -v openssl >/dev/null 2>&1; then openssl dgst -sha256 "${target}" | awk '{print $NF}'; return; fi
	die "No SHA256 tool found (sha256sum/busybox/openssl)."
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

# Prints the version a companion binary reports, or "unknown" for builds that
# predate the -version flag.
binary_version() {
	version="$("$1" -version 2>/dev/null || true)"
	printf '%s\n' "${version:-unknown}"
}

install_binary() {
	mkdir -p "${BASE_DIR}"
	candidate="${BASE_DIR}/companion.candidate.$$"
	cp "$1" "${candidate}"; chmod 755 "${candidate}"
	INSTALLED_VERSION="$(binary_version "${candidate}")"
	if [ -f "${BIN_PATH}" ]; then cp -f "${BIN_PATH}" "${BASE_DIR}/companion.previous"; chmod 755 "${BASE_DIR}/companion.previous" || true; fi
	mv -f "${candidate}" "${BIN_PATH}"
	# The init script promotes companion.new over companion on its next start,
	# so an update the companion staged earlier would replace this binary as
	# soon as the service is restarted below.
	if [ -e "${BASE_DIR}/companion.new" ]; then
		rm -f "${BASE_DIR}/companion.new"
		log "Discarded the staged update ${BASE_DIR}/companion.new."
	fi
	log "Installed binary ${INSTALLED_VERSION} to ${BIN_PATH}"
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
	for runlevel in 2 3 4 5; do
		dir="/etc/rc${runlevel}.d"
		link="${dir}/S99zz${SERVICE_NAME}"
		[ ! -d "${dir}" ] || { rm -f "${link}"; ln -s "../init.d/${SERVICE_NAME}" "${link}"; }
	done
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

FLEXISIP_USERS_DB="/etc/flexisip/users/users.db.txt"
FLEXISIP_ROUTE="/etc/flexisip/users/route.conf"
FLEXISIP_DOMAIN_REG="/etc/flexisip/domain-registration.conf"
SIP_USER="companion"
SIP_CONFIG_CHANGED=0

# Prints the SIP domain the intercom registers under, or fails. A source counts
# only when it yields a non-empty value: awk exits 0 even when it printed
# nothing, so keying off its exit status would let the first readable file win
# with an empty answer and never reach the fallbacks.
flexisip_domain() {
	domain_value=""
	if [ -r "${FLEXISIP_DOMAIN_REG}" ]; then
		domain_value="$(awk '/^[[:space:]]*#/ { next } NF { print $1; exit }' "${FLEXISIP_DOMAIN_REG}" || true)"
		[ -z "${domain_value}" ] || { printf '%s\n' "${domain_value}"; return 0; }
	fi
	for conf in /etc/flexisip/flexisip.conf /home/bticino/cfg/flexisip.conf; do
		[ -r "${conf}" ] || continue
		domain_value="$(awk -F= '/^[[:space:]]*(aliases|reg-domains|auth-domains)=/ { print $2; exit }' "${conf}" | awk '{ print $1; exit }' || true)"
		[ -z "${domain_value}" ] || { printf '%s\n' "${domain_value}"; return 0; }
	done
	return 1
}

backup_once() {
	[ -f "$1" ] || return 1
	[ -f "$1.companion.bak" ] || cp -p "$1" "$1.companion.bak"
}

# route_has_companion reports whether the companion contact is in the alluser
# group of $2, not merely somewhere in the file. Anchoring on the alluser contact
# and refusing a '#' in between is what rejects a match that sits in a trailing
# comment: the insertion below appends to the end of the line, so on a line such
# as '<sip:alluser@dom> <sip:x@dom>   # handsets' it lands after the '#' and
# changes nothing, while a plain grep for the contact finds it there all the same
# and would report a file that was never provisioned as provisioned.
route_has_companion() {
	grep -q "sip:alluser@$1>[^#]*sip:${SIP_USER}@$1" "$2"
}

provision_flexisip_user() {
	domain="$1"
	# route_int.conf is deliberately absent: flexisip reads
	# static-records-file=/etc/flexisip/users/route.conf, so editing route_int.conf
	# provisioned nothing. Requiring a file this installer no longer writes would
	# fail an installation for no reason.
	for file in "${FLEXISIP_USERS_DB}" "${FLEXISIP_ROUTE}"; do
		if ! backup_once "${file}"; then
			warn "Missing ${file}; skipping SIP inbound provisioning."
			return 1
		fi
	done

	if ! grep -q "^${SIP_USER}@${domain} " "${FLEXISIP_USERS_DB}"; then
		hash_line="$(awk -v d="@${domain}" 'index($1, d) { print $2; exit }' "${FLEXISIP_USERS_DB}" || true)"
		if [ -z "${hash_line}" ]; then
			warn "No existing user hash found in ${FLEXISIP_USERS_DB}; skipping SIP inbound provisioning."
			return 1
		fi
		printf '%s@%s %s ;\n' "${SIP_USER}" "${domain}" "${hash_line}" >> "${FLEXISIP_USERS_DB}"
		log "Added ${SIP_USER}@${domain} to users.db.txt"
	fi

	# No static record is written. One used to be, pointing at <sip:127.0.0.1> —
	# which resolves to flexisip itself rather than the companion's listener.
	# Dynamic registration supplies a correct contact, so the record added nothing
	# and made flexisip fork every inbound INVITE twice, delivering every doorbell
	# call to the companion in duplicate. Devices provisioned before this change
	# keep that line until it is removed by hand; see
	# docs/superpowers/specs/2026-08-06-media-teardown-call-safety-design.md.

	if route_has_companion "${domain}" "${FLEXISIP_ROUTE}"; then
		ok "SIP inbound already provisioned."
		return 0
	fi

	tmp="${FLEXISIP_ROUTE}.companion.tmp"
	sed "s|\(<sip:alluser@${domain}>.*\)$|\1, <sip:${SIP_USER}@${domain}>|" "${FLEXISIP_ROUTE}" > "${tmp}"

	# route.conf must stay one line per group; a mangled file breaks the whole
	# intercom, so validate before replacing the original. The count is compared
	# as a string: grep -c prints 0 and exits 1 with no match, and an empty count
	# must take the warn branch rather than make [ ] error out.
	#
	# The second half of the check has to look at where the contact landed, not
	# only whether it is there: the substitution above appends to the end of the
	# line, which on a line carrying a trailing comment is inside the comment. A
	# plain grep passes that, and the installer then reports a provisioned file
	# for a change that does nothing.
	alluser_lines="$(grep -c "sip:alluser@${domain}" "${tmp}" || true)"
	if [ "${alluser_lines}" != "1" ] || ! route_has_companion "${domain}" "${tmp}"; then
		rm -f "${tmp}"
		warn "route.conf rewrite failed validation; original left untouched."
		return 1
	fi

	mv "${tmp}" "${FLEXISIP_ROUTE}"
	log "Added ${SIP_USER}@${domain} to the alluser group"

	return 0
}

enable_sip_inbound() {
	config="${BASE_DIR}/config.yaml"
	[ -f "${config}" ] || { warn "Companion config not found at ${config}; enable companion.sip.inbound manually."; return 1; }

	# The companion writes companion.sip itself, migrating config.yaml files that
	# predate the section, so a missing key means that migration also failed.
	if ! grep -q '^[[:space:]]*inbound:' "${config}"; then
		warn "No 'inbound:' key in ${config}; add these two lines under its 'companion:' key and restart ${SERVICE_NAME}:"
		warn "    sip:"
		warn "        inbound: true"
		return 1
	fi

	if grep -q '^[[:space:]]*inbound:[[:space:]]*true[[:space:]]*$' "${config}"; then
		ok "companion.sip.inbound is already enabled"
		return 0
	fi

	if ! backup_once "${config}"; then
		warn "Could not back up ${config}; leaving companion.sip.inbound unchanged."
		return 1
	fi

	sed -i 's/^\([[:space:]]*\)inbound:.*/\1inbound: true/' "${config}" || {
		warn "Could not edit ${config}; enable companion.sip.inbound manually."
		return 1
	}
	SIP_CONFIG_CHANGED=1
	ok "Enabled companion.sip.inbound"

	return 0
}

setup_sip_inbound() {
	if [ "${COMPANION_SIP_INBOUND:-1}" = "0" ]; then
		log "Skipping SIP inbound provisioning (COMPANION_SIP_INBOUND=0)."
		return 0
	fi

	domain="$(flexisip_domain || true)"
	if [ -z "${domain}" ]; then
		warn "Could not determine the Flexisip domain; skipping SIP inbound provisioning."
		return 0
	fi

	# enable_sip_inbound edits config.yaml, which the companion writes on its
	# first start, so give it time to appear before touching it.
	log "Waiting for the companion to write its configuration (up to ${HEALTHCHECK_TIMEOUT_SEC}s)."
	wait_for_health "$(health_url)" "${HEALTHCHECK_TIMEOUT_SEC}" || true

	log "Provisioning SIP inbound for ${SIP_USER}@${domain}"
	if provision_flexisip_user "${domain}" && enable_sip_inbound; then
		# Only a config.yaml edit needs the companion restarted; re-provisioning
		# the Flexisip files alone changes nothing the companion reads.
		if [ "${SIP_CONFIG_CHANGED}" -eq 1 ]; then
			start_service || warn "Could not restart ${SERVICE_NAME}; restart it manually to pick up companion.sip.inbound."
		fi

		ok "SIP inbound is provisioned. Flexisip may need to be restarted, or the intercom rebooted, before incoming calls reach the companion."
	fi

	return 0
}

post_install_checks() {
	FAILURES=0; pidfile="/var/run/${SERVICE_NAME}.pid"
	[ -x "${BIN_PATH}" ] && ok "Binary exists: ${BIN_PATH}" || fail "Binary missing or not executable: ${BIN_PATH}"
	[ -x "${INIT_SCRIPT}" ] && ok "Init script present: ${INIT_SCRIPT}" || fail "Init script missing: ${INIT_SCRIPT}"
	[ -L "/etc/rc5.d/S99zz${SERVICE_NAME}" ] && ok "Boot symlink present: /etc/rc5.d/S99zz${SERVICE_NAME}" || fail "Boot symlink missing: /etc/rc5.d/S99zz${SERVICE_NAME}"
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
	[ -n "$1" ] && [ -f "$1" ] || die "Missing companion binary for install."
	[ -f "$2" ] || die "Missing init template for install."
	SELECTED_BINARY_PATH="$1"; SELECTED_INIT_TEMPLATE="$2"
	[ ! -d "${SCRIPT_DIR}/../gst" ] || SELECTED_GST_DIR="${SCRIPT_DIR}/../gst"
}

download_latest_artifacts() {
	[ -n "${REPO}" ] || die "Set COMPANION_RELEASE_REPO to download a release."
	ROOT="/tmp/companion-install.$$"; mkdir -p "${ROOT}"
	log "Downloading latest release bundle..."
	bundle="${ROOT}/${BUNDLE_ASSET}"
	fetch "${BASE_URL}/${BUNDLE_ASSET}" "${bundle}" || die "Could not download ${BASE_URL}/${BUNDLE_ASSET}."
	# Under set -e a failed command substitution in an assignment exits on the
	# spot, so the lookup must not fail for the message below to be reached.
	expected_sha="$(resolve_bundle_sha || true)"
	[ -n "${expected_sha}" ] || die "Could not resolve the expected SHA256 digest for ${BUNDLE_ASSET} from ${RELEASE_API}."
	actual_sha="$(sha256_file "${bundle}")"
	[ "${actual_sha}" = "${expected_sha}" ] || die "SHA256 mismatch for ${BUNDLE_ASSET}. Expected: ${expected_sha}; Actual: ${actual_sha}"
	tar -xzf "${bundle}" -C "${ROOT}" || die "Could not extract ${BUNDLE_ASSET}."
	bundle_dir="${ROOT}/companion"
	[ -f "${bundle_dir}/companion" ] && [ -f "${bundle_dir}/init.d/companion" ] && [ -d "${bundle_dir}/gst" ] || die "Bundle asset ${BUNDLE_ASSET} not found or incomplete in latest release."
	chmod 755 "${bundle_dir}/companion" "${bundle_dir}/init.d/companion"
	SELECTED_BINARY_PATH="${bundle_dir}/companion"; SELECTED_INIT_TEMPLATE="${bundle_dir}/init.d/companion"; SELECTED_GST_DIR="${bundle_dir}/gst"
}

main() {
	require_root
	acquire_install_lock
	log "Starting companion installation"

	if [ -n "${1:-}" ]; then
		CURRENT_STEP="checking the local install inputs"
		log "Using local binary input: $1"
		resolve_local_install_inputs "$1" "${LOCAL_INIT_TEMPLATE}"
	else
		CURRENT_STEP="downloading the release"
		download_latest_artifacts
	fi

	CURRENT_STEP="installing the binary"
	install_binary "${SELECTED_BINARY_PATH}"
	CURRENT_STEP="installing the GStreamer runtime"
	install_gst_runtime "${SELECTED_GST_DIR}"
	CURRENT_STEP="registering the init service"
	register_service "${SELECTED_INIT_TEMPLATE}"

	CURRENT_STEP="starting the service"
	start_service || warn "${SERVICE_NAME} did not start cleanly; see /tmp/${SERVICE_NAME}.log."
	CURRENT_STEP="provisioning SIP inbound"
	setup_sip_inbound
	restore_root_ro

	CURRENT_STEP="running the post-install checks"
	post_install_checks
	RUN_FINISHED=1
	log "Installed version: ${INSTALLED_VERSION}"
	[ "${POST_CHECK_FAILURES}" -eq 0 ] || { log "Installation finished with ${POST_CHECK_FAILURES} failed check(s)."; exit 1; }
	log "Installation complete."
}

main "$@"
