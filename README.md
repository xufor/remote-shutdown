# remote-shutdown

A tiny web page with two buttons, **Reboot** and **Shut down**, for a Raspberry Pi
(or any Linux box with systemd). One static Go binary, no dependencies.

- Asks for your sudo password on the page; nothing happens without it.
- Shows live progress: the page polls the machine, tells you when it has gone
  offline, and for a reboot tells you when it is back.
- Installs itself as a systemd service.

## Install

Download the binary for your board from the releases page:

| Binary | For |
| --- | --- |
| `remote-shutdown-linux-arm64` | Pi 3/4/5, Zero 2 on 64-bit Raspberry Pi OS |
| `remote-shutdown-linux-armv7` | Pi 2/3/4 on 32-bit Raspberry Pi OS |
| `remote-shutdown-linux-armv6` | Pi 1, Zero, Zero W |
| `remote-shutdown-linux-amd64` | ordinary PCs |

```sh
chmod +x remote-shutdown-linux-arm64
./remote-shutdown-linux-arm64 install            # asks for sudo and which port (default 8765)
```

Then open `http://<hostname>.local:8765`. To skip the question: `install --port 9000`. Also `--user NAME`.
Remove it with `remote-shutdown uninstall`.

## How it works

The service runs as your normal user, not root. When you press a button the
password is checked (`unix_chkpwd`) and then used for `sudo systemctl poweroff`
or `sudo systemctl reboot`. The password is never stored.

"Shutdown complete" is detected from the browser: the page polls `/api/status`
every second. Three missed polls mean the machine is offline; for a reboot, the
page waits until the server answers again with a different boot ID.

## Security

The page is plain HTTP, so the password crosses your network unencrypted. Use it
on a network you trust, and do not expose the port to the internet. Wrong
passwords are rate-limited to one attempt every two seconds.

## Build

```sh
make build      # ./remote-shutdown for this machine
make release    # all binaries into dist/
./remote-shutdown serve --dry-run   # try the page without really powering off
```

Releases are published by GitHub Actions when a `v*` tag is pushed.
