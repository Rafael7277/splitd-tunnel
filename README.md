# splitd Tunnel v3

**Project designer and analyst:** Rafael7277.

splitd v3 is a Linux TCP tunnel relay built around two separate data flows. The Iran endpoint accepts local proxy connections, while the foreign endpoint connects those sessions to a configured destination.

> **Status:** The reconnect and replay behavior is implemented in the source, but this release must be validated on the intended Iran-to-foreign network path before production use. Start with a test instance and monitor both endpoints.

## How it works

Each client session is multiplexed over two TCP connections:

| Flow | Application payload writer | Reader |
| --- | --- | --- |
| Download | Foreign endpoint | Iran endpoint |
| Upload | Iran endpoint | Foreign endpoint |

The one-way invariant applies to each TCP flow: only one endpoint writes application bytes on that flow. V3 uses two independent, persistent flows: the download flow carries application bytes from foreign to Iran, and the upload flow carries application bytes from Iran to foreign. Together, the pair provides both download and upload for proxy sessions. Each flow can carry many framed chunks and multiplexed sessions; V3 does not open a new TCP connection for every packet. The two roles use distinct ports and role prefaces.

Frames carry sequence numbers and cumulative acknowledgements. A flow's acknowledgement frames are sent on the opposite flow, preserving the single application-payload writer on each TCP connection. TCP's ordinary transport acknowledgements still work normally. If the oldest unacknowledged frame exceeds `FLOW_STALE_SEC`, the affected TCP flow is replaced and outstanding frames are replayed. The receiver uses sequence numbers to discard duplicates. This is a stale-flow health threshold, not a timer that periodically restarts the daemon. If the process itself exits, systemd is configured to start it again.

V3 is not wire-compatible with V2. Install V3 on both endpoints and use separate download and upload ports.

## Package contents

- `main.go`, `go.mod`: V3 source code.
- `manage.sh`: shared interactive installer and service manager.
- `iran/`: Iran-side launcher, sample configuration, service template, and Linux amd64 binary.
- `foreign/`: foreign-side launcher, sample configuration, service template, and Linux amd64 binary.
- `README.md`: this guide.

## Requirements

- Linux with systemd, root access, and either Go 1.23 or newer or the bundled Linux amd64 binary.
- Two reachable TCP ports on the Iran server, one for each flow. Allow the foreign server to connect to both ports in the Iran firewall.
- The same `DL_PORT`, `UL_PORT`, and `FLOW_STALE_SEC` values on both endpoints.
- A local destination on the foreign server, such as the listening port of the proxy inbound you want to reach.

The included binaries target Linux `amd64`. Build a binary for another server architecture before installing there.

## Install

Copy the `v3` directory to both servers, then configure and install each side. The sample foreign configuration contains a placeholder host; replace it with the Iran server's address before installation.

### Iran server

```bash
cd /path/to/v3/iran
vi splitd.conf
bash install.sh
```

The sample Iran local listener is `127.0.0.1:93`. Point the local proxy client's outbound at that address when testing. The sample flow ports are TCP `12797` and `12798`.

### Foreign server

```bash
cd /path/to/v3/foreign
vi splitd.conf
bash install.sh
```

Set `IRAN_HOST` to the Iran server's reachable address. During the interactive setup, enter the foreign-side `TARGET` address and port for the local service that should receive tunneled sessions. Use the same flow ports and stale threshold as on Iran.

### Interactive manager

Running `bash install.sh` without arguments opens the instance menu:

- `n`: add a parallel instance with its own ports and service.
- A listed instance number: reconfigure that instance.
- `d`: delete an instance after confirmation.

When Go is installed locally, the manager builds from source. If that build is unavailable, it uses the bundled Linux amd64 binary.

## Service commands

Run commands from the corresponding role directory (`v3/iran` or `v3/foreign`):

```bash
bash install.sh list
bash install.sh status
bash install.sh start
bash install.sh stop
bash install.sh restart
bash install.sh enable
bash install.sh disable
```

An instance number or service name can be supplied when multiple instances exist:

```bash
bash install.sh status 2
bash install.sh restart splitd-v3-iran-2
```

The default services are `splitd-v3-iran` and `splitd-v3-foreign`. Configuration is installed under `/etc/splitd-v3/`; systemd unit files use the `splitd-v3-iran` and `splitd-v3-foreign` names. Reconfiguring an instance installs its unit and restarts that selected service.

## Configuration

| Setting | Meaning |
| --- | --- |
| `LOCAL_ADDR` | Iran-side local listener for the proxy client. |
| `IRAN_HOST` | Iran server address used by the foreign endpoint. |
| `DL_PORT` | Download-flow TCP port. |
| `UL_PORT` | Upload-flow TCP port. |
| `TARGET` | Foreign-side local destination reached for each session. |
| `FLOW_STALE_SEC` | Seconds an oldest unacknowledged frame may remain pending before flow recovery. Valid range: 5–3600; default: 15. |

The interactive manager asks for role-specific values and keeps logs disabled by default. It does not configure a periodic restart interval.

## Observe and troubleshoot

```bash
systemctl status splitd-v3-iran
journalctl -u splitd-v3-iran -f
```

On the foreign server:

```bash
systemctl status splitd-v3-foreign
journalctl -u splitd-v3-foreign -f
```

Enable application logs in the manager if you need flow-level messages. A stale-flow recovery emits `stale unacknowledged data; replacing TCP connection`, followed by a new flow-established message. Use the same configured threshold on both sides.

## Build from source

From the `v3` directory, build the Linux amd64 binary:

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o splitd-linux-amd64 .
```

For another architecture, set `GOARCH` to the target architecture and distribute the resulting binary in both role directories as `splitd` before installation.

## Network and security notes

- Keep the Iran flow ports reachable from the foreign server and restrict access with the host or provider firewall where possible.
- This tunnel does not add encryption or peer authentication. Protect its network path separately if those properties are required.
- Do not use the V2 and V3 endpoints as opposite halves of one tunnel; both ends must run the same protocol version.
- Replacing a flow can briefly affect in-flight sessions. Validate recovery, throughput, and session behavior on the actual route before moving users to it.

