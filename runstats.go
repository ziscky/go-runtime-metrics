package runstats

import (
	"log"
	"os"
	"time"

	"fmt"

	"github.com/influxdata/influxdb/client/v2"
	"github.com/pkg/errors"
	"github.com/tevjef/go-runtime-metrics/collector"
)

const (
	defaultHost               = "localhost:8086"
	defaultMeasurement        = "go.runtime"
	defaultDatabase           = "stats"
	defaultCollectionInterval = 10 * time.Second
	defaultBatchInterval      = 60 * time.Second
)

// A configuration with default values.
var DefaultConfig = &Config{}

type Config struct {
	// InfluxDb host:port pair.
	// Default is "localhost:8086".
	Host string

	// Database to write points to.
	// Default is "stats" and is auto created
	Database string

	// Username with privileges on provided database.
	Username string

	// Password for provided user.
	Password string

	// Measurement to write points to.
	// Default is "go.runtime.<hostname>".
	Measurement string

	// Measurement to write points to.
	RetentionPolicy string

	// Interval at which to write batched points to InfluxDB.
	// Default is 60 seconds
	BatchInterval time.Duration

	// Precision in time to write your points in.
	// Default is nanoseconds
	Precision string

	// Interval at which to collect points.
	// Default is 10 seconds
	CollectionInterval time.Duration

	// DisableCPU collecting CPU Statistics. cpu.*
	// Default is false
	DisableCPU bool

	// Disable collecting Memory Statistics. mem.*
	DisableMem bool

	// Disable collecting GC Statistics (requires Memory be not be disabled). mem.gc.*
	DisableGc bool

	//CreateDatabase specifies whether to attempt database creation
	//errors returned if database already exists
	CreateDatabase bool

	// Default is DefaultLogger which exits when the library encounters a fatal error.
	Logger Logger
}

func (config *Config) init() (*Config, error) {
	if config == nil {
		config = DefaultConfig
	}

	if config.Database == "" {
		config.Database = defaultDatabase
	}

	if config.Host == "" {
		config.Host = defaultHost
	}

	if config.Measurement == "" {
		config.Measurement = defaultMeasurement

		if hn, err := os.Hostname(); err != nil {
			config.Measurement += ".unknown"
		} else {
			config.Measurement += "." + hn
		}
	}

	if config.CollectionInterval == 0 {
		config.CollectionInterval = defaultCollectionInterval
	}

	if config.BatchInterval == 0 {
		config.BatchInterval = defaultBatchInterval
	}

	if config.Logger == nil {
		config.Logger = &DefaultLogger{}
	}

	return config, nil
}

func RunCollector(config *Config, stop chan struct{}) (err error) {
	if config, err = config.init(); err != nil {
		return err
	}

	// Make client
	clnt, err := client.NewHTTPClient(client.HTTPConfig{
		Addr:     "http://" + config.Host,
		Username: config.Username,
		Password: config.Password,
	})

	if err != nil {
		return errors.Wrap(err, "failed to create influxdb client")
	}

	// Ping InfluxDB to ensure there is a connection
	if _, _, err := clnt.Ping(5 * time.Second); err != nil {
		return errors.Wrap(err, "failed to ping influxdb client")
	}

	if config.CreateDatabase {
		// Auto create database
		_, err = queryDB(clnt, fmt.Sprintf("CREATE DATABASE \"%s\"", config.Database))
		if err != nil {
			config.Logger.Fatalln(err)
			return err
		}
	}

	_runStats := &runStats{
		logger: config.Logger,
		client: clnt,
		config: config,
		pc:     make(chan *client.Point),
	}

	bp, err := _runStats.newBatch()

	if err != nil {
		return err
	}

	_runStats.points = bp

	loopStop := make(chan struct{}, 1)
	collectorStop := make(chan struct{}, 1)

	//Receive stop signal from stop channel and propagate to loop and collector
	go func(stopAll <-chan struct{}, loopStop, collectorStop chan<- struct{}) {
		select {
		case <-stop:
			loopStop <- struct{}{}
			collectorStop <- struct{}{}
		}
	}(stop, loopStop, collectorStop)

	go _runStats.loop(config.BatchInterval, loopStop)

	_collector := collector.New(_runStats.onNewPoint)
	_collector.PauseDur = config.CollectionInterval
	_collector.EnableCPU = !config.DisableCPU
	_collector.EnableMem = !config.DisableMem
	_collector.EnableGC = !config.DisableGc
	_collector.Done = collectorStop

	go _collector.Run()

	return nil
}

type runStats struct {
	logger Logger
	client client.Client
	points client.BatchPoints
	config *Config
	pc     chan *client.Point
}

func (r *runStats) onNewPoint(fields collector.Fields) {
	pt, err := client.NewPoint(r.config.Measurement, fields.Tags(), fields.Values(), time.Now())

	if err != nil {
		r.logger.Fatalln(errors.Wrap(err, "error while creating point"))
	}

	r.pc <- pt
}

func (r *runStats) newBatch() (bp client.BatchPoints, err error) {
	bp, err = client.NewBatchPoints(client.BatchPointsConfig{
		Database:        r.config.Database,
		Precision:       r.config.Precision,
		RetentionPolicy: r.config.RetentionPolicy,
	})

	if err != nil {
		r.logger.Fatalln(errors.Wrap(err, "could not create BatchPoints"))
	}

	return
}

// Write collected points to influxdb periodically
func (r *runStats) loop(interval time.Duration, stop <-chan struct{}) {
	ticks := time.Tick(interval)

	for {
		select {
		case <-ticks:
			if r.points == nil || len(r.points.Points()) <= 0 {
				continue
			}

			if err := r.client.Write(r.points); err != nil {
				r.logger.Errorln(errors.Wrap(err, "could not write points to InfluxDB"))
				continue
			}

			r.points = nil

			bp, err := r.newBatch()

			if err != nil {
				r.logger.Errorln(errors.Wrap(err, "could not create BatchPoints"))
				continue
			}

			r.points = bp

		case pt := <-r.pc:
			if r.points != nil {
				r.logger.Println(pt.String())

				r.points.AddPoint(pt)
			}
		case <-stop:
			return

		}
	}
}

type Logger interface {
	Println(v ...interface{})
	Fatalln(v ...interface{})
	Errorln(v ...interface{})
}

type DefaultLogger struct {
	//Panic when true defaults default strange behaiour on panicking on small errors
	//default returns errors silently
	Panic bool
}

func (*DefaultLogger) Println(v ...interface{}) {}
func (d *DefaultLogger) Fatalln(v ...interface{}) {
	if d.Panic {
		log.Fatalln(v)
	}
}
func (d *DefaultLogger) Errorln(v ...interface{}) {
	log.Println(v)
}

func queryDB(clnt client.Client, cmd string) (res []client.Result, err error) {
	q := client.Query{
		Command: cmd,
	}
	if response, err := clnt.Query(q); err == nil {
		if response.Error() != nil {
			return res, response.Error()
		}
		res = response.Results
	} else {
		return res, err
	}
	return res, nil
}
