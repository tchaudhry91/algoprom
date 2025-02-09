package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

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
		logger.Printf("Starting Metrics Server on: %s", addr)
		http.Handle("/metrics", promhttp.Handler())
		http.ListenAndServe(addr, nil)
	}(addr)

	for _, c := range conf.Checks {
		ticker := time.NewTicker(c.Interval.Duration)
		tickers = append(tickers, ticker)
		logger.Printf("Starting Check: %s with interval:%s", c.Name, c.Interval.Duration)
		go func(c *algochecks.Check) {
			if c.Immediate {
				err := runCheck(c, conf, logger, s)
				if err != nil {
					logger.Printf("Error in Check: %s : %v", c.Name, err)
				}
			}
			// Add a little bit of random starting delay to stagger checks
			staggerSeconds := rand.Intn(int(c.Interval.Duration.Seconds()))
			logger.Printf("Adding Initial Stager of %d seconds", staggerSeconds)
			time.Sleep(time.Duration(staggerSeconds) * time.Second)
			for {
				select {
				case <-done:
					return
				case <-ticker.C:
					err := runCheck(c, conf, logger, s)
					if err != nil {
						logger.Printf("Error in Check: %s : %v", c.Name, err)
					}
				}
			}
		}(&c)
	}

	select {
	case signalKill := <-interrupt:
		logger.Println("Received Interrupt:", signalKill)
		for name, cancel := range contexts {
			logger.Println("Cancelling:", name)
			(*cancel)()
		}
		for _, ticker := range tickers {
			ticker.Stop()
		}
	case err := <-shutdown:
		logger.Println("Error:", err)
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
		return fmt.Errorf("AlgorithmerType:%s not found for check:%s", c.AlgorithmerType, c.Name)
	}

	processed := countProcessed.WithLabelValues(c.Name)
	succeeded := countSuccess.WithLabelValues(c.Name)
	failed := countFail.WithLabelValues(c.Name)
	defer processed.Inc()

	tempWorkDir, err := os.MkdirTemp(conf.BaseWorkingDir, c.Name+"-")
	if err != nil {
		failed.Inc()
		return fmt.Errorf("Unable to create Temp Dir for check %s: %v", c.Name, err)
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
		defer logger.Printf("%s Output: %s", c.Name, output.CombinedOut)
	}
	if err != nil || output.RC != 0 {
		failed.Inc()
		logger.Printf("%s check failed: %v, RC:%d", c.Name, err, output.RC)

		for _, a := range c.Actions {
			actioner := getActioner(&a, conf, logger)
			logger.Printf("Dispatching Action:%s for %s", a.Name, c.Name)
			out, err := actioner.Action(ctx, a.Action, output.CombinedOut, a.Params, tempWorkDir)
			if err != nil {
				logger.Printf("%s Action Failed for Check: %s with error:%v", a.Name, c.Name, err)
			}
			// Store Values to Database
			actionKey, err := s.PutAction(ctx, c.Name, &a, &out)
			if err != nil {
				logger.Printf("%s Action Storage Failed for Check: %s with error:%v", a.Name, c.Name, err)
			}
			output.ActionKeys = append(output.ActionKeys, actionKey)
			outputKey, err := s.PutCheck(ctx, c, &output)
			if err != nil {
				logger.Printf("%s Check Storage Failed: %v", c.Name, err)
			}
			logger.Printf("%s check exited with failure. Output Stored to Key:%s", c.Name, outputKey)
		}
		return err
	}

	outputKey, err := s.PutCheck(ctx, c, &output)
	if err != nil {
		logger.Printf("%s Check Storage Failed: %v", c.Name, err)
	}
	logger.Printf("%s check exited successfully. Output Stored to Key:%s", c.Name, outputKey)
	succeeded.Inc()
	return nil
}
