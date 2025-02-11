package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	log "github.com/charmbracelet/log"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/tchaudhry91/algoprom/actions"
	"github.com/tchaudhry91/algoprom/algochecks"
	"github.com/tchaudhry91/algoprom/measure"
	"github.com/tchaudhry91/algoprom/store"
)

var header = `
	
 ____  _     _____ ____  ____  ____  ____  _            ____  _____ _____ _      _____ 
/  _ \/ \   /  __//  _ \/  __\/  __\/  _ \/ \__/|      /  _ \/  __//  __// \  /|/__ __\
| / \|| |   | |  _| / \||  \/||  \/|| / \|| |\/||_____ | / \|| |  _|  \  | |\ ||  / \  
| |-||| |_/\| |_//| \_/||  __/|    /| \_/|| |  ||\____\| |-||| |_//|  /_ | | \||  | |  
\_/ \|\____/\____\\____/\_/   \_/\_\\____/\_/  \|      \_/ \|\____\\____\\_/  \|  \_/  
                                                                                       
                                                                                       

`

func main() {
	logger := log.Default()
	logger.SetPrefix("algoprom")
	var configF = flag.String("c", "algoprom.json", "config file to use")
	flag.Parse()

	config := Config{}
	confData, err := os.ReadFile(*configF)
	if err != nil {
		logger.Fatalf("Error Reading Config File: %v", err)
	}
	if err = json.Unmarshal(confData, &config); err != nil {
		logger.Fatalf("Unable to Unmarshal Config: %v", err)
	}
	fmt.Print(header)
	run(&config, logger)
}

var contexts = map[string]*context.CancelFunc{}

func run(conf *Config, logger *log.Logger) {
	tickers := make([]*time.Ticker, 0, 1)
	done := make(chan bool)
	shutdown := make(chan error, 1)
	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt, syscall.SIGTERM)

	addr := conf.MetricsListenAddr
	if addr == "" {
		addr = "127.0.0.1:9967"
	}

	s, err := store.NewBoltStore(conf.DatabaseFile, logger)
	if err != nil {
		logger.Fatalf("Could not open database:%v", err)
	}

	go func(addr string) {
		logger.Infof("Starting Metrics Server on: %s", addr)
		http.Handle("/metrics", promhttp.Handler())
		http.ListenAndServe(addr, nil)
	}(addr)

	for _, c := range conf.Checks {
		ticker := time.NewTicker(c.Interval.Duration)
		tickers = append(tickers, ticker)
		logger.Infof("Starting Check: %s with interval:%s", c.Name, c.Interval.Duration)
		go func(c *algochecks.Check, logger *log.Logger) {
			if c.Immediate {
				err := runCheck(c, conf, logger, s)
				if err != nil {
					logger.Errorf("Error: %v", err)
				}
			}
			// Add a little bit of random starting delay to stagger checks
			staggerSeconds := rand.Intn(int(c.Interval.Duration.Seconds()))
			logger.Infof("Adding Initial Stagger of %d seconds", staggerSeconds)
			time.Sleep(time.Duration(staggerSeconds) * time.Second)
			for {
				select {
				case <-done:
					return
				case <-ticker.C:
					err := runCheck(c, conf, logger, s)
					if err != nil {
						logger.Errorf("Error: %v", err)
					}
				}
			}
		}(&c, logger.WithPrefix(c.Name))
	}

	select {
	case signalKill := <-interrupt:
		logger.Infof("Received Interrupt:%s", signalKill)
		for name, cancel := range contexts {
			logger.Infof("Cancelling:%s", name)
			(*cancel)()
		}
		for _, ticker := range tickers {
			ticker.Stop()
		}
	case err := <-shutdown:
		logger.Errorf("Error:%v", err)
	}

}

func getAlgorithmer(c *algochecks.Check, conf *Config, logger *log.Logger) algochecks.Algorithmer {
	var algorithmer algochecks.Algorithmer
	for _, aa := range conf.Algorithmers {
		if aa.Type == c.AlgorithmerType {
			algorithmer = algochecks.Build(aa, logger)
		}
	}
	return algorithmer
}

func getActioner(a *actions.ActionMeta, conf *Config, logger *log.Logger) actions.Actioner {
	var actioner actions.Actioner
	for _, aa := range conf.Actioners {
		if aa.Type == a.Actioner {
			actioner = actions.Build(aa, logger)
		}
	}
	return actioner
}

func runCheck(c *algochecks.Check, conf *Config, logger *log.Logger, s *store.BoltStore) error {

	algorithmer := getAlgorithmer(c, conf, logger)
	if algorithmer == nil {
		return fmt.Errorf("AlgorithmerType:%s not found", c.AlgorithmerType)
	}

	processed := countProcessed.WithLabelValues(c.Name)
	succeeded := countSuccess.WithLabelValues(c.Name)
	failed := countFail.WithLabelValues(c.Name)
	defer processed.Inc()

	tempWorkDir, err := os.MkdirTemp(conf.BaseWorkingDir, c.Name+"-")
	if err != nil {
		failed.Inc()
		return fmt.Errorf("Unable to create Temp Dir: %v", err)
	}
	defer os.RemoveAll(tempWorkDir)

	ctx := context.Background()
	ctx, cancel := context.WithCancel(ctx)
	contexts[c.Name] = &cancel

	// Fetch inputs
	inputs := map[string]measure.Result{}
	for _, i := range c.Inputs {
		d := fetchDatasourceByName(conf, i.Datasource)
		if d == nil {
			failed.Inc()
			return fmt.Errorf("Datasource Not Found: %s", i.Datasource)
		}
		api, err := measure.GetPromAPIClient(d.URL)
		if err != nil {
			failed.Inc()
			return fmt.Errorf("Failed to create Prom API Client: %v", err)
		}
		res, err := i.MeasureProm(ctx, api)
		if err != nil {
			failed.Inc()
			return fmt.Errorf("Failed to measure prometheus query: %v", err)
		}
		inputs[i.Name] = res
	}
	output, err := algorithmer.ApplyAlgorithm(ctx, c.Algorithm, c.AlgorithmParams, inputs, tempWorkDir)
	if c.Debug {
		defer logger.Debugf("Output: %s", output.CombinedOut)
	}
	if err != nil || output.RC != 0 {
		failed.Inc()
		logger.Errorf("%s check failed: %v, RC:%d", c.Name, err, output.RC)

		for _, a := range c.Actions {
			actioner := getActioner(&a, conf, logger)
			logger.Infof("Dispatching Action:%s", a.Name)
			out, err := actioner.Action(ctx, a.Action, output.CombinedOut, a.Params, tempWorkDir)
			if err != nil {
				logger.Errorf("%s Action Failed with error:%v", a.Name, err)
			}
			// Store Values to Database
			actionKey, err := s.PutAction(ctx, c.Name, &a, &out)
			if err != nil {
				logger.Errorf("%s Action Storage Failed with error:%v", a.Name, err)
			}
			output.ActionKeys = append(output.ActionKeys, actionKey)
			outputKey, err := s.PutCheck(ctx, c, &output)
			if err != nil {
				logger.Errorf("Check Storage Failed: %v", err)
			}
			logger.Errorf("Exited with failure. Output Stored to Key:%s", outputKey)
		}
		return err
	}

	outputKey, err := s.PutCheck(ctx, c, &output)
	if err != nil {
		logger.Errorf("Check Storage Failed: %v", err)
	}
	logger.Infof("Exited successfully. Output Stored to Key:%s", outputKey)
	succeeded.Inc()
	return nil
}
