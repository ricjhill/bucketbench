package benches

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/estesp/bucketbench/driver"
	log "github.com/sirupsen/logrus"
)

// CustomBench benchmark runs a series of container lifecycle operations as
// defined in the provided YAML against specified image and driver types
type CustomBench struct {
	benchName   string
	driver      driver.Driver
	imageInfo   string
	cmdOverride string
	logDriver   string
	logOpts     map[string]string
	trace       bool
	stats       []RunStatistics
	elapsed     time.Duration
	state       State
	wg          sync.WaitGroup
}

// Init initializes the benchmark
func (cb *CustomBench) Init(name string, driverType driver.Type, binaryPath, imageInfo, cmdOverride string, trace bool) error {
	driver, err := driver.New(driverType, binaryPath, cb.logDriver, cb.logOpts)
	if err != nil {
		return fmt.Errorf("Error during driver initialization for CustomBench: %v", err)
	}
	// get driver info; will also validate for daemon-based variants whether system is ready/up
	// and running for benchmarking
	info, err := driver.Info()
	if err != nil {
		return fmt.Errorf("Error during driver info query: %v", err)
	}
	log.Infof("Driver initialized: %s", info)
	// prepare environment
	err = driver.Clean()
	if err != nil {
		return fmt.Errorf("Error during driver init cleanup: %v", err)
	}
	cb.benchName = name
	cb.imageInfo = imageInfo
	cb.cmdOverride = cmdOverride
	cb.driver = driver
	cb.trace = trace
	return nil
}

// Validate the unit of benchmark execution (create-run-stop-remove) against
// the initialized driver.
func (cb *CustomBench) Validate() error {
	ctr, err := cb.driver.Create("bb-test", cb.imageInfo, cb.cmdOverride, true, cb.trace)
	if err != nil {
		return fmt.Errorf("Driver validation: error creating test container: %v", err)
	}

	_, _, err = cb.driver.Run(ctr)
	if err != nil {
		return fmt.Errorf("Driver validation: error running test container: %v", err)
	}

	_, _, err = cb.driver.Stop(ctr)
	if err != nil {
		return fmt.Errorf("Driver validation: error stopping test container: %v", err)
	}
	// allow time for quiesce of stopped state in process and container executor metadata
	time.Sleep(50 * time.Millisecond)

	_, _, err = cb.driver.Remove(ctr)
	if err != nil {
		return fmt.Errorf("Driver validation: error deleting test container: %v", err)
	}
	return nil
}

// Run executes the benchmark iterations against a specific engine driver type
// for a specified number of iterations
func (cb *CustomBench) Run(threads, iterations int, commands []string) error {
	log.Infof("Start CustomBench run: threads (%d); iterations (%d)", threads, iterations)
	statChan := make([]chan RunStatistics, threads)
	for i := range statChan {
		statChan[i] = make(chan RunStatistics, iterations)
	}
	cb.state = Running
	start := time.Now()
	for i := 0; i < threads; i++ {
		// create a driver instance for each thread to protect from drivers
		// which may not be threadsafe (e.g. gRPC client connection in containerd?)
		drv, err := driver.New(cb.driver.Type(), cb.driver.Path(), cb.logDriver, cb.logOpts)
		if err != nil {
			return fmt.Errorf("error creating new driver for thread %d: %v", i, err)
		}
		cb.wg.Add(1)
		go cb.runThread(drv, i, iterations, commands, statChan[i])
	}
	cb.wg.Wait()
	cb.elapsed = time.Since(start)

	log.Infof("CustomBench threads complete in %v time elapsed", cb.elapsed)
	//collect stats
	for _, ch := range statChan {
		for statEntry := range ch {
			cb.stats = append(cb.stats, statEntry)
		}
	}
	cb.state = Completed
	// final environment cleanup
	if err := cb.driver.Clean(); err != nil {
		return fmt.Errorf("Error during driver final cleanup: %v", err)
	}
	return nil
}

func (cb *CustomBench) runThread(runner driver.Driver, threadNum, iterations int, commands []string, stats chan RunStatistics) {
	for i := 0; i < iterations; i++ {
		errors := make(map[string]int)
		durations := make(map[string]time.Duration)
		// commands are specified in the passed in array; we will need
		// a container for each set of commands:
		name := fmt.Sprintf("%s-%d-%d", driver.ContainerNamePrefix, threadNum, i)
		ctr, err := runner.Create(name, cb.imageInfo, cb.cmdOverride, true, cb.trace)
		if err != nil {
			log.Errorf("Error on creating container %q from image %q: %v", name, cb.imageInfo, err)
		}

		for _, cmd := range commands {
			log.Debugf("running command: %s", cmd)
			switch strings.ToLower(cmd) {
			case "run", "start":
				out, runElapsed, err := runner.Run(ctr)
				if err != nil {
					errors["run"]++
					log.Warnf("Error during container command %q on %q: %v\n  Output: %s", cmd, name, err, out)
				}
				durations["run"] = runElapsed
				log.Debug(out)
			case "stop", "kill":
				out, stopElapsed, err := runner.Stop(ctr)
				if err != nil {
					errors["stop"]++
					log.Warnf("Error during container command %q on %q: %v\n  Output: %s", cmd, name, err, out)
				}
				durations["stop"] = stopElapsed
				log.Debug(out)
			case "remove", "erase", "delete":
				out, rmElapsed, err := runner.Remove(ctr)
				if err != nil {
					errors["delete"]++
					log.Warnf("Error during container command %q on %q: %v\n  Output: %s", cmd, name, err, out)
				}
				durations["delete"] = rmElapsed
				log.Debug(out)
			case "pause":
				out, pauseElapsed, err := runner.Pause(ctr)
				if err != nil {
					errors["pause"]++
					log.Warnf("Error during container command %q on %q: %v\n  Output: %s", cmd, name, err, out)
				}
				durations["pause"] = pauseElapsed
				log.Debug(out)
			case "unpause", "resume":
				out, unpauseElapsed, err := runner.Unpause(ctr)
				if err != nil {
					errors["resume"]++
					log.Warnf("Error during container command %q on %q: %v\n  Output: %s", cmd, name, err, out)
				}
				durations["resume"] = unpauseElapsed
				log.Debug(out)
			case "wait":
				out, waitElapsed, err := runner.Wait(ctr)
				if err != nil {
					errors["wait"]++
					log.Warnf("Error during container command %q on %q: %v\n  Output: %s", cmd, name, err, out)
				}
				durations["wait"] = waitElapsed
				log.Debug(out)
			case "metrics", "stats":
				out, err := runner.Metrics(ctr)
				if err != nil {
					errors["metrics"]++
					log.Warnf("Error during container command %q on %q: %v\n  Output: %s", cmd, name, err, out)
				}
				log.Debug(out)
			default:
				log.Errorf("Command %q unrecognized from YAML commands list; skipping", cmd)
			}
		}
		stats <- RunStatistics{
			Durations: durations,
			Errors:    errors,
			Timestamp: time.Now().UTC(),
		}
	}
	if err := runner.Close(); err != nil {
		log.Errorf("error on closing driver: %v", err)
	}
	close(stats)
	cb.wg.Done()
}

// Stats returns the statistics of the benchmark run
func (cb *CustomBench) Stats() []RunStatistics {
	if cb.state == Completed {
		return cb.stats
	}
	return []RunStatistics{}
}

// State returns Created, Running, or Completed
func (cb *CustomBench) State() State {
	return cb.state
}

// Elapsed returns the time.Duration taken to run the benchmark
func (cb *CustomBench) Elapsed() time.Duration {
	return cb.elapsed
}

// Type returns the type of benchmark
func (cb *CustomBench) Type() Type {
	return Custom
}

// Info returns a string with the driver type and custom benchmark name
func (cb *CustomBench) Info() string {
	driverType := driver.TypeToString(cb.driver.Type())
	return cb.benchName + ":" + driverType
}
