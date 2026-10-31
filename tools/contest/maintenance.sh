#!/bin/sh
# Stop OS maintenance unrelated to the contest. Keep SSH, SSM, networking,
# clock synchronization, logging, contest registration and collectors intact.
set -eu

mode=${1:-check}
case "$mode" in apply|check) ;; *) echo 'usage: maintenance.sh [apply|check]' >&2; exit 2;; esac

# Cron contains only distribution maintenance jobs on these four hosts.
# The benchmark collectors read /proc directly; they do not use sysstat.
units='
apt-daily.timer apt-daily-upgrade.timer
apt-daily.service apt-daily-upgrade.service unattended-upgrades.service
fwupd-refresh.timer fwupd-refresh.service fwupd.service packagekit.service
update-notifier-download.timer update-notifier-download.service
update-notifier-motd.timer update-notifier-motd.service
motd-news.timer motd-news.service apt-news.service
ua-timer.timer ua-timer.service
man-db.timer man-db.service
dpkg-db-backup.timer dpkg-db-backup.service
e2scrub_all.timer e2scrub_all.service e2scrub_reap.service
fstrim.timer fstrim.service
logrotate.timer logrotate.service
sysstat-collect.timer sysstat-collect.service
sysstat-summary.timer sysstat-summary.service sysstat.service
systemd-tmpfiles-clean.timer systemd-tmpfiles-clean.service
apport-autoreport.timer apport-autoreport.path apport-autoreport.service
apport-forward.socket apport.service
snapd.snap-repair.timer snapd.snap-repair.service
cron.service
'

present=''
for unit in $units; do
    load=$(systemctl show "$unit" --property=LoadState --value)
    [ "$load" != not-found ] || continue
    present="$present $unit"
done

if [ "$mode" = apply ]; then
    # The SSM agent is installed as a snap. Hold updates without stopping SSM
    # or removing its mounts, confinement setup, or the snap daemon.
    snap refresh --hold=forever
    # Disable removes boot/timer links; masking also blocks D-Bus activation.
    # Word splitting is intentional for this fixed list of unit names.
    systemctl disable --now $present
    systemctl mask --now $present
fi

for unit in $present; do
    enabled=$(systemctl is-enabled "$unit" 2>/dev/null || true)
    active=$(systemctl is-active "$unit" 2>/dev/null || true)
    [ "$enabled" = masked ] || { echo "$unit is not masked: $enabled" >&2; exit 1; }
    case "$active" in inactive|failed) ;; *) echo "$unit is still $active" >&2; exit 1;; esac
done

hold=$(snap get system refresh.hold)
[ "$hold" = forever ] || { echo "Snap refresh is not held forever: $hold" >&2; exit 1; }
systemctl is-active --quiet ssh.service
systemctl is-active --quiet chrony.service
systemctl is-active --quiet systemd-journald.service
systemctl is-active --quiet snap.amazon-ssm-agent.amazon-ssm-agent.service
systemctl is-enabled --quiet envcheck.service
systemctl is-enabled --quiet envcheck-baseline.service
echo "$(hostname): maintenance masked; Snap updates held forever; SSH, SSM, clock, logging and envcheck preserved"
