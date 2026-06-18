# wifi-diag

`wifi-diag` is a Linux Wi-Fi diagnostics terminal UI for Pop!_OS and Ubuntu-class systems.

It detects the active wireless interface, lets you choose which diagnostics to run, streams command output live, and builds a running health summary from the collected data.

## Features

- interactive TUI with selectable diagnostics
- live command output panel
- parsed summary for signal, retry rate, latency, and overall health
- dependency checks for required system tools
- root-only execution for commands that need elevated access

## Requirements

- Linux desktop with PCIe Wi-Fi
- Go 1.18+ to build from source
- `sudo` to run the app
- these system tools available in `PATH`:
  - `iw`
  - `ip`
  - `ethtool`
  - `nmcli`
  - `lspci`
  - `ping`
  - `dmesg`

If a tool is missing, the TUI shows that in the summary and skips the affected diagnostics.

## Build

```bash
go build -o wifi-diag .
```

## Run

```bash
sudo ./wifi-diag
```

Optional interface override:

```bash
sudo ./wifi-diag --iface wlp11s0
```

## Controls

- `Up` / `Down`: move through diagnostics
- `Space`: select or clear a diagnostic
- `r`: run selected diagnostics
- `a`: select all diagnostics
- `c`: clear all diagnostics, or cancel the current run
- `q`: quit

## Diagnostics

The app can run these checks:

- adapter info
- link status
- active connection
- nearby networks
- power save state
- station statistics
- driver statistics
- hardware info
- PCIe error check
- driver log check
- router ping
- internet ping

The default ping count is `10` packets for both router and internet latency tests.

## Interface Layout

- left pane: diagnostic checklist and per-test status
- upper-right pane: parsed summary and overall assessment
- lower-right pane: live output from the selected command

## Notes

- The app requires a real terminal session and root privileges.
- `dmesg`-based tests show filtered results relevant to PCIe and Wi-Fi driver problems.
- The summary becomes more useful as more diagnostics are run.
