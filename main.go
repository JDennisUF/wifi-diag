package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

type commandResult struct {
	Name      string
	Command   string
	Output    string
	Err       error
	Duration  time.Duration
	ExitCode  int
	Skipped   bool
	ParseNote string
}

type toolStatus struct {
	Name    string
	Path    string
	Missing bool
}

type adapterInfo struct {
	Interface string
	Driver    string
	Version   string
	Firmware  string
	Chipset   string
}

type linkInfo struct {
	Connected bool
	SSID      string
	BSSID     string
	Frequency string
	SignalDBm int
	HasSignal bool
	RxBitrate string
	TxBitrate string
}

type stationStats struct {
	SignalDBm     int
	HasSignal     bool
	SignalAvgDBm  int
	HasSignalAvg  bool
	TxPackets     uint64
	TxRetries     uint64
	TxFailed      uint64
	RxPackets     uint64
	RxBytes       uint64
	TxBytes       uint64
	RetryPct      float64
	HasRetryPct   bool
	RetrySeverity string
}

type driverStats struct {
	TxPackets      uint64
	TxRetries      uint64
	TxRetryFailed  uint64
	TxMPDUAttempts uint64
	TxMPDUSuccess  uint64
	RxDropped      uint64
	TxSuccessRate  float64
	HasSuccessRate bool
}

type pingStats struct {
	Target        string
	PacketLoss    float64
	HasPacketLoss bool
	MinMs         float64
	AvgMs         float64
	MaxMs         float64
	MdevMs        float64
	HasRTT        bool
	Severity      string
}

type diagnosis struct {
	Adapter           adapterInfo
	Link              linkInfo
	PowerSave         string
	ActiveConnections string
	NearbyNetworks    string
	Station           stationStats
	Driver            driverStats
	PCIeErrors        []string
	DriverLogIssues   []string
	RouterPing        pingStats
	InternetPing      pingStats
	Gateway           string
	Overall           string
	Reasons           []string
	Recommendations   []string
}

type ansi struct {
	Reset   string
	Bold    string
	Dim     string
	Red     string
	Green   string
	Yellow  string
	Blue    string
	Cyan    string
	Magenta string
}

func main() {
	var ifaceFlag string
	flag.StringVar(&ifaceFlag, "iface", "", "wireless interface to inspect")
	flag.Parse()

	if os.Geteuid() != 0 {
		fatalf("this tool requires sudo/root")
	}

	colors := ansi{
		Reset:   "\033[0m",
		Bold:    "\033[1m",
		Dim:     "\033[2m",
		Red:     "\033[31m",
		Green:   "\033[32m",
		Yellow:  "\033[33m",
		Blue:    "\033[34m",
		Cyan:    "\033[36m",
		Magenta: "\033[35m",
	}

	requiredTools := []string{"iw", "ip", "ethtool", "nmcli", "lspci", "ping", "dmesg"}
	toolStatuses := checkTools(requiredTools)

	iface := ifaceFlag
	if iface == "" {
		var err error
		iface, err = detectWirelessInterface()
		if err != nil {
			fatalf("failed to detect wireless interface: %v", err)
		}
	}

	results := []commandResult{}

	gateway := ""
	if gatewayRes := runCommand("gateway_route", "ip", "route"); true {
		results = append(results, gatewayRes)
		gateway = parseGateway(gatewayRes.Output)
	}

	cmds := []struct {
		name string
		bin  string
		args []string
	}{
		{"adapter_info", "ethtool", []string{"-i", iface}},
		{"link_status", "iw", []string{"dev", iface, "link"}},
		{"active_connection", "nmcli", []string{"connection", "show", "--active"}},
		{"nearby_wifi", "nmcli", []string{"-f", "IN-USE,SSID,BSSID,CHAN,RATE,SIGNAL", "dev", "wifi", "list"}},
		{"power_save", "iw", []string{"dev", iface, "get", "power_save"}},
		{"station_dump", "iw", []string{"dev", iface, "station", "dump"}},
		{"driver_stats", "ethtool", []string{"-S", iface}},
		{"hardware_info", "lspci", []string{"-nnk"}},
		{"pcie_errors", "dmesg", nil},
	}

	for _, cmd := range cmds {
		if isToolMissing(toolStatuses, cmd.bin) {
			results = append(results, commandResult{
				Name:    cmd.name,
				Command: strings.Join(append([]string{cmd.bin}, cmd.args...), " "),
				Skipped: true,
				Err:     fmt.Errorf("missing required tool: %s", cmd.bin),
			})
			continue
		}
		results = append(results, runCommand(cmd.name, cmd.bin, cmd.args...))
	}

	driverName := parseDriver(resultByName(results, "adapter_info").Output)
	driverLogRes := commandResult{
		Name:      "driver_logs",
		Command:   "dmesg",
		Skipped:   true,
		ParseNote: "driver name unavailable",
	}
	if driverName != "" && !isToolMissing(toolStatuses, "dmesg") {
		driverLogRes = runCommand("driver_logs", "dmesg")
		driverLogRes.ParseNote = driverName
	}
	results = append(results, driverLogRes)

	if gateway != "" && !isToolMissing(toolStatuses, "ping") {
		results = append(results, runCommand("router_ping", "ping", "-c", "30", gateway))
	} else {
		results = append(results, commandResult{
			Name:    "router_ping",
			Command: "ping -c 30 <gateway>",
			Skipped: true,
			Err:     errors.New("gateway not detected"),
		})
	}

	if !isToolMissing(toolStatuses, "ping") {
		results = append(results, runCommand("internet_ping", "ping", "-c", "30", "1.1.1.1"))
	} else {
		results = append(results, commandResult{
			Name:    "internet_ping",
			Command: "ping -c 30 1.1.1.1",
			Skipped: true,
			Err:     errors.New("missing required tool: ping"),
		})
	}

	diag := buildDiagnosis(iface, gateway, results)
	printReport(colors, iface, toolStatuses, diag, results)
}

func checkTools(names []string) []toolStatus {
	statuses := make([]toolStatus, 0, len(names))
	for _, name := range names {
		path, err := exec.LookPath(name)
		statuses = append(statuses, toolStatus{
			Name:    name,
			Path:    path,
			Missing: err != nil,
		})
	}
	sort.Slice(statuses, func(i, j int) bool { return statuses[i].Name < statuses[j].Name })
	return statuses
}

func missingTools(statuses []toolStatus) []string {
	var out []string
	for _, status := range statuses {
		if status.Missing {
			out = append(out, status.Name)
		}
	}
	return out
}

func isToolMissing(statuses []toolStatus, name string) bool {
	for _, status := range statuses {
		if status.Name == name {
			return status.Missing
		}
	}
	return true
}

func detectWirelessInterface() (string, error) {
	entries, err := os.ReadDir("/sys/class/net")
	if err != nil {
		return "", err
	}
	for _, entry := range entries {
		iface := entry.Name()
		wirelessPath := filepath.Join("/sys/class/net", iface, "wireless")
		if _, err := os.Stat(wirelessPath); err == nil {
			return iface, nil
		}
	}
	return "", errors.New("no wireless interface found under /sys/class/net")
}

func runCommand(name, bin string, args ...string) commandResult {
	start := time.Now()
	cmd := exec.Command(bin, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	duration := time.Since(start)
	output := stdout.String()
	if stderr.Len() > 0 {
		if output != "" && !strings.HasSuffix(output, "\n") {
			output += "\n"
		}
		output += stderr.String()
	}
	exitCode := 0
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = -1
		}
	}
	return commandResult{
		Name:     name,
		Command:  strings.Join(append([]string{bin}, args...), " "),
		Output:   strings.TrimRight(output, "\n"),
		Err:      err,
		Duration: duration,
		ExitCode: exitCode,
	}
}

func resultByName(results []commandResult, name string) commandResult {
	for _, result := range results {
		if result.Name == name {
			return result
		}
	}
	return commandResult{Name: name, Skipped: true, Err: errors.New("result not found")}
}

func buildDiagnosis(iface, gateway string, results []commandResult) diagnosis {
	diag := diagnosis{
		Gateway: gateway,
	}

	adapterRes := resultByName(results, "adapter_info")
	hwRes := resultByName(results, "hardware_info")
	linkRes := resultByName(results, "link_status")
	powerRes := resultByName(results, "power_save")
	stationRes := resultByName(results, "station_dump")
	driverStatsRes := resultByName(results, "driver_stats")
	pcieRes := resultByName(results, "pcie_errors")
	driverLogRes := resultByName(results, "driver_logs")
	routerPingRes := resultByName(results, "router_ping")
	internetPingRes := resultByName(results, "internet_ping")
	activeRes := resultByName(results, "active_connection")
	nearbyRes := resultByName(results, "nearby_wifi")

	diag.Adapter = parseAdapterInfo(iface, adapterRes.Output, hwRes.Output)
	diag.Link = parseLinkInfo(linkRes.Output)
	diag.PowerSave = parsePowerSave(powerRes.Output)
	diag.Station = parseStationStats(stationRes.Output)
	diag.Driver = parseDriverStats(driverStatsRes.Output)
	diag.PCIeErrors = parsePCIeErrors(pcieRes.Output)
	diag.DriverLogIssues = parseDriverLogs(driverLogRes.Output, diag.Adapter.Driver)
	diag.RouterPing = parsePing(routerPingRes.Output, gateway)
	diag.InternetPing = parsePing(internetPingRes.Output, "1.1.1.1")
	diag.ActiveConnections = strings.TrimSpace(activeRes.Output)
	diag.NearbyNetworks = strings.TrimSpace(nearbyRes.Output)

	if !diag.Link.HasSignal && diag.Station.HasSignal {
		diag.Link.SignalDBm = diag.Station.SignalDBm
		diag.Link.HasSignal = true
	}

	diag.Overall, diag.Reasons, diag.Recommendations = assess(diag)
	return diag
}

func parseAdapterInfo(iface, adapterOutput, hardwareOutput string) adapterInfo {
	info := adapterInfo{Interface: iface}
	for _, line := range strings.Split(adapterOutput, "\n") {
		key, value, ok := splitField(line)
		if !ok {
			continue
		}
		switch key {
		case "driver":
			info.Driver = value
		case "version":
			info.Version = value
		case "firmware-version":
			info.Firmware = value
		}
	}
	info.Chipset = parseChipset(hardwareOutput)
	return info
}

func parseDriver(output string) string {
	for _, line := range strings.Split(output, "\n") {
		key, value, ok := splitField(line)
		if ok && key == "driver" {
			return value
		}
	}
	return ""
}

func parseChipset(output string) string {
	lines := strings.Split(output, "\n")
	for i, line := range lines {
		lower := strings.ToLower(line)
		if strings.Contains(lower, "network controller") || strings.Contains(lower, "wireless") {
			parts := strings.SplitN(strings.TrimSpace(line), ": ", 2)
			if len(parts) == 2 {
				return parts[1]
			}
			if i+1 < len(lines) && strings.TrimSpace(lines[i+1]) != "" {
				return strings.TrimSpace(lines[i+1])
			}
			return strings.TrimSpace(line)
		}
	}
	return ""
}

func parseLinkInfo(output string) linkInfo {
	info := linkInfo{}
	lines := strings.Split(output, "\n")
	if len(lines) == 0 {
		return info
	}
	first := strings.TrimSpace(lines[0])
	if strings.HasPrefix(first, "Connected to ") {
		info.Connected = true
		info.BSSID = strings.TrimPrefix(first, "Connected to ")
	}
	for _, line := range lines[1:] {
		line = strings.TrimSpace(line)
		key, value, ok := splitField(line)
		if !ok {
			continue
		}
		switch key {
		case "SSID":
			info.SSID = value
		case "freq":
			info.Frequency = value + " MHz"
		case "signal":
			info.SignalDBm, info.HasSignal = parseLeadingInt(value)
		case "rx bitrate":
			info.RxBitrate = value
		case "tx bitrate":
			info.TxBitrate = value
		}
	}
	return info
}

func parsePowerSave(output string) string {
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(strings.ToLower(line), "power save:") {
			return strings.TrimSpace(strings.TrimPrefix(line, "Power save:"))
		}
	}
	return ""
}

func parseStationStats(output string) stationStats {
	stats := stationStats{}
	for _, line := range strings.Split(output, "\n") {
		key, value, ok := splitField(strings.TrimSpace(line))
		if !ok {
			continue
		}
		switch key {
		case "signal":
			stats.SignalDBm, stats.HasSignal = parseLeadingInt(value)
		case "signal avg":
			stats.SignalAvgDBm, stats.HasSignalAvg = parseLeadingInt(value)
		case "tx packets":
			stats.TxPackets = parseUint(value)
		case "tx retries":
			stats.TxRetries = parseUint(value)
		case "tx failed":
			stats.TxFailed = parseUint(value)
		case "rx packets":
			stats.RxPackets = parseUint(value)
		case "rx bytes":
			stats.RxBytes = parseUint(value)
		case "tx bytes":
			stats.TxBytes = parseUint(value)
		}
	}
	if stats.TxPackets > 0 {
		stats.RetryPct = float64(stats.TxRetries) / float64(stats.TxPackets) * 100
		stats.HasRetryPct = true
		stats.RetrySeverity = retrySeverity(stats.RetryPct)
	}
	return stats
}

func parseDriverStats(output string) driverStats {
	stats := driverStats{}
	for _, line := range strings.Split(output, "\n") {
		key, value, ok := splitField(strings.TrimSpace(line))
		if !ok {
			continue
		}
		switch key {
		case "tx_packets":
			stats.TxPackets = parseUint(value)
		case "tx_retries":
			stats.TxRetries = parseUint(value)
		case "tx_retry_failed":
			stats.TxRetryFailed = parseUint(value)
		case "tx_mpdu_attempts":
			stats.TxMPDUAttempts = parseUint(value)
		case "tx_mpdu_success":
			stats.TxMPDUSuccess = parseUint(value)
		case "rx_dropped":
			stats.RxDropped = parseUint(value)
		}
	}
	if stats.TxMPDUAttempts > 0 {
		stats.TxSuccessRate = float64(stats.TxMPDUSuccess) / float64(stats.TxMPDUAttempts) * 100
		stats.HasSuccessRate = true
	}
	return stats
}

func parsePCIeErrors(output string) []string {
	keywords := []string{"aer", "pcie", "pcie error", "corrected error"}
	var issues []string
	for _, line := range strings.Split(output, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		lower := strings.ToLower(trimmed)
		if strings.Contains(lower, "linux version") || strings.Contains(lower, "command line:") {
			continue
		}
		for _, keyword := range keywords {
			if strings.Contains(lower, keyword) {
				issues = append(issues, trimmed)
				break
			}
		}
	}
	return issues
}

func parseDriverLogs(output, driver string) []string {
	if driver == "" {
		return nil
	}
	keywords := []string{"firmware crash", "reset", "timeout", "dma", "failed"}
	driverLower := strings.ToLower(driver)
	var issues []string
	for _, line := range strings.Split(output, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		lower := strings.ToLower(trimmed)
		if !strings.Contains(lower, driverLower) {
			continue
		}
		for _, keyword := range keywords {
			if strings.Contains(lower, keyword) {
				issues = append(issues, trimmed)
				break
			}
		}
	}
	return issues
}

func parseGateway(output string) string {
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 3 && fields[0] == "default" && fields[1] == "via" {
			ip := net.ParseIP(fields[2])
			if ip != nil {
				return fields[2]
			}
		}
	}
	return ""
}

func parsePing(output, target string) pingStats {
	stats := pingStats{Target: target}
	lossRE := regexp.MustCompile(`(\d+(?:\.\d+)?)%\s+packet loss`)
	rttRE := regexp.MustCompile(`=\s*([0-9.]+)/([0-9.]+)/([0-9.]+)/([0-9.]+)\s*ms`)
	if match := lossRE.FindStringSubmatch(output); len(match) == 2 {
		stats.PacketLoss = parseFloat(match[1])
		stats.HasPacketLoss = true
	}
	if match := rttRE.FindStringSubmatch(output); len(match) == 5 {
		stats.MinMs = parseFloat(match[1])
		stats.AvgMs = parseFloat(match[2])
		stats.MaxMs = parseFloat(match[3])
		stats.MdevMs = parseFloat(match[4])
		stats.HasRTT = true
		stats.Severity = latencySeverity(stats.AvgMs)
	}
	return stats
}

func assess(diag diagnosis) (string, []string, []string) {
	type rank struct {
		level string
		score int
	}
	current := rank{level: "Excellent", score: 0}
	var reasons []string
	var recs []string

	bump := func(level, reason string, recommendation ...string) {
		scoreMap := map[string]int{"Excellent": 0, "Good": 1, "Fair": 2, "Poor": 3, "Severe": 4}
		if scoreMap[level] > current.score {
			current = rank{level: level, score: scoreMap[level]}
		}
		reasons = append(reasons, reason)
		recs = append(recs, recommendation...)
	}

	if diag.Link.HasSignal {
		switch {
		case diag.Link.SignalDBm <= -70:
			bump("Poor", fmt.Sprintf("signal is weak at %d dBm", diag.Link.SignalDBm), "Move closer to the access point or inspect antenna connections.")
		case diag.Link.SignalDBm <= -65:
			bump("Fair", fmt.Sprintf("signal is marginal at %d dBm", diag.Link.SignalDBm), "Test the device closer to the router and compare signal stability.")
		case diag.Link.SignalDBm <= -60:
			bump("Good", fmt.Sprintf("signal is acceptable at %d dBm", diag.Link.SignalDBm))
		}
	} else {
		bump("Severe", "link signal could not be determined", "Confirm the adapter is associated with a Wi-Fi network.")
	}

	if diag.Station.HasRetryPct {
		switch {
		case diag.Station.RetryPct > 30:
			bump("Severe", fmt.Sprintf("retry rate is high at %.1f%%", diag.Station.RetryPct), "Inspect antennas and test with a different access point if available.")
		case diag.Station.RetryPct > 20:
			bump("Poor", fmt.Sprintf("retry rate is elevated at %.1f%%", diag.Station.RetryPct), "Check for interference and test on 5 GHz versus 2.4 GHz.")
		case diag.Station.RetryPct > 10:
			bump("Fair", fmt.Sprintf("retry rate is moderate at %.1f%%", diag.Station.RetryPct))
		case diag.Station.RetryPct > 5:
			bump("Good", fmt.Sprintf("retry rate is slightly elevated at %.1f%%", diag.Station.RetryPct))
		}
	}

	if diag.RouterPing.HasRTT {
		switch {
		case diag.RouterPing.AvgMs > 30 || diag.RouterPing.MaxMs > 50:
			bump("Severe", fmt.Sprintf("router latency is unstable with %.1f ms avg and %.1f ms max", diag.RouterPing.AvgMs, diag.RouterPing.MaxMs), "This points to a local Wi-Fi issue; compare over Ethernet if possible.")
		case diag.RouterPing.AvgMs > 20:
			bump("Poor", fmt.Sprintf("router latency is high at %.1f ms avg", diag.RouterPing.AvgMs), "Check channel congestion and retest near the router.")
		case diag.RouterPing.AvgMs > 10:
			bump("Fair", fmt.Sprintf("router latency is fair at %.1f ms avg", diag.RouterPing.AvgMs))
		case diag.RouterPing.AvgMs > 5:
			bump("Good", fmt.Sprintf("router latency is acceptable at %.1f ms avg", diag.RouterPing.AvgMs))
		}
	}

	if strings.EqualFold(diag.PowerSave, "on") {
		bump("Fair", "power save is enabled", "Disable Wi-Fi power saving while troubleshooting to remove latency jitter.")
	}

	if len(diag.PCIeErrors) > 0 {
		bump("Severe", "PCIe/AER errors were found in dmesg", "Review PCIe error logs and BIOS settings; hardware or platform issues may be involved.")
	}

	if len(diag.DriverLogIssues) > 0 {
		bump("Severe", "driver log issues were found in dmesg", "Investigate driver or firmware stability, and compare with a newer kernel or firmware package.")
	}

	if diag.Driver.HasSuccessRate && diag.Driver.TxSuccessRate < 80 {
		bump("Poor", fmt.Sprintf("transmit success rate is low at %.1f%%", diag.Driver.TxSuccessRate))
	}

	if diag.RouterPing.HasRTT && diag.InternetPing.HasRTT && diag.RouterPing.AvgMs > 15 && diag.InternetPing.AvgMs >= diag.RouterPing.AvgMs {
		reasons = append(reasons, "local Wi-Fi appears to be contributing to internet latency")
	}

	if len(reasons) == 0 {
		reasons = append(reasons, "signal, retries, and router latency all look healthy")
	}

	recs = uniqueStrings(recs)
	if len(recs) == 0 {
		recs = []string{"No obvious Wi-Fi fault detected from the collected metrics."}
	}

	return current.level, reasons, recs
}

func printReport(colors ansi, iface string, tools []toolStatus, diag diagnosis, results []commandResult) {
	sep := strings.Repeat("=", 72)
	line := strings.Repeat("-", 72)

	fmt.Println(colors.Bold + sep)
	fmt.Println("Wi-Fi Diagnostic Report")
	fmt.Println(sep + colors.Reset)

	printSection(colors, "Runtime")
	fmt.Printf("Interface: %s\n", iface)
	if diag.Gateway != "" {
		fmt.Printf("Gateway:   %s\n", diag.Gateway)
	}
	fmt.Println()

	printSection(colors, "Dependencies")
	for _, tool := range tools {
		status := colorizeStatus(colors, !tool.Missing)
		path := tool.Path
		if path == "" {
			path = "missing"
		}
		fmt.Printf("%-10s %s (%s)\n", tool.Name+":", status, path)
	}
	fmt.Println()

	printSection(colors, "Adapter")
	fmt.Printf("Chipset:   %s\n", valueOrNA(diag.Adapter.Chipset))
	fmt.Printf("Driver:    %s\n", valueOrNA(diag.Adapter.Driver))
	fmt.Printf("Version:   %s\n", valueOrNA(diag.Adapter.Version))
	fmt.Printf("Firmware:  %s\n", valueOrNA(diag.Adapter.Firmware))
	fmt.Println()

	printSection(colors, "Connection")
	fmt.Printf("SSID:      %s\n", valueOrNA(diag.Link.SSID))
	fmt.Printf("BSSID:     %s\n", valueOrNA(diag.Link.BSSID))
	fmt.Printf("Frequency: %s\n", valueOrNA(diag.Link.Frequency))
	fmt.Printf("Signal:    %s\n", signalString(diag.Link))
	fmt.Printf("RX Rate:   %s\n", valueOrNA(diag.Link.RxBitrate))
	fmt.Printf("TX Rate:   %s\n", valueOrNA(diag.Link.TxBitrate))
	fmt.Println()

	printSection(colors, "Health")
	if diag.Station.HasRetryPct {
		fmt.Printf("Retry Rate:        %.1f%% (%s)\n", diag.Station.RetryPct, diag.Station.RetrySeverity)
	} else {
		fmt.Printf("Retry Rate:        n/a\n")
	}
	if diag.Driver.HasSuccessRate {
		fmt.Printf("Transmit Success:  %.1f%%\n", diag.Driver.TxSuccessRate)
	} else {
		fmt.Printf("Transmit Success:  n/a\n")
	}
	fmt.Printf("Power Save:        %s\n", strings.ToUpper(valueOrNA(diag.PowerSave)))
	fmt.Printf("PCIe Errors:       %d\n", len(diag.PCIeErrors))
	fmt.Printf("Driver Log Issues: %d\n", len(diag.DriverLogIssues))
	fmt.Println()

	printPingSection(colors, "Router Ping", diag.RouterPing)
	printPingSection(colors, "Internet Ping", diag.InternetPing)

	printSection(colors, "Overall Assessment")
	fmt.Println(severityColor(colors, diag.Overall) + strings.ToUpper(diag.Overall) + colors.Reset)
	fmt.Println()
	fmt.Println("Reasons:")
	for _, reason := range diag.Reasons {
		fmt.Printf("- %s\n", reason)
	}
	fmt.Println()
	fmt.Println("Recommendations:")
	for _, rec := range diag.Recommendations {
		fmt.Printf("- %s\n", rec)
	}
	fmt.Println()

	printSection(colors, "Active Connections")
	fmt.Println(valueOrNA(diag.ActiveConnections))
	fmt.Println()

	printSection(colors, "Nearby Wi-Fi Networks")
	fmt.Println(valueOrNA(diag.NearbyNetworks))
	fmt.Println()

	fmt.Println(colors.Bold + line)
	fmt.Println("Raw Diagnostics")
	fmt.Println(line + colors.Reset)
	for _, result := range results {
		fmt.Printf("%s$ %s%s\n", colors.Cyan, result.Command, colors.Reset)
		if result.Skipped {
			fmt.Printf("%sskipped: %v%s\n\n", colors.Yellow, result.Err, colors.Reset)
			continue
		}
		if result.Err != nil {
			fmt.Printf("%serror (exit %d): %v%s\n", colors.Red, result.ExitCode, result.Err, colors.Reset)
			if result.Output != "" {
				fmt.Println(result.Output)
			}
			fmt.Println()
			continue
		}
		if result.Output == "" {
			fmt.Printf("%s(no output)%s\n\n", colors.Dim, colors.Reset)
			continue
		}
		fmt.Println(result.Output)
		fmt.Println()
	}
}

func printSection(colors ansi, title string) {
	line := strings.Repeat("-", 72)
	fmt.Println(colors.Bold + title)
	fmt.Println(line + colors.Reset)
}

func printPingSection(colors ansi, title string, stats pingStats) {
	printSection(colors, title)
	if !stats.HasRTT && !stats.HasPacketLoss {
		fmt.Println("n/a")
		fmt.Println()
		return
	}
	if stats.HasRTT {
		fmt.Printf("Target: %s\n", stats.Target)
		fmt.Printf("Min:    %.1f ms\n", stats.MinMs)
		fmt.Printf("Avg:    %.1f ms\n", stats.AvgMs)
		fmt.Printf("Max:    %.1f ms\n", stats.MaxMs)
		fmt.Printf("Mdev:   %.1f ms\n", stats.MdevMs)
	}
	if stats.HasPacketLoss {
		fmt.Printf("Loss:   %.1f%%\n", stats.PacketLoss)
	}
	fmt.Println()
}

func colorizeStatus(colors ansi, ok bool) string {
	if ok {
		return colors.Green + "OK" + colors.Reset
	}
	return colors.Red + "MISSING" + colors.Reset
}

func severityColor(colors ansi, severity string) string {
	switch severity {
	case "Excellent":
		return colors.Green
	case "Good":
		return colors.Cyan
	case "Fair":
		return colors.Yellow
	case "Poor", "Severe":
		return colors.Red
	default:
		return colors.Magenta
	}
}

func signalString(link linkInfo) string {
	if !link.HasSignal {
		return "n/a"
	}
	return fmt.Sprintf("%d dBm", link.SignalDBm)
}

func valueOrNA(value string) string {
	if strings.TrimSpace(value) == "" {
		return "n/a"
	}
	return value
}

func splitField(line string) (string, string, bool) {
	parts := strings.SplitN(line, ":", 2)
	if len(parts) != 2 {
		return "", "", false
	}
	return strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1]), true
}

func parseLeadingInt(value string) (int, bool) {
	fields := strings.Fields(value)
	if len(fields) == 0 {
		return 0, false
	}
	n, err := strconv.Atoi(fields[0])
	if err != nil {
		return 0, false
	}
	return n, true
}

func parseUint(value string) uint64 {
	fields := strings.Fields(value)
	if len(fields) == 0 {
		return 0
	}
	n, err := strconv.ParseUint(fields[0], 10, 64)
	if err != nil {
		return 0
	}
	return n
}

func parseFloat(value string) float64 {
	n, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return 0
	}
	return n
}

func retrySeverity(pct float64) string {
	switch {
	case pct <= 5:
		return "Excellent"
	case pct <= 10:
		return "Good"
	case pct <= 20:
		return "Fair"
	case pct <= 30:
		return "Poor"
	default:
		return "Severe"
	}
}

func latencySeverity(avg float64) string {
	switch {
	case avg < 5:
		return "Excellent"
	case avg < 15:
		return "Good"
	case avg < 30:
		return "Fair"
	default:
		return "Poor"
	}
}

func uniqueStrings(items []string) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, item := range items {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		if _, ok := seen[item]; ok {
			continue
		}
		seen[item] = struct{}{}
		out = append(out, item)
	}
	return out
}

func fatalf(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, "error: "+format+"\n", args...)
	os.Exit(1)
}
