# woodhouse-shelly

A Woodhouse bridge for [Shelly](https://www.shelly.com/) devices. It discovers devices on the
local network via mDNS, speaking either the Gen 1 (`shelly_v1`) or Gen 2 (`shelly_v2`) HTTP/
WebSocket API depending on the device, and exposes them to Woodhouse (currently: the Dimmer 2,
Plus 1PM, Plus 2PM, and Plus Plug UK).

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
