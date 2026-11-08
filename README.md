# woodhouse-shelly

A Woodhouse bridge for [Shelly](https://www.shelly.com/) devices. It discovers devices on the
local network via mDNS, speaking either the Gen 1 (`shelly_v1`) or Gen 2 (`shelly_v2`) HTTP/
WebSocket API depending on the device, and exposes them to Woodhouse (currently: the Dimmer 2,
Plus 1PM, Plus 2PM, and Plus Plug UK).

## Device shape (schema v2)

Device IDs are `shelly<mac>`. Component indexes start at 0 on the device; the endpoint ID is the
index plus one, as endpoint `0` is the root endpoint. Endpoint labels are the channel names set on
the device, else `Output N`.

- Root endpoint: `info`, `online`, `update` (with `start`), and `device_temperature` where the
  device reports a device-wide temperature (Dimmer 2).
- Dimmer 2: endpoint `1` has `light` (feature `brightness`, `light.set` with a transition).
- Plus 1PM, Plus 2PM, Plus Plug UK: one endpoint per switch with `on_off`, `power_meter` (the
  metering the device reports), `device_temperature` (when the switch reports one) and the
  input with the same index. Inputs follow their configured type: switch is `binary_input`,
  button is `button`, analog is `analog_input` (feature `range_map`). An input with no switch of
  the same index gets its own endpoint. Counter inputs are not published.

The endpoints and services of Plus devices are built from the device's config on first
connection; restart the bridge after changing an input type or adding components.

## Usage

```sh
woodhouse-shelly --store <path> --addr <woodhouse-server-addr>
```

Key flags:

- `--store` (required) — path to local storage for pairing/config state
- `--addr` (required) — Woodhouse server address
- `--id` — bridge ID (default `shelly`)
- `--debug` / `-v` — enable debug logging

## Taskfile tasks

- `task build` — build the bridge binary into `build-<os>-<arch>/`
- `task run` — build and run the bridge (extra args after `--` are passed through)
- `task init-deploy` — create/populate `.env` with the deploy variables (`DEPLOY_HOST`,
  `DEPLOY_USER`, `DEPLOY_DIR`, `DEPLOY_GOOS`, `DEPLOY_GOARCH`)
- `task deploy` — build for the configured deploy target, rsync it to the remote host, and
  restart its systemd service

`build-component` and `build-and-deploy-component` are internal helpers shared by the tasks
above; they aren't meant to be run directly.
