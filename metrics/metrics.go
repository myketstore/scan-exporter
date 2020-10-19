package metrics

import (
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/go-redis/redis"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/rs/zerolog"

	"github.com/devops-works/scan-exporter/common"
	"github.com/devops-works/scan-exporter/handlers"
)

// ResMsg holds all the data received from a scan.
type ResMsg struct {
	Name            string
	IP              string
	Protocol        string
	OpenPorts       []string
	UnexpectedPorts []string
	ClosedPorts     []string
}

var (
	numOfTargets = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "scanexporter_targets_number_total",
		Help: "Number of targets detected in config file.",
	})

	numOfDownTargets = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "scanexporter_icmp_not_responding_total",
		Help: "Number of targets that doesn't respond to pings.",
	})

	unexpectedPorts = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "scanexporter_unexpected_open_ports_total",
		Help: "Number of ports that are open, and shouldn't be.",
	},
		[]string{
			"proto",
			"name",
		},
	)

	openPorts = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "scanexporter_open_ports_total",
		Help: "Number of ports that are open.",
	},
		[]string{
			"proto",
			"name",
		},
	)

	closedPorts = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "scanexporter_unexpected_closed_ports_total",
		Help: "Number of ports that are closed and shouldn't be.",
	},
		[]string{
			"proto",
			"name",
		},
	)

	diffPorts = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "scanexporter_diff_ports_total",
		Help: "Number of ports that are different from previous scan.",
	},
		[]string{
			"proto",
			"name",
		},
	)

	notRespondingList = []string{}
	rdb               *redis.Client
)

// Handle receives data from a finished scan. It also receive the number of targets declared in config file.
func Handle(res ResMsg) error {
	var m sync.Mutex
	if res.Protocol == "icmp" {
		icmpNotResponding(res.OpenPorts, res.IP, &m)
		return nil
	}

	setName := res.IP + "/" + res.Protocol

	// Expose the number of unexpected ports.
	unexpectedPorts.WithLabelValues(res.Protocol, res.Name).Set(float64(len(res.UnexpectedPorts)))

	// Expose the number of open ports.
	openPorts.WithLabelValues(res.Protocol, res.Name).Set(float64(len(res.OpenPorts)))

	// Expose the number of closed ports.
	closedPorts.WithLabelValues(res.Protocol, res.Name).Set(float64(len(res.ClosedPorts)))

	// Redis
	prev, err := readSet(rdb, setName)
	if err != nil {
		return err
	}

	diff := common.CompareStringSlices(prev, res.OpenPorts)
	diffPorts.WithLabelValues(res.Protocol, res.Name).Set(float64(diff))

	wipeSet(rdb, setName)
	err = writeSet(rdb, setName, res.OpenPorts)
	if err != nil {
		return err
	}

	return nil
}

// StartServ starts the prometheus server.
func StartServ(l zerolog.Logger, nTargets int) {
	// Set the number of targets. This is done once.
	numOfTargets.Set(float64(nTargets))

	// Set the number of hosts that doesn't respond to ping to 0.
	numOfDownTargets.Set(0)

	// Init Redis client.
	if err := initRedisClient(); err != nil {
		l.Error().Msgf("redis init error : %v", err)
	}

	srv := &http.Server{
		Addr:         ":2112",
		Handler:      handlers.HandleFunc(),
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 10 * time.Second,
	}
	l.Error().Msgf("server error : %v", srv.ListenAndServe())
}

// icmpNotResponding adjust the numOfDownTargets variable depending of the current and the previous
// status of the target.
func icmpNotResponding(ports []string, IP string, m *sync.Mutex) {
	isResponding := true
	if len(ports) == 0 {
		isResponding = !isResponding
	}

	m.Lock()
	alreadyNotResponding := common.StringInSlice(IP, notRespondingList)
	m.Unlock()

	if isResponding && alreadyNotResponding {
		// Wasn't responding, but now is ok
		numOfDownTargets.Dec()

		for index := range notRespondingList {
			if notRespondingList[index] == IP {
				// Remove the element at index i from a.
				m.Lock()
				notRespondingList[index] = notRespondingList[len(notRespondingList)-1]
				notRespondingList[len(notRespondingList)-1] = ""
				notRespondingList = notRespondingList[:len(notRespondingList)-1]
				m.Unlock()
			}
		}

	} else if !isResponding && !alreadyNotResponding {
		// First time it doesn't respond.
		// Increment the number of down targets.
		numOfDownTargets.Inc()
		// Add IP to notRespondingList.
		m.Lock()
		notRespondingList = append(notRespondingList, IP)
		m.Unlock()
	}
	// Else, everything is good, do nothing or everything is as bad as it was, so do nothing too.
}

// writeSet writes items in a Redis dataset called setName.
func writeSet(rdb *redis.Client, setName string, items []string) error {
	for _, item := range items {
		err := rdb.SAdd(setName, item).Err()
		if err != nil {
			return err
		}
	}

	return nil
}

// readSet reads items from a Redis dataset called setName.
func readSet(rdb *redis.Client, setName string) ([]string, error) {
	items, err := rdb.SMembers(setName).Result()
	if err != nil {
		return nil, err
	}
	return items, nil
}

// wipeSet clear a Redis dataset.
func wipeSet(rdb *redis.Client, setName string) {
	rdb.Del(setName)
}

// initRedisClient initiates a new Redis client item.
func initRedisClient() error {
	redisURL := os.Getenv("REDIS_URL")
	if redisURL == "" {
		redisURL = "redis://localhost:6379/0"
	}

	opt, err := redis.ParseURL(redisURL)
	if err != nil {
		return err
	}

	rdb := redis.NewClient(opt)

	_, err = rdb.Ping().Result()
	if err != nil {
		return err
	}
	return nil
}

// init is called at package initialisation. It initialize prometheus variables.
func init() {
	prometheus.MustRegister(numOfTargets)
	prometheus.MustRegister(numOfDownTargets)
	prometheus.MustRegister(unexpectedPorts)
	prometheus.MustRegister(openPorts)
	prometheus.MustRegister(closedPorts)
	prometheus.MustRegister(diffPorts)
}
