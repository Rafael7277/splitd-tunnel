# Splitd V3 Iran-side setup

## Install through the interactive panel

Copy the repository to the Iran server, then run the installer as root:

```bash
cd /path/to/splitd-tunnel/iran
bash install.sh
```

The panel asks for the local panel address, download and upload ports, stale-flow threshold, and logging preference. Press Enter to accept the suggested values. The defaults are `127.0.0.1:93`, TCP ports `12797` and `12798`, and a 15-second stale-flow threshold. The panel installs the binary and systemd service, enables the service, and starts it. You do not need to edit `splitd.conf` for the normal setup.

After both endpoints are configured, point the local proxy client's outbound to `127.0.0.1:93` or the local address you selected. Allow the foreign server to reach both flow ports through the Iran firewall.

## Manage the service

Run these commands from this directory as root:

```bash
bash install.sh status
bash install.sh start
bash install.sh stop
bash install.sh restart
```

`FLOW_STALE_SEC` is a threshold for replacing a TCP flow only after its oldest application frame remains unacknowledged. It is not a periodic process restart. The Iran and foreign endpoints must use the same value.
