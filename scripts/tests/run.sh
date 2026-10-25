#!/usr/bin/env sh
# Logic tests for install.sh and uninstall.sh. Each case runs a copy of the
# script with its absolute paths moved under a scratch root and with fake id,
# mount and init script, so it runs unprivileged under any POSIX sh:
#   sh scripts/tests/run.sh
set -u

HERE="$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)"
SCRIPTS="$(dirname "${HERE}")"
PASSED=0
FAILED=0

export COMPANION_SIP_INBOUND=0 COMPANION_HEALTHCHECK_TIMEOUT_SEC=1

check() {
	description="$1"
	shift
	if "$@"; then
		PASSED=$((PASSED + 1))
	else
		FAILED=$((FAILED + 1))
		printf 'not ok - %s\n' "${description}"
		sed 's/^/    | /' "${R}/out" 2>/dev/null || true
	fi
}

setup() {
	R="$(mktemp -d)"
	B="${R}/home/bticino/cfg/extra/companion"
	mkdir -p "${R}/shim" "${R}/var/run" "${R}/etc/init.d" "${R}/etc/network/if-pre-up.d" "${R}/etc/flexisip/users" "${B}"
	for n in 0 1 2 3 4 5 6; do mkdir -p "${R}/etc/rc${n}.d"; done
	sed -e "s|/home/bticino|${R}/home/bticino|g" -e "s|/var/run|${R}/var/run|g" -e "s|/etc/|${R}/etc/|g" \
		-e "s|/tmp/companion-install|${R}/dl|g" "${SCRIPTS}/$1" > "${R}/run.sh"
	printf '#!/bin/sh\necho "${FAKE_UID:-0}"\n' > "${R}/shim/id"
	printf '#!/bin/sh\nexit 0\n' > "${R}/shim/mount"
	chmod +x "${R}/shim/id" "${R}/shim/mount"
}

run_script() {
	PATH="${R}/shim:${PATH}" sh "${R}/run.sh" "$@" > "${R}/out" 2>&1
	echo $? > "${R}/rc"
}

rc_is() { [ "$(cat "${R}/rc")" = "$1" ]; }
out_has() { grep -qF -- "$1" "${R}/out"; }
out_lacks() { ! grep -qF -- "$1" "${R}/out"; }
# file_is <file> <printf format>: the file holds exactly what the format prints.
file_is() { printf "$2" | cmp -s - "$1"; }

# The hook as the stock firmware ships it, with 80 already open. Written with
# printf rather than a heredoc because the loop body is tab-indented.
STOCK_HOOK='# ssh & sip enabled\nfor i in 22 80 5060; do\n\tiptables -A INPUT -p tcp --dport $i -j ACCEPT\ndone\n\n#disable all other stuff\niptables -P INPUT DROP\n'

# The same hook after install.sh added its TCP ports and UDP blocks.
INSTALLED_HOOK='# ssh & sip enabled\nfor i in 22 80 5060 8080 8554 51826; do\n\tiptables -A INPUT -p tcp --dport $i -j ACCEPT\ndone\n\n# companion udp service\niptables -A INPUT -p udp -m udp --dport 5353 -j ACCEPT\n\n# companion udp service\niptables -A INPUT -p udp -m udp --dport 8555 -j ACCEPT\n\n#disable all other stuff\niptables -P INPUT DROP\n'

write_hook() {
	printf "${STOCK_HOOK}" > "${R}/etc/network/if-pre-up.d/iptables"
}

# A companion as install.sh leaves it, with 80 already open before it came.
write_installed_companion() {
	printf '#!/bin/sh\necho "init $1" >> "%s/init.log"\nexit 0\n' "${R}" > "${R}/etc/init.d/companion"
	chmod +x "${R}/etc/init.d/companion"
	for n in 2 3 4 5; do ln -s ../init.d/companion "${R}/etc/rc${n}.d/S99zzcompanion"; done
	for n in 0 1 6; do ln -s ../init.d/companion "${R}/etc/rc${n}.d/K55companion"; done
	echo binary > "${B}/companion"
	printf '8080\n8554\n51826\n' > "${B}/firewall-added"
	printf "${INSTALLED_HOOK}" > "${R}/etc/network/if-pre-up.d/iptables"
	printf 'user1@example.local abc ;\ncompanion@example.local abc ;\n' > "${R}/etc/flexisip/users/users.db.txt"
	printf '<sip:alluser@example.local> <sip:user1@example.local>, <sip:companion@example.local>\n' > "${R}/etc/flexisip/users/route.conf"
	touch "${R}/etc/flexisip/users/users.db.txt.companion.bak" "${R}/etc/flexisip/users/route.conf.companion.bak"
}

test_install_records_the_ports_it_adds() {
	setup install.sh
	mkdir -p "${R}/init.d"
	printf '#!/bin/sh\nexit 0\n' > "${R}/init.d/companion"
	printf '#!/bin/sh\n[ "${1:-}" = -version ] && echo v1.2.3\nexit 0\n' > "${R}/companion"
	chmod +x "${R}/init.d/companion" "${R}/companion"
	write_hook
	run_script "${R}/companion"
	check "install records exactly the TCP ports it added" file_is "${B}/firewall-added" '8080\n8554\n51826\n'
}

test_uninstall_removes_the_companion() {
	setup uninstall.sh
	write_installed_companion
	run_script
	check "uninstall exits 0" rc_is 0
	check "uninstall ends with the OK line" out_has "OK: Companion removed. Restart Flexisip or reboot the intercom."
	check "uninstall stops the service" grep -qx "init stop" "${R}/init.log"
	check "uninstall removes the init script" [ ! -e "${R}/etc/init.d/companion" ]
	check "uninstall removes the boot links" [ ! -e "${R}/etc/rc5.d/S99zzcompanion" ] && [ ! -e "${R}/etc/rc6.d/K55companion" ]
	check "uninstall removes only the recorded TCP ports and the UDP blocks" file_is "${R}/etc/network/if-pre-up.d/iptables" "${STOCK_HOOK}"
	check "uninstall removes the SIP user" file_is "${R}/etc/flexisip/users/users.db.txt" 'user1@example.local abc ;\n'
	check "uninstall removes the alluser contact" file_is "${R}/etc/flexisip/users/route.conf" '<sip:alluser@example.local> <sip:user1@example.local>\n'
	check "uninstall deletes the backups" [ ! -e "${R}/etc/flexisip/users/route.conf.companion.bak" ]
	check "uninstall deletes the companion directory" [ ! -e "${B}" ]
	check "uninstall releases the lock" [ ! -e "${R}/var/run/companion-install.lock" ]
}

test_uninstall_without_port_record_keeps_tcp_ports() {
	setup uninstall.sh
	write_installed_companion
	rm -f "${B}/firewall-added"
	run_script
	check "uninstall without a port record exits 0" rc_is 0
	check "uninstall without a port record warns" out_has "WARN: No record of the TCP ports the installer added"
	check "uninstall without a port record keeps the TCP list" grep -q "for i in 22 80 5060 8080 8554 51826; do" "${R}/etc/network/if-pre-up.d/iptables"
}

test_uninstall_leaves_an_unexpected_hook_alone() {
	setup uninstall.sh
	write_installed_companion
	printf 'iptables -P INPUT ACCEPT\n' > "${R}/etc/network/if-pre-up.d/iptables"
	run_script
	check "uninstall with an unexpected hook exits 0" rc_is 0
	check "uninstall with an unexpected hook warns" out_has "WARN: Could not find the SSH/SIP firewall block"
	check "uninstall with an unexpected hook leaves it unchanged" file_is "${R}/etc/network/if-pre-up.d/iptables" 'iptables -P INPUT ACCEPT\n'
}

test_uninstall_with_nothing_installed() {
	setup uninstall.sh
	rm -rf "${B}" "${R}/etc/flexisip/users"
	run_script
	check "uninstall with nothing installed exits 0" rc_is 0
	check "uninstall with nothing installed ends with the OK line" out_has "OK: Companion removed."
}

test_uninstall_refuses_a_second_run() {
	setup uninstall.sh
	write_installed_companion
	sleep 30 &
	holder=$!
	mkdir -p "${R}/var/run/companion-install.lock"
	echo "${holder}" > "${R}/var/run/companion-install.lock/pid"
	run_script
	kill "${holder}" 2>/dev/null
	check "uninstall during an installation exits 1" rc_is 1
	check "uninstall during an installation says why" out_has "FAIL: Another installation or removal is already running"
	check "uninstall during an installation changes nothing" [ -e "${R}/etc/init.d/companion" ]
}

test_uninstall_requires_root() {
	setup uninstall.sh
	FAKE_UID=1000
	export FAKE_UID
	run_script
	unset FAKE_UID
	check "uninstall as non-root exits 1" rc_is 1
	check "uninstall as non-root says why" out_has "FAIL: This script must run as root."
}

test_uninstall_removes_the_legacy_static_route() {
	setup uninstall.sh
	write_installed_companion
	printf '<sip:alluser@example.local> <sip:user1@example.local>, <sip:companion@example.local>\n<sip:companion@example.local> <sip:127.0.0.1>\n' > "${R}/etc/flexisip/users/route.conf"
	run_script
	check "uninstall with a legacy static route exits 0" rc_is 0
	check "uninstall with a legacy static route ends with the OK line" out_has "OK: Companion removed."
	check "uninstall with a legacy static route does not fail validation" out_lacks "route.conf rewrite failed validation"
	check "uninstall removes the legacy line and the alluser contact, nothing else" file_is "${R}/etc/flexisip/users/route.conf" '<sip:alluser@example.local> <sip:user1@example.local>\n'
}

test_uninstall_with_an_empty_port_record_prints_no_ports_message() {
	setup uninstall.sh
	write_installed_companion
	: > "${B}/firewall-added"
	run_script
	check "uninstall with an empty port record exits 0" rc_is 0
	check "uninstall with an empty port record does not print an empty port list" out_lacks "Removed TCP ports  from"
}

test_install_records_the_ports_it_adds
test_uninstall_removes_the_companion
test_uninstall_without_port_record_keeps_tcp_ports
test_uninstall_leaves_an_unexpected_hook_alone
test_uninstall_with_nothing_installed
test_uninstall_refuses_a_second_run
test_uninstall_requires_root
test_uninstall_removes_the_legacy_static_route
test_uninstall_with_an_empty_port_record_prints_no_ports_message

printf '%s passed, %s failed\n' "${PASSED}" "${FAILED}"
[ "${FAILED}" -eq 0 ]
