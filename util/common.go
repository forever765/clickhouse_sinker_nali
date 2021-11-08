/*Copyright [2019] housepower

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

   http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package util

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io/ioutil"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"gopkg.in/natefinch/lumberjack.v2"

	"github.com/fagongzi/goetty"
	"github.com/pkg/errors"
)

var (
	GlobalTimerWheel  *goetty.TimeoutWheel //the global timer wheel
	GlobalParsingPool *WorkerPool          //for all tasks' parsing, cpu intensive
	GlobalWritingPool *WorkerPool          //the all tasks' writing ClickHouse, cpu-net balance
	Logger            *zap.Logger
	logAtomLevel      zap.AtomicLevel
	logPaths          []string
)

// InitGlobalTimerWheel initialize the global timer wheel
func InitGlobalTimerWheel() {
	if GlobalTimerWheel != nil {
		return
	}
	GlobalTimerWheel = goetty.NewTimeoutWheel(goetty.WithTickInterval(time.Second))
}

// InitGlobalParsingPool initialize GlobalParsingPool
func InitGlobalParsingPool() {
	if GlobalParsingPool != nil {
		return
	}
	maxWorkers := 10
	if runtime.NumCPU() >= 2 {
		if maxWorkers > runtime.NumCPU()/2 {
			maxWorkers = runtime.NumCPU() / 2
		}
	} else {
		maxWorkers = 1
	}
	queueSize := 1 << 16
	GlobalParsingPool = NewWorkerPool(maxWorkers, queueSize)
	Logger.Info("initialized parsing pool", zap.Int("maxWorkers", maxWorkers), zap.Int("queueSize", queueSize))
}

// InitGlobalWritingPool initialize GlobalWritingPool
func InitGlobalWritingPool(maxWorkers int) {
	if GlobalWritingPool != nil {
		return
	}
	queueSize := 3
	GlobalWritingPool = NewWorkerPool(maxWorkers, queueSize)
	Logger.Info("initialized writing pool", zap.Int("maxWorkers", maxWorkers), zap.Int("queueSize", queueSize))
}

// StringContains check if contains string in array
func StringContains(arr []string, str string) bool {
	for _, s := range arr {
		if s == str {
			return true
		}
	}
	return false
}

// GetSourceName returns the field name in message for the given ClickHouse column
func GetSourceName(name string) (sourcename string) {
	sourcename = strings.Replace(name, ".", "\\.", -1)
	return
}

// GetShift returns the smallest `shift` which 1<<shift is no smaller than s
func GetShift(s int) (shift uint) {
	for shift = 0; (1 << shift) < s; shift++ {
	}
	return
}

// GetOutboundIP get preferred outbound ip of this machine
//https://stackoverflow.com/questions/23558425/how-do-i-get-the-local-ip-address-in-go
func GetOutboundIP() (ip net.IP, err error) {
	var conn net.Conn
	if conn, err = net.Dial("udp", "8.8.8.8:80"); err != nil {
		err = errors.Wrapf(err, "")
		return
	}
	defer conn.Close()
	localAddr, _ := conn.LocalAddr().(*net.UDPAddr)
	ip = localAddr.IP
	return
}

// GetSpareTCPPort find a spare TCP port
func GetSpareTCPPort(portBegin int) (port int) {
LOOP:
	for port = portBegin; ; port++ {
		addr := fmt.Sprintf(":%d", port)
		ln, err := net.Listen("tcp", addr)
		if err == nil {
			ln.Close()
			break LOOP
		}
	}
	return
}

// https://stackoverflow.com/questions/50428176/how-to-get-ip-and-port-from-net-addr-when-it-could-be-a-net-udpaddr-or-net-tcpad
func GetNetAddrPort(addr net.Addr) (port int) {
	switch addr := addr.(type) {
	case *net.UDPAddr:
		port = addr.Port
	case *net.TCPAddr:
		port = addr.Port
	}
	return
}

// Refers to:
// https://medium.com/processone/using-tls-authentication-for-your-go-kafka-client-3c5841f2a625
// https://github.com/denji/golang-tls
// https://www.baeldung.com/java-keystore-truststore-difference
func NewTLSConfig(caCertFiles, clientCertFile, clientKeyFile string, insecureSkipVerify bool) (*tls.Config, error) {
	tlsConfig := tls.Config{}
	// Load client cert
	if clientCertFile != "" && clientKeyFile != "" {
		cert, err := tls.LoadX509KeyPair(clientCertFile, clientKeyFile)
		if err != nil {
			err = errors.Wrapf(err, "")
			return &tlsConfig, err
		}
		tlsConfig.Certificates = []tls.Certificate{cert}
	}

	// Load CA cert
	caCertPool := x509.NewCertPool()
	for _, caCertFile := range strings.Split(caCertFiles, ",") {
		caCert, err := ioutil.ReadFile(caCertFile)
		if err != nil {
			err = errors.Wrapf(err, "")
			return &tlsConfig, err
		}
		caCertPool.AppendCertsFromPEM(caCert)
	}
	tlsConfig.RootCAs = caCertPool
	tlsConfig.InsecureSkipVerify = insecureSkipVerify
	return &tlsConfig, nil
}

func EnvStringVar(value *string, key string) {
	realKey := strings.ReplaceAll(strings.ToUpper(key), "-", "_")
	val, found := os.LookupEnv(realKey)
	if found {
		*value = val
	}
}

func EnvIntVar(value *int, key string) {
	realKey := strings.ReplaceAll(strings.ToUpper(key), "-", "_")
	val, found := os.LookupEnv(realKey)
	if found {
		valInt, err := strconv.Atoi(val)
		if err == nil {
			*value = valInt
		}
	}
}

func EnvBoolVar(value *bool, key string) {
	realKey := strings.ReplaceAll(strings.ToUpper(key), "-", "_")
	if _, found := os.LookupEnv(realKey); found {
		*value = true
	}
}

// JksToPem converts JKS to PEM
// Refers to:
// https://serverfault.com/questions/715827/how-to-generate-key-and-crt-file-from-jks-file-for-httpd-apache-server
func JksToPem(jksPath, jksPassword string, overwrite bool) (certPemPath, keyPemPath string, err error) {
	dir, fn := filepath.Split(jksPath)
	certPemPath = filepath.Join(dir, fn+".cert.pem")
	keyPemPath = filepath.Join(dir, fn+".key.pem")
	pkcs12Path := filepath.Join(dir, fn+".p12")
	if overwrite {
		for _, fp := range []string{certPemPath, keyPemPath, pkcs12Path} {
			if err = os.RemoveAll(fp); err != nil {
				err = errors.Wrapf(err, "")
				return
			}
		}
	} else {
		for _, fp := range []string{certPemPath, keyPemPath, pkcs12Path} {
			if _, err = os.Stat(fp); err == nil {
				return
			}
		}
	}
	cmds := [][]string{
		{"keytool", "-importkeystore", "-srckeystore", jksPath, "-destkeystore", pkcs12Path, "-deststoretype", "PKCS12"},
		{"openssl", "pkcs12", "-in", pkcs12Path, "-nokeys", "-out", certPemPath, "-passin", "env:password"},
		{"openssl", "pkcs12", "-in", pkcs12Path, "-nodes", "-nocerts", "-out", keyPemPath, "-passin", "env:password"},
	}
	for _, cmd := range cmds {
		Logger.Info(strings.Join(cmd, " "))
		exe := exec.Command(cmd[0], cmd[1:]...)
		if cmd[0] == "keytool" {
			exe.Stdin = bytes.NewReader([]byte(jksPassword + "\n" + jksPassword + "\n" + jksPassword))
		} else if cmd[0] == "openssl" {
			exe.Env = []string{fmt.Sprintf("password=%s", jksPassword)}
		}
		var out []byte
		out, err = exe.CombinedOutput()
		Logger.Info(string(out))
		if err != nil {
			err = errors.Wrapf(err, "")
			return
		}
	}
	return
}

func InitLogger(newLogPaths []string) {
	if reflect.DeepEqual(logPaths, newLogPaths) {
		return
	}
	logAtomLevel = zap.NewAtomicLevel()
	logPaths = newLogPaths
	var syncers []zapcore.WriteSyncer
	for _, p := range logPaths {
		switch p {
		case "stdout":
			syncers = append(syncers, zapcore.AddSync(os.Stdout))
		case "stderr":
			syncers = append(syncers, zapcore.AddSync(os.Stderr))
		default:
			writeFile := zapcore.AddSync(&lumberjack.Logger{
				Filename:   p,
				MaxSize:    100, // megabytes
				MaxBackups: 10,
				LocalTime:  true,
			})
			syncers = append(syncers, writeFile)
		}
	}

	cfg := zap.NewProductionEncoderConfig()
	cfg.EncodeTime = zapcore.ISO8601TimeEncoder
	core := zapcore.NewCore(
		zapcore.NewJSONEncoder(cfg),
		zapcore.NewMultiWriteSyncer(syncers...),
		logAtomLevel,
	)
	Logger = zap.New(core, zap.AddStacktrace(zap.ErrorLevel))
}

func SetLogLevel(newLogLevel string) {
	if Logger != nil {
		var lvl zapcore.Level
		if err := lvl.Set(newLogLevel); err != nil {
			lvl = zap.InfoLevel
		}
		logAtomLevel.SetLevel(lvl)
	}
}
