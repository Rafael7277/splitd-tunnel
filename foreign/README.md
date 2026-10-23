# Splitd V3 foreign-side setup

## Install through the interactive panel

Copy the repository to the foreign server, then run the installer as root:

```bash
cd /path/to/splitd-tunnel/foreign
bash install.sh
```

The panel asks for the Iran server address, download and upload ports, the local `TARGET` service, stale-flow threshold, and logging preference. Enter the reachable Iran IP or hostname and the local destination that should receive tunneled sessions. The ports and `FLOW_STALE_SEC` must match the Iran endpoint. The panel installs the binary and systemd service, enables the service, and starts it. You do not need to edit `splitd.conf` for the normal setup.

## Manage the service

Run these commands from this directory as root:

```bash
bash install.sh status
bash install.sh start
bash install.sh stop
bash install.sh restart
```

The foreign endpoint writes application payload on the download flow and reads the upload flow. Acknowledgements travel over the opposite flow, so application payload remains one-way on each TCP connection. `FLOW_STALE_SEC` triggers flow recovery only when pending data is stale; it is not a periodic restart.
