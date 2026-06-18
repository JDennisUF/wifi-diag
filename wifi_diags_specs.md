# Wi-Fi Diagnostics CLI Specification

## Purpose

Create a small CLI utility for Linux (Pop!_OS/Ubuntu) that gathers common Wi-Fi diagnostics into a single report.

Suggested names:

* wifi-diag
* netcheck
* wifi-health

Language:

* Go preferred
* Single static binary
* Human-readable output
* Optional JSON output

---

# Commands To Run

## 1. Basic Adapter Information

Command:

```bash
ethtool -i wlp11s0
```

Purpose:

* Driver name
* Driver version
* Firmware version

Capture:

```text
driver
version
firmware-version
```

---

## 2. Link Status

Command:

```bash
iw dev wlp11s0 link
```

Capture:

```text
SSID
BSSID
frequency
signal
rx bitrate
tx bitrate
```

Example:

```text
Connected to 40:48:6e:5e:48:0b
SSID: KINETIC_5e4801
signal: -60 dBm
rx bitrate: 432.3 MBit/s
tx bitrate: 288.2 MBit/s
```

---

## 3. Active Connection

Command:

```bash
nmcli connection show --active
```

Purpose:

Show active network connections.

---

## 4. Nearby Wi-Fi Networks

Command:

```bash
nmcli -f IN-USE,SSID,BSSID,CHAN,RATE,SIGNAL dev wifi list
```

Purpose:

Show all visible access points.

Capture:

```text
SSID
BSSID
Channel
Rate
Signal
Connected AP
```

---

## 5. Power Save Status

Command:

```bash
iw dev wlp11s0 get power_save
```

Expected:

```text
Power save: off
```

Warning if enabled.

---

## 6. Wi-Fi Station Statistics

Command:

```bash
iw dev wlp11s0 station dump
```

Capture:

```text
signal
signal avg
tx packets
tx retries
tx failed
rx packets
rx bytes
tx bytes
```

Calculate:

```text
Retry %
= tx_retries / tx_packets * 100
```

Example:

```text
tx packets: 67849
tx retries: 24246
retry %: 35.73
```

Health thresholds:

```text
0-5%      Excellent
5-10%     Good
10-20%    Fair
20-30%    Poor
>30%      Severe
```

---

## 7. Driver Statistics

Command:

```bash
ethtool -S wlp11s0
```

Capture:

```text
tx_packets
tx_retries
tx_retry_failed
tx_mpdu_attempts
tx_mpdu_success
rx_dropped
```

Calculate:

```text
Transmit Success Rate
= tx_mpdu_success / tx_mpdu_attempts
```

---

## 8. Hardware Information

Command:

```bash
lspci -nnk | grep -A3 -i network
```

Purpose:

Identify chipset.

Example:

```text
MediaTek MT7921e
Intel AX210
```

---

## 9. PCIe Error Check

Command:

```bash
dmesg | grep -Ei "aer|pcie|error|corrected"
```

Flag:

```text
AER
Corrected Error
PCIe Error
```

Ignore normal boot messages.

---

## 10. Wi-Fi Driver Log Check

MediaTek Example:

```bash
dmesg | grep -i mt7921
```

Intel Example:

```bash
dmesg | grep -i iwlwifi
```

Look for:

```text
firmware crash
reset
timeout
DMA
failed
```

---

# Latency Tests

## Router Ping Test

Discover gateway:

```bash
ip route
```

Example:

```text
default via 192.168.254.254
```

Run:

```bash
ping -c 30 GATEWAY
```

Collect:

```text
min
avg
max
mdev
packet loss
```

Health:

```text
avg < 5ms          Excellent
avg 5-15ms         Good
avg 15-30ms        Fair
avg > 30ms         Poor
```

---

## Internet Ping Test

Run:

```bash
ping -c 30 1.1.1.1
```

Collect:

```text
min
avg
max
mdev
packet loss
```

Compare with router ping.

If router ping is bad:

```text
Local Wi-Fi issue likely
```

---

# Health Summary

Generate overall rating:

## Excellent

```text
Signal > -60 dBm
Retry % < 5%
Router ping < 5ms
No errors
```

## Good

```text
Signal > -65 dBm
Retry % < 10%
Router ping < 10ms
```

## Fair

```text
Signal > -70 dBm
Retry % < 20%
Router ping < 20ms
```

## Poor

```text
Signal <= -70 dBm
Retry % > 20%
Router ping > 20ms
```

## Severe

```text
Retry % > 30%
Router ping spikes > 50ms
```

---

# Output Example

```text
=========================================
Wi-Fi Diagnostic Report
=========================================

Adapter
-----------------------------------------
Chipset: MediaTek MT7921e
Driver: mt7921e
Firmware: 20260106153507

Connection
-----------------------------------------
SSID: KINETIC_5e4801
BSSID: 40:48:6E:5E:48:0B
Signal: -60 dBm
RX Rate: 432 Mbps
TX Rate: 288 Mbps

Health
-----------------------------------------
Retry Rate: 35.7%
Transmit Success: 67.9%
Power Save: OFF

Router Ping
-----------------------------------------
Min: 1.8 ms
Avg: 37.2 ms
Max: 118.9 ms

Internet Ping
-----------------------------------------
Min: 15 ms
Avg: 42 ms
Max: 117 ms

Overall Assessment
-----------------------------------------
SEVERE

Reason:
- Retry rate exceeds 30%
- Router latency highly unstable
- Wi-Fi link unhealthy

Recommendation:
- Inspect antennas
- Test Ethernet
- Consider replacing MT7921e with Intel AX210
```

---

# Optional Features

* `wifi-diag --json`
* `wifi-diag --watch`
* `wifi-diag --save report.txt`
* `wifi-diag --compare baseline.json`
* Colorized output
* Export markdown report
* Detect common Linux Wi-Fi chipsets
* Suggest likely causes based on metrics
