package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/felixge/httpsnoop"
	"github.com/justinas/alice"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/hlog"
)

var (
	logger = zerolog.New(os.Stdout).With().
		Timestamp().
		Logger()

	listenAddress = flag.String("listen-address", ":4243", "The address to listen on for HTTP requests.")
	metricsPath   = flag.String("metrics-path", "/metrics", "Path under which to expose metrics.")
	neoHubAddress = flag.String("neohub-address", "", "Required: Address to neohub eg. 192.168.1.1:4242")
)

func main() {
	flag.Parse()

	// Create non-global registry.
	registry := prometheus.NewRegistry()

	collector := newNeoHubCollector(*neoHubAddress)

	registry.MustRegister(
		// Add go runtime metrics
		collectors.NewGoCollector(),
		// Add process collectors
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		collector,
	)

	go collector.loop(context.Background())

	c := alice.New()

	// Install the logger handler with default output on the console
	c = c.Append(hlog.NewHandler(logger))

	// Install some provided extra handler to set some request's context fields.
	// Thanks to that handler, all our logs will come with some prepopulated fields.
	c = c.Append(hlog.AccessHandler(func(r *http.Request, status, size int, duration time.Duration) {
		hlog.FromRequest(r).Debug().
			Str("method", r.Method).
			Stringer("url", r.URL).
			Int("status", status).
			Int("size", size).
			Dur("duration", duration).
			Msg("")
	}))
	c = c.Append(hlog.RemoteAddrHandler("ip"))
	c = c.Append(hlog.UserAgentHandler("user_agent"))
	c = c.Append(hlog.RefererHandler("referer"))
	c = c.Append(hlog.RequestIDHandler("req_id", "Request-Id"))

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<html>
			<head><title>Heatmiser Exporter</title></head>
			<body>
			<h1>Heatmiser Exporter</h1>
			<p><a href="` + *metricsPath + `">Metrics</a></p>
			</body>
			</html>`))
	})
	// Expose /metrics HTTP endpoint using the created custom registry.
	mux.Handle(
		*metricsPath,
		promhttp.HandlerFor(
			registry,
			promhttp.HandlerOpts{}),
	)

	log.Fatalln(http.ListenAndServe(*listenAddress, c.ThenFunc(func(w http.ResponseWriter, r *http.Request) {
		httpsnoop.CaptureMetrics(mux, w, r)
	})))
}

func newNeoHubCollector(host string) *neoHubCollector {
	return &neoHubCollector{
		host: host,
		temperature: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Namespace: "heatmiser",
				Name:      "temperature_celsius",
			},
			[]string{"zone", "sensor"},
		),
		mode: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Namespace: "heatmiser",
				Name:      "mode",
			},
			[]string{"zone", "mode"},
		),
		lastSeen: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Namespace: "heatmiser",
				Name:      "last_seen_unixtime",
			},
			[]string{"zone"},
		),
		lastPolled: prometheus.NewGauge(
			prometheus.GaugeOpts{
				Namespace: "heatmiser",
				Name:      "last_polled_unixtime",
			},
		),
		pollDuration: prometheus.NewHistogram(
			prometheus.HistogramOpts{
				Namespace: "heatmiser",
				Name:      "poll_duration_seconds",
			},
		),
	}
}

type neoHubCollector struct {
	host string

	temperature  *prometheus.GaugeVec
	mode         *prometheus.GaugeVec
	lastSeen     *prometheus.GaugeVec
	lastPolled   prometheus.Gauge
	pollDuration prometheus.Histogram
}

func boolToFloat(b bool) float64 {
	if b {
		return 1.0
	}

	return 0.0
}

func (c *neoHubCollector) loop(ctx context.Context) {

	update := func() {
		ctx, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()

		if err := c.update(ctx); err != nil {
			logger.Error().Err(err).Msg("unable to gather status from neohub")
		}

	}

	update()

	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		update()
	}
}

func (c *neoHubCollector) update(ctx context.Context) error {
	before := time.Now()

	status, err := readStatus(ctx, c.host)
	if err != nil {
		return err
	}

	// record duration
	diff := time.Since(before)
	defer func() {
		c.pollDuration.Observe(diff.Seconds())
		c.lastPolled.SetToCurrentTime()
	}()

	c.temperature.Reset()
	c.mode.Reset()
	for _, d := range status.LiveData.Devices {
		// replace zone name with a prometheus one
		zoneName := strings.Map(func(r rune) rune {
			if r > '\u007a' || r < '\u0021' {
				return '-'
			}
			return r
		}, strings.ToLower(d.ZoneName))

		if engineersData, ok := status.Engineers[d.ZoneName]; ok {
			c.lastSeen.WithLabelValues(zoneName).Set(float64(engineersData.Timestamp))

			if !d.Offline {
				c.temperature.WithLabelValues(zoneName, "floor-limit").Set(engineersData.FloorLimit)
			}
		}

		if d.Offline {
			c.mode.WithLabelValues(zoneName, "offline").Set(1)
			continue
		}

		c.mode.WithLabelValues(zoneName, "offline").Set(0)
		c.mode.WithLabelValues(zoneName, "heat").Set(boolToFloat(d.HeatOn))
		c.mode.WithLabelValues(zoneName, "preheat").Set(boolToFloat(d.PreheatActive))
		c.mode.WithLabelValues(zoneName, "holiday").Set(boolToFloat(d.Holiday))
		c.mode.WithLabelValues(zoneName, "lock").Set(boolToFloat(d.Lock))
		c.mode.WithLabelValues(zoneName, "away").Set(boolToFloat(d.Away))
		if v, err := strconv.ParseFloat(d.ActualTemp, 64); err == nil {
			c.temperature.WithLabelValues(zoneName, "room").Set(v)
		}
		c.temperature.WithLabelValues(zoneName, "floor").Set(d.CurrentFloorTemperature)
	}
	return nil
}

func (c *neoHubCollector) Collect(ch chan<- prometheus.Metric) {
	c.mode.Collect(ch)
	c.temperature.Collect(ch)
	c.lastSeen.Collect(ch)
	c.lastPolled.Collect(ch)
	c.pollDuration.Collect(ch)
}

func (c *neoHubCollector) Describe(ch chan<- *prometheus.Desc) {
	c.mode.Describe(ch)
	c.temperature.Describe(ch)
	c.lastSeen.Describe(ch)
	c.lastPolled.Describe(ch)
	c.pollDuration.Describe(ch)
}

type NeoHubDevice struct {
	ActiveLevel             int      `json:"ACTIVE_LEVEL"`
	ActiveProfile           int      `json:"ACTIVE_PROFILE"`
	ActualTemp              string   `json:"ACTUAL_TEMP"`
	AvailableModes          []string `json:"AVAILABLE_MODES"`
	Away                    bool     `json:"AWAY"`
	CoolMode                bool     `json:"COOL_MODE"`
	CoolOn                  bool     `json:"COOL_ON"`
	CoolTemp                float64  `json:"COOL_TEMP"`
	CurrentFloorTemperature float64  `json:"CURRENT_FLOOR_TEMPERATURE"`
	Date                    string   `json:"DATE"`
	DeviceID                int      `json:"DEVICE_ID"`
	FanControl              string   `json:"FAN_CONTROL"`
	FanSpeed                string   `json:"FAN_SPEED"`
	FloorLimit              bool     `json:"FLOOR_LIMIT"`
	HcMode                  string   `json:"HC_MODE"`
	HeatMode                bool     `json:"HEAT_MODE"`
	HeatOn                  bool     `json:"HEAT_ON"`
	HoldCool                float64  `json:"HOLD_COOL"`
	HoldOff                 bool     `json:"HOLD_OFF"`
	HoldOn                  bool     `json:"HOLD_ON"`
	HoldTemp                float64  `json:"HOLD_TEMP"`
	HoldTime                string   `json:"HOLD_TIME"`
	Holiday                 bool     `json:"HOLIDAY"`
	Lock                    bool     `json:"LOCK"`
	LowBattery              bool     `json:"LOW_BATTERY"`
	ManualOff               bool     `json:"MANUAL_OFF"`
	Modelock                bool     `json:"MODELOCK"`
	ModulationLevel         int      `json:"MODULATION_LEVEL"`
	Offline                 bool     `json:"OFFLINE"`
	PinNumber               string   `json:"PIN_NUMBER"`
	PreheatActive           bool     `json:"PREHEAT_ACTIVE"`
	PrgTemp                 float64  `json:"PRG_TEMP"`
	PrgTimer                bool     `json:"PRG_TIMER"`
	RecentTemps             []string `json:"RECENT_TEMPS"`
	RelativeHumidity        int      `json:"RELATIVE_HUMIDITY"`
	SetTemp                 string   `json:"SET_TEMP"`
	Standby                 bool     `json:"STANDBY"`
	SwitchDelayLeft         string   `json:"SWITCH_DELAY_LEFT"`
	TemporarySetFlag        bool     `json:"TEMPORARY_SET_FLAG"`
	Thermostat              bool     `json:"THERMOSTAT,omitempty"`
	Time                    string   `json:"TIME"`
	TimerOn                 bool     `json:"TIMER_ON"`
	WindowOpen              bool     `json:"WINDOW_OPEN"`
	WriteCount              int      `json:"WRITE_COUNT"`
	ZoneName                string   `json:"ZONE_NAME"`
	Timeclock               bool     `json:"TIMECLOCK,omitempty"`
}

type NeoHubLiveData struct {
	CloseDelay                    int            `json:"CLOSE_DELAY"`
	CoolInput                     bool           `json:"COOL_INPUT"`
	HolidayEnd                    int            `json:"HOLIDAY_END"`
	HubAway                       bool           `json:"HUB_AWAY"`
	HubHoliday                    bool           `json:"HUB_HOLIDAY"`
	HubTime                       int            `json:"HUB_TIME"`
	OpenDelay                     int            `json:"OPEN_DELAY"`
	TimestampDeviceLists          int            `json:"TIMESTAMP_DEVICE_LISTS"`
	TimestampEngineers            int            `json:"TIMESTAMP_ENGINEERS"`
	TimestampProfile0             int            `json:"TIMESTAMP_PROFILE_0"`
	TimestampProfileComfortLevels int            `json:"TIMESTAMP_PROFILE_COMFORT_LEVELS"`
	TimestampProfileTimers        int            `json:"TIMESTAMP_PROFILE_TIMERS"`
	TimestampProfileTimers0       int            `json:"TIMESTAMP_PROFILE_TIMERS_0"`
	TimestampRecipes              int            `json:"TIMESTAMP_RECIPES"`
	TimestampSystem               int            `json:"TIMESTAMP_SYSTEM"`
	Devices                       []NeoHubDevice `json:"devices"`
}

type NeoHubSystem struct {
	AltTimerFormat   interface{} `json:"ALT_TIMER_FORMAT"`
	Coolbox          string      `json:"COOLBOX"`
	CoolboxOverride  interface{} `json:"COOLBOX_OVERRIDE"`
	CoolboxPresent   int         `json:"COOLBOX_PRESENT"`
	Corf             string      `json:"CORF"`
	DeviceID         string      `json:"DEVICE_ID"`
	DstAuto          bool        `json:"DST_AUTO"`
	DstOn            bool        `json:"DST_ON"`
	ExtendedHistory  string      `json:"EXTENDED_HISTORY"`
	Format           int         `json:"FORMAT"`
	Gdevlist         []int       `json:"GDEVLIST"`
	GlobalHcMode     string      `json:"GLOBAL_HC_MODE"`
	GlobalSystemType string      `json:"GLOBAL_SYSTEM_TYPE"`
	HeatingLevels    int         `json:"HEATING_LEVELS"`
	HubType          int         `json:"HUB_TYPE"`
	HubVersion       int         `json:"HUB_VERSION"`
	LegacyLocalPort  bool        `json:"LEGACY_LOCAL_PORT"`
	NoRfBroadcast    bool        `json:"NO_RF_BROADCAST"`
	NtpOn            string      `json:"NTP_ON"`
	Partition        string      `json:"PARTITION"`
	Timestamp        int         `json:"TIMESTAMP"`
	Timezonestr      interface{} `json:"TIMEZONESTR"`
	TimeZone         float64     `json:"TIME_ZONE"`
	Utc              int         `json:"UTC"`
}

type NeoHubEngineers struct {
	CoolEnable            bool    `json:"COOL_ENABLE"`
	Deadband              int     `json:"DEADBAND"`
	DeviceID              int     `json:"DEVICE_ID"`
	DeviceType            int     `json:"DEVICE_TYPE"`
	DewPoint              bool    `json:"DEW_POINT"`
	FloorLimit            float64 `json:"FLOOR_LIMIT"`
	FrostTemp             float64 `json:"FROST_TEMP"`
	MaxPreheat            int     `json:"MAX_PREHEAT"`
	OutputDelay           int     `json:"OUTPUT_DELAY"`
	PumpDelay             int     `json:"PUMP_DELAY"`
	RfSensorMode          string  `json:"RF_SENSOR_MODE"`
	SensorMode            string  `json:"SENSOR_MODE"`
	StatFailsafe          int     `json:"STAT_FAILSAFE"`
	StatVersion           int     `json:"STAT_VERSION"`
	SwitchingDifferential float64 `json:"SWITCHING DIFFERENTIAL"`
	SwitchDelay           int     `json:"SWITCH_DELAY"`
	SystemType            int     `json:"SYSTEM_TYPE"`
	Timestamp             int     `json:"TIMESTAMP"`
	UltraVersion          int     `json:"ULTRA_VERSION"`
	UserLimit             int     `json:"USER_LIMIT"`
	WindowSwitchOpen      bool    `json:"WINDOW_SWITCH_OPEN"`
}

type NeoHubStatus struct {
	LiveData  NeoHubLiveData
	System    NeoHubSystem
	Engineers map[string]NeoHubEngineers
}

func readStatus(ctx context.Context, host string) (*NeoHubStatus, error) {
	var (
		closeOnce sync.Once
		d         net.Dialer
	)

	c, err := d.DialContext(ctx, "tcp", host)
	if err != nil {
		return nil, err
	}

	closeConn := func() {
		closeOnce.Do(func() {
			if err := c.Close(); err != nil {
				logger.Warn().Err(err).Msg("unable to close connection")
			}
		})
	}
	defer closeConn()

	// close connection when timeout is reached
	go func() {
		<-ctx.Done()
		closeConn()
	}()

	ctx.Done()

	var (
		scanner = bufio.NewScanner(c)
		status  NeoHubStatus
	)
	for _, part := range []struct {
		name string
		dest interface{}
	}{
		{"LIVE_DATA", &status.LiveData},
		{"SYSTEM", &status.System},
		{"ENGINEERS", &status.Engineers},
	} {

		logger.Debug().Str("host", host).Msgf("request %s data", part.name)
		if _, err := c.Write([]byte(fmt.Sprintf("{\"GET_%s\":0}\x00\r", part.name))); err != nil {
			return nil, fmt.Errorf("error requesting %s data: %w", part.name, err)
		}

		if !scanner.Scan() {
			return nil, fmt.Errorf("no response received for %s", part.name)
		}

		if err := json.Unmarshal(scanner.Bytes(), part.dest); err != nil {
			return nil, fmt.Errorf("error parsing %s: %w", part.name, err)
		}

		logger.Debug().Str("host", host).Interface("status", part.dest).Msgf("received %s data", part.name)

		// read the zero byte
		if _, err := c.Read([]byte{0x0}); err != nil {
			return nil, fmt.Errorf("error reading zero byte: %w", err)
		}
	}

	return &status, nil
}
