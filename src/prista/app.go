package prista

import (
	"fmt"
	"github.com/btnguyen2k/consu/reddo"
	"github.com/btnguyen2k/consu/semita"
	"github.com/btnguyen2k/singu"
	"github.com/btnguyen2k/singu/leveldb"
	"github.com/go-akka/configuration"
	"log"
	"main/src/logger"
	"main/src/utils"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var (
	AppConfig  *configuration.Config
	LogConfig  *configuration.Config
	LogWriters map[string]*logger.LogWriterAndInfo
	Buffer     singu.IQueue

	ConcurrentWrite int64 = 0
)

/*
Start bootstraps the application.
*/
func Start() {
	var err error
	AppConfig = initAppConfig()
	utils.Location, err = time.LoadLocation(AppConfig.GetString("timezone"))
	if err != nil {
		panic(err)
	}

	LogConfig = AppConfig.GetConfig("log")

	Buffer = initBuffer(LogConfig)

	LogWriters = initLogWriters(LogConfig)
	if LogWriters == nil {
		panic("no valid log writer configured")
	}
	if _, ok := LogWriters["default"]; !ok {
		panic("no valid log writer for 'default' category")
	}

	go goWriteLogs(Buffer)
	go goProcessOrphanLogs(Buffer)

	var wg sync.WaitGroup
	if initHttpServer(&wg) {
		wg.Add(1)
	}
	if initUdpServer(&wg) {
		wg.Add(1)
	}
	if initGrpcServer(&wg) {
		wg.Add(1)
	}
	wg.Wait()

	fmt.Printf("Application exists.")
}

const defaultConfigFile = "./config/application.conf"

func initAppConfig() *configuration.Config {
	configFile := os.Getenv("APP_CONFIG")
	if configFile == "" {
		log.Printf("No environment APP_CONFIG found, fallback to [%s]", defaultConfigFile)
		configFile = defaultConfigFile
	}
	return loadAppConfig(configFile)
}

func initBuffer(config *configuration.Config) singu.IQueue {
	tempDir := config.GetString("temp_dir", "./temp")
	return leveldb.NewLeveldbQueue("buffer", tempDir, 0, false, 0)
}

// Go routine to requeue orphan messages
func goProcessOrphanLogs(buffer singu.IQueue) {
	for {
		time.Sleep(11 * time.Second)
		if msgList, err := buffer.OrphanMessages(10, 1000); err != nil {
			log.Printf(fmt.Sprintf("ERROR: error fetchig orphan messages: %e", err))
		} else if len(msgList) > 0 {
			log.Printf(fmt.Sprintf("INFO: processing %d orphan messages...", len(msgList)))
			for _, msg := range msgList {
				if _, err := buffer.Requeue(msg.Id, false); err != nil {
					log.Printf(fmt.Sprintf("ERROR: error requeueing orphan message %s/%s: %e", msg.Id, string(msg.Payload), err))
				}
			}
		}
	}
}

// Go routine to fetch messages from buffer and send to log writer
func goWriteLogs(buffer singu.IQueue) {
	for {
		time.Sleep(1 * time.Second)
		var counterAll, counterSuccess int64 = 0, 0
		t1 := time.Now()
		for msg, err := buffer.Take(); err == nil && msg != nil; msg, err = buffer.Take() {
			counterAll++
			tokens := strings.Split(string(msg.Payload), logger.SeparatorTsv)
			var finish = true
			if len(tokens) == 2 {
				logWriter := LogWriters[tokens[0]]
				if logWriter == nil {
					logWriter = LogWriters["default"]
				}
				if logWriter == nil {
					log.Printf(fmt.Sprintf("WARM: no log writer found for category [%s]", tokens[0]))
				} else if err := logWriter.LogWriter.Write(tokens[0], tokens[1]); err != nil {
					log.Printf(fmt.Sprintf("ERROR: error writing log to [%s]: %e", tokens[0], err))
					if logWriter.RetrySeconds < 0 || msg.Timestamp.Unix()+logWriter.RetrySeconds >= time.Now().Unix() {
						// set finish=false to requeue if message has not been queued for 'RetrySeconds'
						finish = false
					}
				} else {
					counterSuccess++
				}
			}
			if finish {
				if err := buffer.Finish(msg.Id); err != nil {
					log.Printf(fmt.Sprintf("ERROR: error finishing message %s/%s: %e", msg.Id, string(msg.Payload), err))
				}
			} else if _, err := buffer.Requeue(msg.Id, false); err != nil {
				log.Printf(fmt.Sprintf("ERROR: error requeueing message %s/%s: %e", msg.Id, string(msg.Payload), err))
			}
			if ConcurrentWrite > 0 && counterAll >= 100/(ConcurrentWrite+1) || time.Now().Unix()-t1.Unix() >= 10 {
				// throttle [buffer->log-writer] rate
				break
			}
		}
		if counterSuccess > 0 {
			log.Printf(fmt.Sprintf("INFO: %d log(s) written", counterSuccess))
		}
	}
}

func initLogWriters(config *configuration.Config) map[string]*logger.LogWriterAndInfo {
	if config != nil && config.Root().IsObject() {
		result := make(map[string]*logger.LogWriterAndInfo)
		for cat, conf := range config.Root().GetObject().Items() {
			if conf != nil && conf.IsObject() {
				cat = strings.ToLower(cat)
				if writer, err := logger.NewLogWriter(cat, conf.GetObject().Unwrapped()); err != nil {
					panic(err)
				} else {
					lwi := logger.LogWriterAndInfo{LogWriter: writer}
					wInfo := writer.Info()
					info := semita.NewSemita(wInfo)
					retrySeconds, err := info.GetValueOfType(logger.ConfRetrySeconds, reddo.TypeInt)
					if err != nil || retrySeconds == nil {
						retrySeconds = logger.DefaultRetrySeconds
					}
					lwi.RetrySeconds = retrySeconds.(int64)
					result[cat] = &lwi
				}
			} else {
				panic(fmt.Sprintf("invalid config for log writer [%s]: %v", cat, conf))
			}
		}
		return result
	}
	return nil
}

// convenient function to handle incoming message
func handleIncomingMessage(payload []byte) error {
	// increase concurrency count to throttle [buffer->log-writer] rate
	atomic.AddInt64(&ConcurrentWrite, 1)
	defer atomic.AddInt64(&ConcurrentWrite, -1)

	_, err := Buffer.Queue(singu.NewQueueMessage(payload))
	return err
}
