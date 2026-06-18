package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
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

type testSpec struct {
	ID          string
	Title       string
	Description string
	Tool        string
	Command     func(*appModel) (string, string, []string, error)
	Filter      func(*appModel, string) string
}

type testState struct {
	Spec     testSpec
	Selected bool
	Status   string
	Result   commandResult
}

type commandDoneMsg struct {
	TestID string
	Result commandResult
}

type liveCapture struct {
	mu      sync.Mutex
	buf     bytes.Buffer
	onChunk func(string)
}

func (l *liveCapture) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	n, err := l.buf.Write(p)
	if n > 0 && l.onChunk != nil {
		l.onChunk(string(p[:n]))
	}
	return n, err
}

func (l *liveCapture) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

type appModel struct {
	app         *tview.Application
	testsTable  *tview.Table
	outputView  *tview.TextView
	summaryView *tview.TextView
	statusView  *tview.TextView

	iface        string
	gateway      string
	toolStatuses []toolStatus
	tests        []testState
	testIndex    map[string]int
	results      map[string]commandResult
	liveOutputs  map[string]string
	selectedRow  int

	running      bool
	runningIndex int
	cancelRun    context.CancelFunc
}

func main() {
	var ifaceFlag string
	flag.StringVar(&ifaceFlag, "iface", "", "wireless interface to inspect")
	flag.Parse()

	if os.Geteuid() != 0 {
		fmt.Fprintln(os.Stderr, "error: this tool requires sudo/root")
		os.Exit(1)
	}

	requiredTools := []string{"iw", "ip", "ethtool", "nmcli", "lspci", "ping", "dmesg"}
	toolStatuses := checkTools(requiredTools)

	iface := ifaceFlag
	if iface == "" {
		var err error
		iface, err = detectWirelessInterface()
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: failed to detect wireless interface: %v\n", err)
			os.Exit(1)
		}
	}

	gateway := ""
	if !isToolMissing(toolStatuses, "ip") {
		routeRes := runCommand("gateway_route", "ip", "route")
		gateway = parseGateway(routeRes.Output)
	}

	app := tview.NewApplication()
	model := newAppModel(app, iface, gateway, toolStatuses)

	if err := app.SetRoot(model.layout(), true).EnableMouse(true).Run(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func newAppModel(app *tview.Application, iface, gateway string, toolStatuses []toolStatus) *appModel {
	model := &appModel{
		app:          app,
		iface:        iface,
		gateway:      gateway,
		toolStatuses: toolStatuses,
		testIndex:    map[string]int{},
		results:      map[string]commandResult{},
		liveOutputs:  map[string]string{},
		selectedRow:  0,
		runningIndex: -1,
	}

	model.tests = defaultTests()
	for i := range model.tests {
		model.tests[i].Selected = true
		model.tests[i].Status = "pending"
		model.testIndex[model.tests[i].Spec.ID] = i
	}

	model.testsTable = tview.NewTable().
		SetBorders(false).
		SetSelectable(true, false).
		SetFixed(1, 0)
	model.testsTable.SetBorder(true).SetTitle(" Diagnostics ").SetTitleAlign(tview.AlignLeft)
	model.testsTable.SetSelectedStyle(tcell.StyleDefault.Background(tcell.Color24).Foreground(tcell.ColorWhite))
	model.testsTable.SetSelectionChangedFunc(func(row, _ int) {
		if row <= 0 || row-1 >= len(model.tests) {
			return
		}
		model.selectedRow = row - 1
		model.showSelectedOutput(row - 1)
	})
	model.testsTable.SetInputCapture(model.handleTableKeys)

	model.summaryView = tview.NewTextView().
		SetDynamicColors(true).
		SetWrap(true)
	model.summaryView.SetBorder(true).SetTitle(" Summary ").SetTitleAlign(tview.AlignLeft)

	model.outputView = tview.NewTextView().
		SetDynamicColors(true).
		SetScrollable(true).
		SetWrap(false)
	model.outputView.SetBorder(true).SetTitle(" Command Output ").SetTitleAlign(tview.AlignLeft)

	model.statusView = tview.NewTextView().
		SetDynamicColors(true).
		SetWrap(false)
	model.statusView.SetBorder(true)

	model.refreshTestsTable()
	model.refreshSummary()
	model.refreshStatus("Ready. Select tests with space and press r to run.")
	model.showSelectedOutput(0)

	app.SetInputCapture(model.handleGlobalKeys)
	return model
}

func (m *appModel) layout() tview.Primitive {
	header := tview.NewTextView().
		SetDynamicColors(true).
		SetText(fmt.Sprintf("[::b]wifi-diag[::-]  Interface: [green]%s[-]  Gateway: [green]%s[-]", m.iface, valueOrNA(m.gateway)))
	header.SetBorder(true).SetTitle(" Wi-Fi Diagnostics ").SetTitleAlign(tview.AlignLeft)

	right := tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(m.summaryView, 14, 0, false).
		AddItem(m.outputView, 0, 1, false)

	body := tview.NewFlex().
		AddItem(m.testsTable, 44, 0, true).
		AddItem(right, 0, 1, false)

	root := tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(header, 3, 0, false).
		AddItem(body, 0, 1, true).
		AddItem(m.statusView, 3, 0, false)
	return root
}

func (m *appModel) handleGlobalKeys(event *tcell.EventKey) *tcell.EventKey {
	switch event.Key() {
	case tcell.KeyCtrlC:
		if m.running && m.cancelRun != nil {
			m.cancelRun()
		}
		m.app.Stop()
		return nil
	}

	switch event.Rune() {
	case 'q':
		if m.running && m.cancelRun != nil {
			m.cancelRun()
		}
		m.app.Stop()
		return nil
	case 'r':
		if !m.running {
			m.runSelected()
		}
		return nil
	case 'a':
		if !m.running {
			m.setAllSelected(true)
		}
		return nil
	case 'c':
		if !m.running {
			m.setAllSelected(false)
		} else if m.cancelRun != nil {
			m.cancelRun()
		}
		return nil
	}

	return event
}

func (m *appModel) handleTableKeys(event *tcell.EventKey) *tcell.EventKey {
	row, _ := m.testsTable.GetSelection()
	index := row - 1
	if index < 0 || index >= len(m.tests) {
		return event
	}

	switch event.Key() {
	case tcell.KeyRune:
		switch event.Rune() {
		case ' ':
			if m.running {
				return nil
			}
			m.tests[index].Selected = !m.tests[index].Selected
			m.refreshTestsTable()
			return nil
		case '\n':
			m.showSelectedOutput(index)
			return nil
		}
	case tcell.KeyEnter:
		m.showSelectedOutput(index)
		return nil
	}
	return event
}

func (m *appModel) setAllSelected(selected bool) {
	for i := range m.tests {
		m.tests[i].Selected = selected
	}
	m.refreshTestsTable()
	if selected {
		m.refreshStatus("All tests selected.")
	} else {
		m.refreshStatus("All tests cleared.")
	}
}

func (m *appModel) runSelected() {
	selected := make([]int, 0, len(m.tests))
	for i := range m.tests {
		if m.tests[i].Selected {
			selected = append(selected, i)
		}
	}
	if len(selected) == 0 {
		m.refreshStatus("No tests selected.")
		return
	}

	m.running = true
	m.refreshStatus(fmt.Sprintf("Running %d selected test(s). Press c to cancel.", len(selected)))

	go func() {
		for _, idx := range selected {
			m.app.QueueUpdateDraw(func() {
				m.runningIndex = idx
				m.tests[idx].Status = "running"
				m.liveOutputs[m.tests[idx].Spec.ID] = ""
				m.refreshTestsTable()
				if m.selectedRow == idx {
					m.showSelectedOutput(idx)
				}
				m.refreshStatus(fmt.Sprintf("Running: %s", m.tests[idx].Spec.Title))
			})

			ctx, cancel := context.WithCancel(context.Background())
			m.cancelRun = cancel
			result := m.executeTest(ctx, idx)
			cancel()

			m.app.QueueUpdateDraw(func() {
				m.cancelRun = nil
				m.runningIndex = -1
				m.results[result.Name] = result
				m.tests[idx].Result = result
				if result.Skipped {
					m.tests[idx].Status = "skipped"
				} else if result.Err != nil {
					m.tests[idx].Status = "error"
				} else {
					m.tests[idx].Status = "done"
				}
				m.refreshTestsTable()
				m.refreshSummary()
				if m.selectedRow == idx {
					m.showSelectedOutput(idx)
				}
			})

			if errors.Is(result.Err, context.Canceled) {
				break
			}
		}

		m.app.QueueUpdateDraw(func() {
			m.running = false
			m.cancelRun = nil
			m.runningIndex = -1
			m.refreshSummary()
			m.refreshStatus("Run complete. Use arrows to inspect command output, r to rerun.")
		})
	}()
}

func (m *appModel) executeTest(ctx context.Context, idx int) commandResult {
	test := m.tests[idx]
	if test.Spec.Tool != "" && isToolMissing(m.toolStatuses, test.Spec.Tool) {
		return commandResult{
			Name:    test.Spec.ID,
			Command: test.Spec.Tool,
			Skipped: true,
			Err:     fmt.Errorf("missing required tool: %s", test.Spec.Tool),
		}
	}

	display, bin, args, err := test.Spec.Command(m)
	if err != nil {
		return commandResult{
			Name:    test.Spec.ID,
			Command: display,
			Skipped: true,
			Err:     err,
		}
	}

	result := runCommandStreaming(ctx, test.Spec.ID, display, bin, args, func(chunk string) {
		m.app.QueueUpdateDraw(func() {
			m.liveOutputs[test.Spec.ID] += chunk
			if m.selectedRow == idx {
				m.showSelectedOutput(idx)
			}
		})
	})
	if test.Spec.Filter != nil {
		result.Output = strings.TrimRight(test.Spec.Filter(m, result.Output), "\n")
	}
	if result.Output == "" && result.Err == nil {
		result.Output = "(no matching output)"
	}
	return result
}

func runCommandStreaming(ctx context.Context, name, display, bin string, args []string, onChunk func(string)) commandResult {
	start := time.Now()
	cmd := exec.CommandContext(ctx, bin, args...)
	capture := &liveCapture{onChunk: onChunk}
	cmd.Stdout = capture
	cmd.Stderr = capture
	err := cmd.Run()
	duration := time.Since(start)

	exitCode := 0
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exitCode = exitErr.ExitCode()
		} else if errors.Is(err, context.Canceled) {
			exitCode = -1
		}
	}

	return commandResult{
		Name:     name,
		Command:  display,
		Output:   strings.TrimRight(capture.String(), "\n"),
		Err:      err,
		Duration: duration,
		ExitCode: exitCode,
	}
}

func (m *appModel) commandPreview(idx int) string {
	display, _, _, err := m.tests[idx].Spec.Command(m)
	if err != nil {
		return m.tests[idx].Spec.Title
	}
	return display
}

func (m *appModel) refreshTestsTable() {
	headers := []string{"Sel", "Test", "Status"}
	for col, header := range headers {
		cell := tview.NewTableCell("[::b]" + header).
			SetSelectable(false).
			SetTextColor(tcell.ColorWhite)
		m.testsTable.SetCell(0, col, cell)
	}

	for i, test := range m.tests {
		check := "[ ]"
		if test.Selected {
			check = "[x]"
		}

		status := test.Status
		color := "yellow"
		switch status {
		case "done":
			color = "green"
		case "error":
			color = "red"
		case "skipped":
			color = "gray"
		case "running":
			color = "blue"
		}

		m.testsTable.SetCell(i+1, 0, tview.NewTableCell(check))
		m.testsTable.SetCell(i+1, 1, tview.NewTableCell(test.Spec.Title))
		m.testsTable.SetCell(i+1, 2, tview.NewTableCell(fmt.Sprintf("[%s]%s[-]", color, status)))
	}
}

func (m *appModel) refreshSummary() {
	results := make([]commandResult, 0, len(m.results))
	for _, test := range m.tests {
		if result, ok := m.results[test.Spec.ID]; ok {
			results = append(results, result)
		}
	}
	sort.Slice(results, func(i, j int) bool {
		return results[i].Name < results[j].Name
	})

	diag := buildDiagnosis(m.iface, m.gateway, results)
	var b strings.Builder
	fmt.Fprintf(&b, "[::b]Adapter[::-]\n")
	fmt.Fprintf(&b, "Chipset: [green]%s[-]\n", valueOrNA(diag.Adapter.Chipset))
	fmt.Fprintf(&b, "Driver:  [green]%s[-]\n", valueOrNA(diag.Adapter.Driver))
	fmt.Fprintf(&b, "SSID:    [green]%s[-]\n", valueOrNA(diag.Link.SSID))
	if diag.Link.HasSignal {
		fmt.Fprintf(&b, "Signal:  [green]%d dBm[-]\n", diag.Link.SignalDBm)
	} else {
		fmt.Fprintf(&b, "Signal:  [yellow]n/a[-]\n")
	}
	if diag.Station.HasRetryPct {
		fmt.Fprintf(&b, "Retry:   [%s]%.1f%% (%s)[-]\n", severityTag(diag.Station.RetrySeverity), diag.Station.RetryPct, diag.Station.RetrySeverity)
	} else {
		fmt.Fprintf(&b, "Retry:   [yellow]n/a[-]\n")
	}
	if diag.RouterPing.HasRTT {
		fmt.Fprintf(&b, "Router:  [%s]%.1f ms avg[-]\n", severityTag(latencySeverity(diag.RouterPing.AvgMs)), diag.RouterPing.AvgMs)
	} else {
		fmt.Fprintf(&b, "Router:  [yellow]n/a[-]\n")
	}
	fmt.Fprintf(&b, "\n[::b]Overall[::-] [%s::b]%s[-]\n", severityTag(diag.Overall), strings.ToUpper(valueOrNA(diag.Overall)))
	if len(diag.Reasons) > 0 {
		fmt.Fprintf(&b, "%s\n", diag.Reasons[0])
	}
	if missing := missingTools(m.toolStatuses); len(missing) > 0 {
		fmt.Fprintf(&b, "\n[red]Missing tools:[-] %s\n", strings.Join(missing, ", "))
	}

	m.summaryView.SetText(b.String())
}

func (m *appModel) refreshStatus(text string) {
	help := "space: toggle  r: run  a: all  c: clear/cancel  q: quit"
	m.statusView.SetText(fmt.Sprintf("[green]%s[-]\n[gray]%s[-]", text, help))
}

func (m *appModel) showSelectedOutput(idx int) {
	if idx < 0 || idx >= len(m.tests) {
		return
	}
	test := m.tests[idx]
	m.outputView.Clear()
	fmt.Fprintf(m.outputView, "$ %s\n\n", m.commandPreview(idx))
	if liveOutput := m.liveOutputs[test.Spec.ID]; liveOutput != "" && test.Status == "running" {
		fmt.Fprint(m.outputView, liveOutput)
		return
	}
	if result, ok := m.results[test.Spec.ID]; ok {
		if result.Skipped {
			fmt.Fprintf(m.outputView, "skipped: %v\n", result.Err)
			return
		}
		if result.Err != nil && result.Output == "" {
			fmt.Fprintf(m.outputView, "error: %v\n", result.Err)
			return
		}
		if result.Output == "" {
			fmt.Fprintln(m.outputView, "(no output)")
			return
		}
		fmt.Fprintln(m.outputView, result.Output)
		return
	}
	fmt.Fprintf(m.outputView, "(%s)\n", test.Spec.Description)
}

func defaultTests() []testState {
	return []testState{
		{Spec: testSpec{
			ID:          "adapter_info",
			Title:       "Adapter Info",
			Description: "Driver, version, and firmware details.",
			Tool:        "ethtool",
			Command: func(m *appModel) (string, string, []string, error) {
				return fmt.Sprintf("ethtool -i %s", m.iface), "ethtool", []string{"-i", m.iface}, nil
			},
		}},
		{Spec: testSpec{
			ID:          "link_status",
			Title:       "Link Status",
			Description: "Current SSID, BSSID, signal, and link rates.",
			Tool:        "iw",
			Command: func(m *appModel) (string, string, []string, error) {
				return fmt.Sprintf("iw dev %s link", m.iface), "iw", []string{"dev", m.iface, "link"}, nil
			},
		}},
		{Spec: testSpec{
			ID:          "active_connection",
			Title:       "Active Connection",
			Description: "NetworkManager active connections.",
			Tool:        "nmcli",
			Command: func(*appModel) (string, string, []string, error) {
				return "nmcli connection show --active", "nmcli", []string{"connection", "show", "--active"}, nil
			},
		}},
		{Spec: testSpec{
			ID:          "nearby_wifi",
			Title:       "Nearby Networks",
			Description: "Visible access points.",
			Tool:        "nmcli",
			Command: func(*appModel) (string, string, []string, error) {
				return "nmcli -f IN-USE,SSID,BSSID,CHAN,RATE,SIGNAL dev wifi list", "nmcli", []string{"-f", "IN-USE,SSID,BSSID,CHAN,RATE,SIGNAL", "dev", "wifi", "list"}, nil
			},
		}},
		{Spec: testSpec{
			ID:          "power_save",
			Title:       "Power Save",
			Description: "Wi-Fi power save state.",
			Tool:        "iw",
			Command: func(m *appModel) (string, string, []string, error) {
				return fmt.Sprintf("iw dev %s get power_save", m.iface), "iw", []string{"dev", m.iface, "get", "power_save"}, nil
			},
		}},
		{Spec: testSpec{
			ID:          "station_dump",
			Title:       "Station Stats",
			Description: "Station statistics and retry metrics.",
			Tool:        "iw",
			Command: func(m *appModel) (string, string, []string, error) {
				return fmt.Sprintf("iw dev %s station dump", m.iface), "iw", []string{"dev", m.iface, "station", "dump"}, nil
			},
		}},
		{Spec: testSpec{
			ID:          "driver_stats",
			Title:       "Driver Stats",
			Description: "Driver-level transmit and receive statistics.",
			Tool:        "ethtool",
			Command: func(m *appModel) (string, string, []string, error) {
				return fmt.Sprintf("ethtool -S %s", m.iface), "ethtool", []string{"-S", m.iface}, nil
			},
		}},
		{Spec: testSpec{
			ID:          "hardware_info",
			Title:       "Hardware Info",
			Description: "PCIe Wi-Fi chipset identification.",
			Tool:        "lspci",
			Command: func(*appModel) (string, string, []string, error) {
				return "lspci -nnk", "lspci", []string{"-nnk"}, nil
			},
			Filter: func(_ *appModel, output string) string {
				return filterHardwareInfo(output)
			},
		}},
		{Spec: testSpec{
			ID:          "pcie_errors",
			Title:       "PCIe Error Check",
			Description: "Kernel PCIe and AER error log review.",
			Tool:        "dmesg",
			Command: func(*appModel) (string, string, []string, error) {
				return "dmesg", "dmesg", nil, nil
			},
			Filter: func(_ *appModel, output string) string {
				return strings.Join(parsePCIeErrors(output), "\n")
			},
		}},
		{Spec: testSpec{
			ID:          "driver_logs",
			Title:       "Driver Log Check",
			Description: "Driver-specific error messages from dmesg.",
			Tool:        "dmesg",
			Command: func(m *appModel) (string, string, []string, error) {
				driver := currentDriver(m)
				if driver == "" {
					return "dmesg", "", nil, errors.New("driver name unavailable; run Adapter Info first")
				}
				return fmt.Sprintf("dmesg (filtered for %s)", driver), "dmesg", nil, nil
			},
			Filter: func(m *appModel, output string) string {
				return strings.Join(parseDriverLogs(output, currentDriver(m)), "\n")
			},
		}},
		{Spec: testSpec{
			ID:          "router_ping",
			Title:       "Router Ping",
			Description: "Latency and loss to the default gateway.",
			Tool:        "ping",
			Command: func(m *appModel) (string, string, []string, error) {
				if m.gateway == "" {
					return "ping -c 10 <gateway>", "", nil, errors.New("gateway not detected")
				}
				return fmt.Sprintf("ping -c 10 %s", m.gateway), "ping", []string{"-c", "10", m.gateway}, nil
			},
		}},
		{Spec: testSpec{
			ID:          "internet_ping",
			Title:       "Internet Ping",
			Description: "Latency and loss to 1.1.1.1.",
			Tool:        "ping",
			Command: func(*appModel) (string, string, []string, error) {
				return "ping -c 10 1.1.1.1", "ping", []string{"-c", "10", "1.1.1.1"}, nil
			},
		}},
	}
}

func currentDriver(m *appModel) string {
	if result, ok := m.results["adapter_info"]; ok {
		driver := parseDriver(result.Output)
		if driver != "" {
			return driver
		}
	}
	return ""
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

func filterHardwareInfo(output string) string {
	lines := strings.Split(output, "\n")
	var blocks []string
	for i := 0; i < len(lines); i++ {
		lower := strings.ToLower(lines[i])
		if strings.Contains(lower, "network controller") || strings.Contains(lower, "wireless") {
			end := i + 4
			if end > len(lines) {
				end = len(lines)
			}
			block := strings.Join(lines[i:end], "\n")
			blocks = append(blocks, strings.TrimSpace(block))
		}
	}
	return strings.Join(blocks, "\n\n")
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

func severityTag(severity string) string {
	switch severity {
	case "Excellent", "Good":
		return "green"
	case "Fair":
		return "yellow"
	case "Poor", "Severe":
		return "red"
	default:
		return "white"
	}
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

var _ io.Writer = (*liveCapture)(nil)
