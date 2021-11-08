package main

/*
CREATE TABLE apache_access_log ON CLUSTER abc (
	`@collectiontime` DateTime,
	`@hostname` LowCardinality(String),
	`@ip` LowCardinality(String),
	`@path` String,
	`@lineno` Int64,
	`@message` String,
	agent String,
	auth String,
	bytes Int64,
	clientIp String,
	device_family LowCardinality(String),
	httpversion LowCardinality(String),
	ident String,
	os_family LowCardinality(String),
	os_major LowCardinality(String),
	os_minor LowCardinality(String),
	referrer String,
	request String,
	requesttime Float64,
	response LowCardinality(String),
	timestamp DateTime64(3),
	userAgent_family LowCardinality(String),
	userAgent_major LowCardinality(String),
	userAgent_minor LowCardinality(String),
	verb LowCardinality(String),
	xforwardfor LowCardinality(String)
) ENGINE=ReplicatedMergeTree('/clickhouse/tables/{cluster}/{database}/{table}/{shard}', '{replica}')
PARTITION BY toYYYYMMDD(timestamp)
ORDER BY (timestamp, `@hostname`, `@path`, `@lineno`);

CREATE TABLE dist_apache_access_log ON CLUSTER abc AS apache_access_log ENGINE = Distributed(abc, default, apache_access_log);

*/

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/Shopify/sarama"
	"github.com/bytedance/sonic"
	"github.com/google/gops/agent"
	"github.com/housepower/clickhouse_sinker/util"
	"github.com/pkg/errors"
	"go.uber.org/zap"
)

var (
	KafkaBrokers   string
	KafkaTopic     string
	LogfileDir     string
	LogfilePattern string

	ListHostname = []string{"vm101101", "vm101102", "vm101103", "vm101104", "vm101105", "vm101106", "vm101107", "vm101108", "vm101109", "vm101110"}
	ListIP       = []string{"192.168.101.101",
		"192.168.101.102",
		"192.168.101.103",
		"192.168.101.104",
		"192.168.101.105",
		"192.168.101.106",
		"192.168.101.107",
		"192.168.101.108",
		"192.168.101.109",
		"192.168.101.110"}
	ListAgent           = []string{"Mozilla/5.0(Windows NT 6.1; Win64; x64)AppleWebKit/537.36(KHTML,like Gecko)Chrome/69.0.3497.100Safari/537.36"}
	ListAuth            = []string{"RFC1413身份"}
	ListClientIP        = []string{"192.168.1.1", "192.168.1.2", "192.168.1.3", "192.168.1.4", "192.168.1.5"}
	ListDeviceFamily    = []string{"Hawei", "Xiaomi", "OPPO", "Apple", "Other"}
	ListHttpversion     = []string{"1.0", "1.1", "2.0", "3.0"}
	ListOsFamily        = []string{"Android", "Mac OS X", "HMS"}
	ListOsMajor         = []string{"6", "7", "8", "9", "10"}
	ListOsMinor         = []string{"0", "1", "2", "3"}
	ListResponse        = []string{"200", "301", "400", "404", "503"}
	ListUserAgentFamily = []string{"Chrome", "Firefox", "AppleWebKit"}
	ListUserAgentMajor  = []string{"75", "76", "77", "78", "79", "80", "81"}
	ListUserAgentMinor  = []string{"0", "1", "2", "3"}
	ListVerb            = []string{"GET", "POST", "HEAD"}
)

// generated by https://mholt.github.io/json-to-go/, https://transform.tools/json-to-go
type Log struct {
	Collectiontime  time.Time `json:"@collectiontime"`
	Hostname        string    `json:"@hostname"`
	IP              string    `json:"@ip"`
	Path            string    `json:"@path"`
	LineNo          int       `json:"@lineno"`
	Message         string    `json:"@message"`
	Agent           string    `json:"agent"`
	Auth            string    `json:"auth"`
	Bytes           int       `json:"bytes"`
	ClientIP        string    `json:"clientIp"`
	DeviceFamily    string    `json:"device_family"`
	Httpversion     string    `json:"httpversion"`
	Ident           string    `json:"ident"`
	OsFamily        string    `json:"os_family"`
	OsMajor         string    `json:"os_major"`
	OsMinor         string    `json:"os_minor"`
	Referrer        string    `json:"referrer"`
	Request         string    `json:"request"`
	Requesttime     int       `json:"requesttime"`
	Response        string    `json:"response"`
	Timestamp       time.Time `json:"timestamp"`
	UserAgentFamily string    `json:"userAgent_family"`
	UserAgentMajor  string    `json:"userAgent_major"`
	UserAgentMinor  string    `json:"userAgent_minor"`
	Verb            string    `json:"verb"`
	Xforwardfor     string    `json:"xforwardfor"`
}

func randElement(list []string) string {
	off := rand.Intn(len(list))
	return list[off]
}

type LogGenerator struct {
	logfiles []string
	off      int
	fp       string
	lineno   int
	reader   *os.File
	scanner  *bufio.Scanner
	lines    int64
	size     int64
}

func (g *LogGenerator) Stat() (l, s int64) {
	l = atomic.LoadInt64(&g.lines)
	s = atomic.LoadInt64(&g.size)
	return
}

//reset logfiles
func (g *LogGenerator) Init() error {
	g.logfiles = nil
	g.off = -1
	g.fp = ""
	g.lineno = 0
	fnPatt := regexp.MustCompile(LogfilePattern)
	d, err := os.Open(LogfileDir)
	defer func() {
		d.Close()
	}()
	if err != nil {
		err = errors.Wrapf(err, "")
		return err
	}
	fis, err := d.Readdir(0)
	if err != nil {
		err = errors.Wrapf(err, "")
		return err
	}
	for _, fi := range fis {
		if !fi.IsDir() && fnPatt.MatchString(fi.Name()) {
			fp, err := filepath.Abs(filepath.Join(LogfileDir, fi.Name()))
			if err != nil {
				err = errors.Wrapf(err, "")
				return err
			}
			g.logfiles = append(g.logfiles, fp)
		}
	}
	if g.logfiles == nil || len(g.logfiles) == 0 {
		err := errors.Errorf("There is no files under %v match pattern %v", LogfileDir, LogfilePattern)
		return err
	}
	sort.Strings(g.logfiles)
	util.Logger.Info(fmt.Sprintf("Following files under %v match pattern %v: %+v", LogfileDir, LogfilePattern, g.logfiles))

	if err := g.next(); err != nil {
		return err
	}
	return nil
}

//switch to next log file
func (g *LogGenerator) next() (err error) {
	g.scanner = nil
	if g.reader != nil {
		g.reader.Close()
		g.reader = nil
	}
	g.lineno = 0
	for i := 0; i < len(g.logfiles); i++ {
		// a log file may disappear, retry another log file
		g.off = (g.off + 1) % len(g.logfiles)
		g.fp = g.logfiles[g.off]
		var reader *os.File
		if reader, err = os.Open(g.fp); err == nil {
			g.reader = reader
			g.scanner = bufio.NewScanner(g.reader)
			util.Logger.Debug(fmt.Sprintf("scanning %+v", g.fp))
			return nil
		}
		err = errors.Wrapf(err, "")
		util.Logger.Fatal("os.Open failed", zap.String("path", g.fp), zap.Error(err))
		time.Sleep(6000 * time.Second)
	}
	err = errors.Errorf("no readable file")
	return
}

func (g *LogGenerator) getLine() (fp string, lineno int, line string) {
	for {
		if g.scanner.Scan() {
			g.lineno++
			return g.fp, g.lineno, g.scanner.Text()
		}
		if err := g.scanner.Err(); err != nil {
			util.Logger.Fatal("got error", zap.Error(err))
		}
		if err := g.next(); err != nil {
			util.Logger.Fatal("got error", zap.Error(err))
		}
	}
}

func (g *LogGenerator) Run() {
	toRound := time.Now()
	// refers to time.Time.Truncate
	rounded := time.Date(toRound.Year(), toRound.Month(), toRound.Day(), 0, 0, 0, 0, toRound.Location())

	wp := util.NewWorkerPool(10, 10000)
	config := sarama.NewConfig()
	config.Version = sarama.V2_1_0_0
	w, err := sarama.NewAsyncProducer(strings.Split(KafkaBrokers, ","), config)
	if err != nil {
		util.Logger.Fatal("sarama.NewAsyncProducer failed", zap.Error(err))
	}
	defer w.Close()
	chInput := w.Input()

	var b []byte
	for day := 0; ; day++ {
		tsDay := rounded.Add(time.Duration(-24*day) * time.Hour)
		for step := 0; step < 24*60*60*1000; step++ {
			timestamp := tsDay.Add(time.Duration(step) * time.Millisecond)
			fp, lineno, line := g.getLine()
			logObj := Log{
				Collectiontime:  timestamp,
				Hostname:        randElement(ListHostname),
				IP:              randElement(ListIP),
				Path:            fp,
				LineNo:          lineno,
				Message:         line,
				Agent:           randElement(ListAgent),
				Auth:            randElement(ListAuth),
				Bytes:           len(line),
				ClientIP:        randElement(ListClientIP),
				DeviceFamily:    randElement(ListDeviceFamily),
				Httpversion:     randElement(ListHttpversion),
				Ident:           "",
				OsFamily:        randElement(ListOsFamily),
				OsMajor:         randElement(ListOsMajor),
				OsMinor:         randElement(ListOsMinor),
				Referrer:        "",
				Request:         "",
				Requesttime:     rand.Intn(1000),
				Response:        randElement(ListResponse),
				Timestamp:       timestamp,
				UserAgentFamily: randElement(ListUserAgentFamily),
				UserAgentMajor:  randElement(ListUserAgentMajor),
				UserAgentMinor:  randElement(ListUserAgentMinor),
				Verb:            randElement(ListVerb),
				Xforwardfor:     "",
			}
			_ = wp.Submit(func() {
				if b, err = sonic.Marshal(&logObj); err != nil {
					err = errors.Wrapf(err, "")
					util.Logger.Fatal("got error", zap.Error(err))
				}
				chInput <- &sarama.ProducerMessage{
					Topic: KafkaTopic,
					Key:   sarama.StringEncoder(logObj.Hostname),
					Value: sarama.ByteEncoder(b),
				}
				atomic.AddInt64(&g.lines, int64(1))
				atomic.AddInt64(&g.size, int64(len(b)))
			})
		}
	}
}

func main() {
	util.InitLogger([]string{"stdout"})
	flag.Usage = func() {
		usage := fmt.Sprintf(`Usage of %s
    %s kakfa_brokers topic log_file_dir log_file_pattern
This util read log from given paths, fill some fields with random content, serialize and send to kafka.
kakfa_brokers: for example, 192.168.102.114:9092,192.168.102.115:9092
topic: for example, apache_access_log
log_file_dir: log file directory, for example, /var/log
log_file_pattern: file name pattern, for example, '^secure.*$'`, os.Args[0], os.Args[0])
		util.Logger.Info(usage)
		os.Exit(0)
	}
	flag.Parse()
	args := flag.Args()
	if len(args) != 4 {
		flag.Usage()
	}
	KafkaBrokers = args[0]
	KafkaTopic = args[1]
	LogfileDir = args[2]
	LogfilePattern = args[3]
	util.Logger.Info("CLI options",
		zap.String("KafkaBrokers", KafkaBrokers),
		zap.String("KafkaTopic", KafkaTopic),
		zap.String("LogfileDir", LogfileDir),
		zap.String("LogFilePattern", LogfilePattern))

	if err := agent.Listen(agent.Options{}); err != nil {
		util.Logger.Fatal("got error", zap.Error(err))
	}

	ctx, _ := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	g := &LogGenerator{}
	if err := g.Init(); err != nil {
		util.Logger.Fatal("got error", zap.Error(err))
	}
	go g.Run()

	var prevLines, prevSize int64
	ticker := time.NewTicker(10 * time.Second)
LOOP:
	for {
		select {
		case <-ctx.Done():
			util.Logger.Info("quit due to context been canceled")
			break LOOP
		case <-ticker.C:
			var speedLine, speedSize int64
			lines, size := g.Stat()
			if lines != 0 {
				speedLine = (lines - prevLines) / int64(10)
				speedSize = (size - prevSize) / int64(10)
			}
			prevLines = lines
			prevSize = size
			util.Logger.Info("status", zap.Int64("lines", lines), zap.Int64("bytes", size), zap.Int64("speed(lines/s)", speedLine), zap.Int64("speed(bytes/s)", speedSize))
		}
	}
}
