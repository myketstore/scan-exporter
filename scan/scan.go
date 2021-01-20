package scan

import (
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"github.com/sparrc/go-ping"

	"github.com/devops-works/scan-exporter/common"
	"github.com/devops-works/scan-exporter/metrics"
)

// Target holds an IP and a range of ports to scan.
type Target struct {
	name      string
	ip        string
	workers   int
	protos    map[string]protocol
	metrics   metrics.MetricsManager
	logger    zerolog.Logger
	startTime map[string]time.Time
	timeMutex sync.Mutex

	// those maps hold the protocol and the ports.
	portsToScan map[string][]string
}

// protocol holds everything that is given in config file about a specific protocol.
type protocol struct {
	period   string
	rng      string
	expected string
}

// jobMsg contains the data which is sent to/by workers.
type jobMsg struct {
	id       string
	jobCount int
	ip       string
	protocol string
	ports    []string
}

// New checks that target specification is valid, and if target is responding.
func New(name, ip string, workers int, m metrics.MetricsManager, o ...func(*Target) error) (*Target, error) {
	if i := net.ParseIP(ip); i == nil {
		return nil, fmt.Errorf("unable to parse IP address %s", ip)
	}

	t := &Target{
		name:        name,
		ip:          ip,
		workers:     workers,
		metrics:     m,
		protos:      make(map[string]protocol),
		portsToScan: make(map[string][]string),
		startTime:   make(map[string]time.Time),
	}

	for _, f := range o {
		if err := f(t); err != nil {
			return nil, err
		}
	}

	return t, nil
}

// WithPorts adds TCP or UDP ports specifications to scan target.
func WithPorts(proto, period, rng, expected string) func(*Target) error {
	return func(t *Target) error {
		return t.setPorts(proto, period, rng, expected)
	}
}

// setPorts populates the target's ports arrays. It calls readPortRange to validate
// port range.
func (t *Target) setPorts(proto, period, rng, exp string) error {
	if !common.StringInSlice(proto, []string{"udp", "tcp", "icmp"}) {
		return fmt.Errorf("unsupported protocol %q for target %s", proto, t.name)
	}

	t.protos[proto] = protocol{
		period:   period,
		rng:      rng,
		expected: exp,
	}

	var err error
	t.portsToScan[proto], err = readPortsRange(rng)
	if err != nil {
		return err
	}

	return nil
}

// WithLogger adds logger specifications to scan target.
func WithLogger(l zerolog.Logger) func(*Target) error {
	return func(t *Target) error {
		return t.setLogger(l)
	}
}

// setLogger sets the logger on a target.
func (t *Target) setLogger(l zerolog.Logger) error {
	t.logger = l
	return nil
}

// Name returns target name.
func (t *Target) Name() string {
	return t.name
}

// Run should be called using `go` and will run forever running the scanning
// schedule.
func (t *Target) Run() {
	// Create trigger channel for scheduler.
	trigger := make(chan string, 100)
	workersCount := t.workers

	protoList := t.getWantedProto()

	// Start scheduler.
	go t.scheduler(trigger, protoList)

	// Create channel to send jobMsg.
	jobsChan := make(chan jobMsg, 3*workersCount)

	// Create channel to send scan results.
	resChan := make(chan jobMsg, 3*workersCount)

	// postScan allow receiver to send scan results into the redis gouroutine.
	postScan := make(chan metrics.ResMsg, 3*workersCount)

	// scan to metrics goroutine.
	go t.sendResults(postScan)

	// Create receiver that will receive done jobs.
	go t.receiver(resChan, postScan)

	// Start required number (n) of workers.
	for w := 0; w < workersCount; w++ {
		go t.worker(jobsChan, resChan)
	}
	t.logger.Info().Msgf("%d workers started", workersCount)

	// Infinite loop that wait for trigger.
	for {
		select {
		case proto := <-trigger:
			if t.startTime[proto].IsZero() {
				t.logger.Warn().Msgf("%s: a scan already started", t.name)
				break
			}
			// Create n jobs containing 1/n of total scan range.
			jobs, err := t.createJobs(proto)
			if err != nil {
				t.logger.Error().Msgf("error creating jobs")
				return
			}

			jobID := common.GenerateRandomString(10)

			// Send jobs to channel.
			for _, j := range jobs {
				j.id = jobID
				j.jobCount = len(jobs)
				jobsChan <- j
			}
		}
	}
}

// receiver is created once.
// It waits for incoming results (sent by workers when a port is open).
func (t *Target) receiver(resChan chan jobMsg, postScan chan metrics.ResMsg) {
	// openPorts holds all openPorts for a jobID
	var openPorts = make(map[string][]string)
	var jobsStarted = make(map[string]int)

	for {
		select {
		case res := <-resChan:
			jobsStarted[res.id]++

			// Append ports.
			openPorts[res.id] = append(openPorts[res.id], res.ports...)

			if jobsStarted[res.id] == res.jobCount {
				// Do not log ICMP scan duration
				if res.protocol != "icmp" {
					t.logger.Info().Msgf("%s/%s scan duration %s", t.name, res.protocol, time.Since(t.startTime[res.protocol]))
				}
				t.setTimeTo(res.protocol, time.Time{})

				// results holds all the informations about a finished scan.
				results := metrics.ResMsg{
					Name:      t.Name(),
					IP:        res.ip,
					Protocol:  res.protocol,
					OpenPorts: openPorts[res.id],
				}

				var unexpectedPorts, closedPorts []string
				var err error

				// Check diff between expected and open.
				if results.Protocol != "icmp" {
					unexpectedPorts, closedPorts, err = t.checkAccordance(results.Protocol, results.OpenPorts)
					if err != nil {
						t.logger.Error().Msgf("error occured while checking port accordance: %s", err)
					}
				}

				t.recap(t.Name(), unexpectedPorts, closedPorts, t.logger)

				results.UnexpectedPorts = unexpectedPorts
				results.ClosedPorts = closedPorts

				// send results to redis channel
				postScan <- results
			}
		}
	}
}

// checkAccordance verifies if the open ports list matches the expected ports list given in config.
// It returns a list of unexpected ports and closedPorts :
// unexpectedPorts holds the ports that are open but not expected;
// closedPorts holds the ports that are expected but not opened.
// The list is empty if everything is ok.
func (t *Target) checkAccordance(proto string, open []string) ([]string, []string, error) {
	var unexpectedPorts = []string{}
	var closedPorts = []string{}

	expected, err := readPortsRange(t.protos[proto].expected)
	if err != nil {
		return unexpectedPorts, closedPorts, err
	}

	// If the port is open but not expected
	for _, port := range open {
		if !common.StringInSlice(port, expected) {
			unexpectedPorts = append(unexpectedPorts, port)
		}
	}

	// If the port is expected but not open
	for _, port := range expected {
		if !common.StringInSlice(port, open) {
			closedPorts = append(closedPorts, port)
		}
	}

	return unexpectedPorts, closedPorts, nil
}

// recap logs one-line logs if there is some unexpected or closed ports in the last scan.
// If the lists are empty, nothing is logged.
// Logs are written with warn level.
func (t *Target) recap(name string, unexpected, closed []string, l zerolog.Logger) {
	if len(unexpected) > 0 {
		sorted, err := sortPorts(unexpected)
		if err != nil {
			t.logger.Error().Msgf("[%s] error sorting unexpected ports", name)
		}
		t.logger.Warn().Msgf("[%s] %s unexpected", name, sorted)
	}

	if len(closed) > 0 {
		sorted, err := sortPorts(closed)
		if err != nil {
			t.logger.Error().Msgf("[%s] error sorting closed ports", name)
			sorted = closed
		}
		t.logger.Warn().Msgf("[%s] %s closed", name, sorted)
	}
}

// getWantedProto checks if a protocol is set in config file and returns a slice
// of wanted protocols.
func (t *Target) getWantedProto() []string {
	var protoList = []string{}
	if p := t.protos["tcp"].period; p != "" {
		protoList = append(protoList, "tcp")
	}

	if p := t.protos["udp"].period; p != "" {
		protoList = append(protoList, "udp")
	}

	if p := t.protos["icmp"].period; p != "" {
		protoList = append(protoList, "icmp")
	}

	return protoList
}

// worker is a neverending goroutine which waits for incoming jobs.
// Depending of the job's protocol, it launches different kinds of scans.
// If a scan is successful, it sends a resMsg to the receiver.
func (t *Target) worker(jobsChan chan jobMsg, resChan chan jobMsg) {
	for {
		select {
		case job := <-jobsChan:
			// res holds the result of the scan and some more infos
			res := jobMsg{
				id:       job.id,
				ip:       job.ip,
				jobCount: job.jobCount,
				protocol: job.protocol,
			}
			switch res.protocol {
			case "tcp":
				// Launch TCP scan
				for _, p := range job.ports {
					success := tcpScan(job.ip, p)
					if success {
						// Fill res.ports with open ports
						res.ports = append(res.ports, p)
						t.logger.Debug().Msgf("%s/%s open", p, res.protocol)
					}
				}
				resChan <- res
			case "udp":
				// Launch UDP scan
				for _, p := range job.ports {
					success, err := udpScan(job.ip, p)
					if err != nil {
						t.logger.Warn().Msgf("error while scanning udp: %v", err)
						continue
					}
					if success {
						// Fill res.ports with open ports
						res.ports = append(res.ports, p)
						t.logger.Debug().Msgf("%s/%s open", p, res.protocol)
					}
				}
				resChan <- res
			case "icmp":
				success, err := icmpScan(job.ip)
				if err != nil {
					t.logger.Warn().Msgf("error while scanning tcp: %v", err)
					continue
				}
				if success {
					res.ports = append(res.ports, "1")
					t.logger.Debug().Msgf("%s responds", res.protocol)
				} else {
					t.logger.Warn().Msgf("%s/%s doesn't responds", t.name, res.protocol)
				}
				resChan <- res
			}
		}
	}
}

// createJobs split portsToScan from a specified protocol into an equal number
// of jobs that will be returned.
func (t *Target) createJobs(proto string) ([]jobMsg, error) {
	// init jobs slice
	jobs := []jobMsg{}

	// check protocol accordance
	if _, ok := t.portsToScan[proto]; !ok {
		return nil, fmt.Errorf("no such protocol %q in current protocol list", proto)
	}

	// if proto is ICMP, we do not need to scan ports
	if proto == "icmp" {
		return []jobMsg{
			jobMsg{ip: t.ip, protocol: proto},
		}, nil
	}

	defSize := len(t.portsToScan[proto]) / t.workers
	numBigger := len(t.portsToScan[proto]) - defSize*t.workers

	size := defSize + 1
	for i, idx := 0, 0; i < t.workers; i++ {
		if i == numBigger {
			size--
			if size == 0 {
				break // 0 ports left to scan
			}
		}
		jobs = append(jobs, jobMsg{
			ip:       t.ip,
			protocol: proto,
			ports:    t.portsToScan[proto][idx : idx+size],
		})
		idx += size
	}
	return jobs, nil
}

// readPortsRange transforms a range of ports given in conf to an array of
// effective ports.
func readPortsRange(ranges string) ([]string, error) {
	ports := []string{}

	parts := strings.Split(ranges, ",")

	for _, spec := range parts {
		if spec == "" {
			continue
		}
		switch spec {
		case "all":
			for port := 1; port <= 65535; port++ {
				ports = append(ports, strconv.Itoa(port))
			}
		case "reserved":
			for port := 1; port < 1024; port++ {
				ports = append(ports, strconv.Itoa(port))
			}
		default:
			var decomposedRange []string

			if !strings.Contains(spec, "-") {
				decomposedRange = []string{spec, spec}
			} else {
				decomposedRange = strings.Split(spec, "-")
			}

			min, err := strconv.Atoi(decomposedRange[0])
			if err != nil {
				return nil, err
			}
			max, err := strconv.Atoi(decomposedRange[len(decomposedRange)-1])
			if err != nil {
				return nil, err
			}

			if min > max {
				return nil, fmt.Errorf("lower port %d is higher than high port %d", min, max)
			}
			if max > 65535 {
				return nil, fmt.Errorf("port %d is higher than max port", max)
			}
			for i := min; i <= max; i++ {
				ports = append(ports, strconv.Itoa(i))
			}
		}
	}

	return ports, nil
}

// scheduler create tickers for each protocol given and when they tick,
// it sends the protocol name in the trigger's channel in order to alert
// feeder that a scan must be started.
func (t *Target) scheduler(trigger chan string, protocols []string) {
	var tcpTicker, udpTicker, icmpTicker *time.Ticker
	for _, proto := range protocols {
		switch proto {
		case "tcp":
			tcpFreq, err := getDuration(t.protos[proto].period)
			if err != nil {
				t.logger.Error().Msgf("error getting %s frequency in scheduler: %s", proto, err)
			}
			tcpTicker = time.NewTicker(tcpFreq)
			// starts its own ticker
			go t.ticker(trigger, proto, tcpTicker)
		case "udp":
			udpFreq, err := getDuration(t.protos[proto].period)
			if err != nil {
				t.logger.Error().Msgf("error getting %s frequency in scheduler: %s", proto, err)
			}
			udpTicker = time.NewTicker(udpFreq)
			// starts its own ticker
			go t.ticker(trigger, proto, udpTicker)
		case "icmp":
			icmpFreq, err := getDuration(t.protos[proto].period)
			if err != nil {
				t.logger.Error().Msgf("error getting %s frequency in scheduler: %s", proto, err)
			}
			icmpTicker = time.NewTicker(icmpFreq)
			// starts its own ticker
			go t.ticker(trigger, proto, icmpTicker)
		}
	}
}

// sendREsults is used as an interface between scan and metrics packages.
// It receives results from the runner, and call metrics.ReceiveResults interface.
func (t *Target) sendResults(resChan chan metrics.ResMsg) {
	for {
		select {
		case res := <-resChan:
			err := t.metrics.ReceiveResults(res)
			if err != nil {
				t.logger.Error().Err(err).Msg("error handling results")
			}
		}
	}
}

func (t *Target) setTimeTo(proto string, time time.Time) {
	t.timeMutex.Lock()
	t.startTime[proto] = time
	t.timeMutex.Unlock()
}

// ticker handles a protocol ticker, and send the protocol in a channel when the ticker ticks
func (t *Target) ticker(trigger chan string, proto string, protTicker *time.Ticker) {
	// First scan at the start
	t.setTimeTo(proto, time.Now())
	trigger <- proto

	for {
		select {
		case <-protTicker.C:
			t.setTimeTo(proto, time.Now())
			trigger <- proto
		}
	}
}

// tcpScan scans an ip and returns true if the port responds.
func tcpScan(ip, port string) bool {
	conn, err := net.DialTimeout("tcp", ip+":"+port, 2*time.Second)
	if err != nil {
		if strings.Contains(err.Error(), "too many open files") {
			// Wait and retry to scan the port by calling the same function
			// recursively and returning it's return value.
			time.Sleep(2 * time.Second)
			return tcpScan(ip, port)
		} else {
			return false
		}
	}

	conn.Close()
	return true
}

// udpScan scans an ip and returns true if the port responds.
func udpScan(ip, port string) (bool, error) {
	serverAddr, err := net.ResolveUDPAddr("udp", ip+":"+port)
	if err != nil {
		return false, err
	}
	conn, err := net.DialUDP("udp", nil, serverAddr)
	if err != nil {
		return false, err
	}
	defer conn.Close()

	// write 3 times to the udp socket and check
	// if there's any kind of error
	errorCount := 0
	for i := 0; i < 3; i++ {
		buf := []byte{'\000'}
		_, err := conn.Write(buf)
		if err != nil {
			errorCount++
		}
	}
	// port is closed
	return errorCount <= 0, nil
}

// icmpScan pings a host
func icmpScan(ip string) (bool, error) {
	pinger, err := ping.NewPinger(ip)
	if err != nil {
		return false, err
	}
	pinger.SetPrivileged(true)
	pinger.Count = 3
	pinger.Timeout = 2 * time.Second
	pinger.Run()
	stats := pinger.Statistics()

	if stats.PacketLoss != 0 {
		return false, nil
	}
	return true, nil
}

// getDuration transforms a protocol's period into a time.Duration value.
func getDuration(period string) (time.Duration, error) {
	// only hours, minutes and seconds are handled by ParseDuration
	if strings.ContainsAny(period, "hms") {
		t, err := time.ParseDuration(period)
		if err != nil {
			return 0, err
		}
		return t, nil
	}

	sep := strings.Split(period, "d")
	days, err := strconv.Atoi(sep[0])
	if err != nil {
		return 0, err
	}

	t := time.Duration(days) * time.Hour * 24
	return t, nil
}

// sortPorts sorts the ports in a string slice in numerical order. For example,
// 22 will be before 1337, which is not the case when using sort.Strings.
func sortPorts(ports []string) ([]string, error) {
	var sorted []string
	var holder []int

	for _, p := range ports {
		pint, err := strconv.Atoi(p)
		if err != nil {
			return nil, err
		}

		holder = append(holder, pint)
	}

	sort.Ints(holder)

	for _, pint := range holder {
		sorted = append(sorted, strconv.Itoa(pint))
	}

	return sorted, nil
}
