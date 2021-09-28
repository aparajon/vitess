/*
   Copyright 2014 Outbrain Inc.

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

package logic

import (
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/patrickmn/go-cache"
	"github.com/rcrowley/go-metrics"
	"github.com/sjmudd/stopwatch"

	"vitess.io/vitess/go/vt/orchestrator/collection"
	"vitess.io/vitess/go/vt/orchestrator/config"
	"vitess.io/vitess/go/vt/orchestrator/discovery"
	"vitess.io/vitess/go/vt/orchestrator/external/golib/log"
	"vitess.io/vitess/go/vt/orchestrator/inst"
	"vitess.io/vitess/go/vt/orchestrator/kv"
	ometrics "vitess.io/vitess/go/vt/orchestrator/metrics"
	"vitess.io/vitess/go/vt/orchestrator/process"
	"vitess.io/vitess/go/vt/orchestrator/util"
	"vitess.io/vitess/go/vt/vtctl/reparentutil"
)

const (
	discoveryMetricsName = "DISCOVERY_METRICS"
)

// discoveryQueue is a channel of deduplicated instanceKey-s
// that were requested for discovery.  It can be continuously updated
// as discovery process progresses.
var discoveryQueue *discovery.Queue
var snapshotDiscoveryKeys chan inst.InstanceKey
var snapshotDiscoveryKeysMutex sync.Mutex
var hasReceivedSIGTERM int32
var ersInProgressMutex sync.Mutex
var ersInProgress bool

var discoveriesCounter = metrics.NewCounter()
var failedDiscoveriesCounter = metrics.NewCounter()
var instancePollSecondsExceededCounter = metrics.NewCounter()
var discoveryQueueLengthGauge = metrics.NewGauge()
var discoveryRecentCountGauge = metrics.NewGauge()
var isElectedGauge = metrics.NewGauge()
var isHealthyGauge = metrics.NewGauge()
var discoveryMetrics = collection.CreateOrReturnCollection(discoveryMetricsName)

var isElectedNode int64 = 0

var recentDiscoveryOperationKeys *cache.Cache
var kvFoundCache = cache.New(10*time.Minute, time.Minute)

func init() {
	snapshotDiscoveryKeys = make(chan inst.InstanceKey, 10)

	metrics.Register("discoveries.attempt", discoveriesCounter)
	metrics.Register("discoveries.fail", failedDiscoveriesCounter)
	metrics.Register("discoveries.instance_poll_seconds_exceeded", instancePollSecondsExceededCounter)
	metrics.Register("discoveries.queue_length", discoveryQueueLengthGauge)
	metrics.Register("discoveries.recent_count", discoveryRecentCountGauge)
	metrics.Register("elect.is_elected", isElectedGauge)
	metrics.Register("health.is_healthy", isHealthyGauge)

	ometrics.OnMetricsTick(func() {
		discoveryQueueLengthGauge.Update(int64(discoveryQueue.QueueLen()))
	})
	ometrics.OnMetricsTick(func() {
		if recentDiscoveryOperationKeys == nil {
			return
		}
		discoveryRecentCountGauge.Update(int64(recentDiscoveryOperationKeys.ItemCount()))
	})
	ometrics.OnMetricsTick(func() {
		isElectedGauge.Update(atomic.LoadInt64(&isElectedNode))
	})
	ometrics.OnMetricsTick(func() {
		isHealthyGauge.Update(atomic.LoadInt64(&process.LastContinousCheckHealthy))
	})
}

func IsLeader() bool {
	return atomic.LoadInt64(&isElectedNode) == 1
}

func IsLeaderOrActive() bool {
	return atomic.LoadInt64(&isElectedNode) == 1
}

// used in several places
func instancePollSecondsDuration() time.Duration {
	return time.Duration(config.Config.InstancePollSeconds) * time.Second
}

// acceptSignals registers for OS signals
func acceptSignals() {
	c := make(chan os.Signal, 1)

	signal.Notify(c, syscall.SIGHUP)
	signal.Notify(c, syscall.SIGTERM)
	go func() {
		for sig := range c {
			switch sig {
			case syscall.SIGHUP:
				log.Infof("Received SIGHUP. Reloading configuration")
				inst.AuditOperation("reload-configuration", nil, "Triggered via SIGHUP")
				config.Reload()
				discoveryMetrics.SetExpirePeriod(time.Duration(config.Config.DiscoveryCollectionRetentionSeconds) * time.Second)
			case syscall.SIGTERM:
				log.Infof("Received SIGTERM. Starting shutdown")
				atomic.StoreInt32(&hasReceivedSIGTERM, 1)
				discoveryMetrics.StopAutoExpiration()
				// probably should poke other go routines to stop cleanly here ...
				inst.AuditOperation("shutdown", nil, "Triggered via SIGTERM")
				timeout := time.After(*shutdownWaitTime)
				func() {
					for {
						count := atomic.LoadInt32(&shardsLockCounter)
						if count == 0 {
							return
						}
						select {
						case <-timeout:
							log.Infof("wait for lock release timed out. Some locks might not have been released.")
							return
						default:
							time.Sleep(100 * time.Millisecond)
						}
					}
				}()
				log.Infof("Shutting down orchestrator")
				os.Exit(0)
			}
		}
	}()
}

// handleDiscoveryRequests iterates the discoveryQueue channel and calls upon
// instance discovery per entry.
func handleDiscoveryRequests() {
	discoveryQueue = discovery.CreateOrReturnQueue("DEFAULT")

	// create a pool of discovery workers
	for i := uint(0); i < config.Config.DiscoveryMaxConcurrency; i++ {
		go func() {
			for {
				instanceKey := discoveryQueue.Consume()
				// Possibly this used to be the elected node, but has
				// been demoted, while still the queue is full.
				if !IsLeaderOrActive() {
					log.Debugf("Node apparently demoted. Skipping discovery of %+v. "+
						"Remaining queue size: %+v", instanceKey, discoveryQueue.QueueLen())
					discoveryQueue.Release(instanceKey)
					continue
				}

				DiscoverInstance(instanceKey, false)
				discoveryQueue.Release(instanceKey)
			}
		}()
	}
}

// DiscoverInstance will attempt to discover (poll) an instance (unless
// it is already up to date) and will also ensure that its primary and
// replicas (if any) are also checked.
func DiscoverInstance(instanceKey inst.InstanceKey, forceDiscovery bool) {
	if inst.InstanceIsForgotten(&instanceKey) {
		log.Debugf("discoverInstance: skipping discovery of %+v because it is set to be forgotten", instanceKey)
		return
	}
	if inst.RegexpMatchPatterns(instanceKey.StringCode(), config.Config.DiscoveryIgnoreHostnameFilters) {
		log.Debugf("discoverInstance: skipping discovery of %+v because it matches DiscoveryIgnoreHostnameFilters", instanceKey)
		return
	}

	// create stopwatch entries
	latency := stopwatch.NewNamedStopwatch()
	latency.AddMany([]string{
		"backend",
		"instance",
		"total"})
	latency.Start("total") // start the total stopwatch (not changed anywhere else)

	defer func() {
		latency.Stop("total")
		discoveryTime := latency.Elapsed("total")
		if discoveryTime > instancePollSecondsDuration() {
			instancePollSecondsExceededCounter.Inc(1)
			log.Warningf("discoverInstance exceeded InstancePollSeconds for %+v, took %.4fs", instanceKey, discoveryTime.Seconds())
		}
	}()

	instanceKey.ResolveHostname()
	if !instanceKey.IsValid() {
		return
	}

	// Calculate the expiry period each time as InstancePollSeconds
	// _may_ change during the run of the process (via SIGHUP) and
	// it is not possible to change the cache's default expiry..
	if existsInCacheError := recentDiscoveryOperationKeys.Add(instanceKey.DisplayString(), true, instancePollSecondsDuration()); existsInCacheError != nil && !forceDiscovery {
		// Just recently attempted
		return
	}

	latency.Start("backend")
	instance, found, _ := inst.ReadInstance(&instanceKey)
	latency.Stop("backend")
	if !forceDiscovery && found && instance.IsUpToDate && instance.IsLastCheckValid {
		// we've already discovered this one. Skip!
		return
	}

	discoveriesCounter.Inc(1)

	// First we've ever heard of this instance. Continue investigation:
	instance, err := inst.ReadTopologyInstanceBufferable(&instanceKey, config.Config.BufferInstanceWrites, latency)
	// panic can occur (IO stuff). Therefore it may happen
	// that instance is nil. Check it, but first get the timing metrics.
	totalLatency := latency.Elapsed("total")
	backendLatency := latency.Elapsed("backend")
	instanceLatency := latency.Elapsed("instance")

	if instance == nil {
		failedDiscoveriesCounter.Inc(1)
		discoveryMetrics.Append(&discovery.Metric{
			Timestamp:       time.Now(),
			InstanceKey:     instanceKey,
			TotalLatency:    totalLatency,
			BackendLatency:  backendLatency,
			InstanceLatency: instanceLatency,
			Err:             err,
		})
		if util.ClearToLog("discoverInstance", instanceKey.StringCode()) {
			log.Warningf(" DiscoverInstance(%+v) instance is nil in %.3fs (Backend: %.3fs, Instance: %.3fs), error=%+v",
				instanceKey,
				totalLatency.Seconds(),
				backendLatency.Seconds(),
				instanceLatency.Seconds(),
				err)
		}
		return
	}

	discoveryMetrics.Append(&discovery.Metric{
		Timestamp:       time.Now(),
		InstanceKey:     instanceKey,
		TotalLatency:    totalLatency,
		BackendLatency:  backendLatency,
		InstanceLatency: instanceLatency,
		Err:             nil,
	})
}

// onHealthTick handles the actions to take to discover/poll instances
func onHealthTick() {
	wasAlreadyElected := IsLeader()

	{
		myIsElectedNode, err := process.AttemptElection()
		if err != nil {
			log.Errore(err)
		}
		if myIsElectedNode {
			atomic.StoreInt64(&isElectedNode, 1)
		} else {
			atomic.StoreInt64(&isElectedNode, 0)
		}
		if !myIsElectedNode {
			if electedNode, _, err := process.ElectedNode(); err == nil {
				log.Infof("Not elected as active node; active node: %v; polling", electedNode.Hostname)
			} else {
				log.Infof("Not elected as active node; active node: Unable to determine: %v; polling", err)
			}
		}
	}
	if !IsLeaderOrActive() {
		return
	}
	instanceKeys, err := inst.ReadOutdatedInstanceKeys()
	if err != nil {
		log.Errore(err)
	}

	if !wasAlreadyElected {
		// Just turned to be leader!
		go process.RegisterNode(process.ThisNodeHealth)
		go inst.ExpireMaintenance()
	}

	func() {
		// Normally onHealthTick() shouldn't run concurrently. It is kicked by a ticker.
		// However it _is_ invoked inside a goroutine. I like to be safe here.
		snapshotDiscoveryKeysMutex.Lock()
		defer snapshotDiscoveryKeysMutex.Unlock()

		countSnapshotKeys := len(snapshotDiscoveryKeys)
		for i := 0; i < countSnapshotKeys; i++ {
			instanceKeys = append(instanceKeys, <-snapshotDiscoveryKeys)
		}
	}()
	// avoid any logging unless there's something to be done
	if len(instanceKeys) > 0 {
		for _, instanceKey := range instanceKeys {
			if instanceKey.IsValid() {
				discoveryQueue.Push(instanceKey)
			}
		}
	}
}

// SubmitPrimariesToKvStores records a cluster's primary (or all clusters primaries) to kv stores.
// This should generally only happen once in a lifetime of a cluster. Otherwise KV
// stores are updated via failovers.
func SubmitPrimariesToKvStores(clusterName string, force bool) (kvPairs [](*kv.KVPair), submittedCount int, err error) {
	kvPairs, err = inst.GetPrimariesKVPairs(clusterName)
	log.Debugf("kv.SubmitPrimariesToKvStores, clusterName: %s, force: %+v: numPairs: %+v", clusterName, force, len(kvPairs))
	if err != nil {
		return kvPairs, submittedCount, log.Errore(err)
	}
	var selectedError error
	var submitKvPairs [](*kv.KVPair)
	for _, kvPair := range kvPairs {
		if !force {
			// !force: Called periodically to auto-populate KV
			// We'd like to avoid some overhead.
			if _, found := kvFoundCache.Get(kvPair.Key); found {
				// Let's not overload database with queries. Let's not overload raft with events.
				continue
			}
			v, found, err := kv.GetValue(kvPair.Key)
			if err == nil && found && v == kvPair.Value {
				// Already has the right value.
				kvFoundCache.Set(kvPair.Key, true, cache.DefaultExpiration)
				continue
			}
		}
		submitKvPairs = append(submitKvPairs, kvPair)
	}
	log.Debugf("kv.SubmitPrimariesToKvStores: submitKvPairs: %+v", len(submitKvPairs))
	for _, kvPair := range submitKvPairs {
		err := kv.PutKVPair(kvPair)
		if err == nil {
			submittedCount++
		} else {
			selectedError = err
		}
	}
	if err := kv.DistributePairs(kvPairs); err != nil {
		log.Errore(err)
	}
	return kvPairs, submittedCount, log.Errore(selectedError)
}

func injectSeeds(seedOnce *sync.Once) {
	seedOnce.Do(func() {
		for _, seed := range config.Config.DiscoverySeeds {
			instanceKey, err := inst.ParseRawInstanceKey(seed)
			if err == nil {
				inst.InjectSeed(instanceKey)
			} else {
				log.Errorf("Error parsing seed %s: %+v", seed, err)
			}
		}
	})
}

// ContinuousDiscovery starts an asynchronuous infinite discovery process where instances are
// periodically investigated and their status captured, and long since unseen instances are
// purged and forgotten.
//nolint SA1015: using time.Tick leaks the underlying ticker
func ContinuousDiscovery() {
	log.Infof("continuous discovery: setting up")
	continuousDiscoveryStartTime := time.Now()
	checkAndRecoverWaitPeriod := 3 * instancePollSecondsDuration()
	recentDiscoveryOperationKeys = cache.New(instancePollSecondsDuration(), time.Second)

	inst.LoadHostnameResolveCache()
	go handleDiscoveryRequests()

	healthTick := time.Tick(config.HealthPollSeconds * time.Second)
	instancePollTick := time.Tick(instancePollSecondsDuration())
	caretakingTick := time.Tick(time.Minute)
	recoveryTick := time.Tick(time.Duration(config.RecoveryPollSeconds) * time.Second)
	tabletTopoTick := OpenTabletDiscovery()
	var recoveryEntrance int64
	var snapshotTopologiesTick <-chan time.Time
	if config.Config.SnapshotTopologiesIntervalHours > 0 {
		snapshotTopologiesTick = time.Tick(time.Duration(config.Config.SnapshotTopologiesIntervalHours) * time.Hour)
	}

	runCheckAndRecoverOperationsTimeRipe := func() bool {
		return time.Since(continuousDiscoveryStartTime) >= checkAndRecoverWaitPeriod
	}

	var seedOnce sync.Once

	go ometrics.InitMetrics()
	go acceptSignals()
	go kv.InitKVStores()
	reparentutil.SetDurabilityPolicy(config.Config.Durability, config.Config.DurabilityParams)

	if *config.RuntimeCLIFlags.GrabElection {
		process.GrabElection()
	}

	log.Infof("continuous discovery: starting")
	for {
		select {
		case <-healthTick:
			go func() {
				onHealthTick()
			}()
		case <-instancePollTick:
			go func() {
				// This tick does NOT do instance poll (these are handled by the oversampling discoveryTick)
				// But rather should invoke such routinely operations that need to be as (or roughly as) frequent
				// as instance poll
				if IsLeaderOrActive() {
					go inst.UpdateClusterAliases()
					go inst.ExpireDowntime()
					go injectSeeds(&seedOnce)
				}
			}()
		case <-caretakingTick:
			// Various periodic internal maintenance tasks
			go func() {
				if IsLeaderOrActive() {
					go inst.ReviewUnseenInstances()
					go inst.InjectUnseenPrimaries()

					go inst.ForgetLongUnseenInstances()
					go inst.ForgetUnseenInstancesDifferentlyResolved()
					go inst.ForgetExpiredHostnameResolves()
					go inst.DeleteInvalidHostnameResolves()
					go inst.ResolveUnknownPrimaryHostnameResolves()
					go inst.ExpireMaintenance()
					go inst.ExpireCandidateInstances()
					go inst.ExpireHostnameUnresolve()
					go inst.ExpireClusterDomainName()
					go inst.ExpireAudit()
					go inst.ExpirePoolInstances()
					go inst.FlushNontrivialResolveCacheToDatabase()
					go inst.ExpireStaleInstanceBinlogCoordinates()
					go process.ExpireNodesHistory()
					go process.ExpireAccessTokens()
					go process.ExpireAvailableNodes()
					go ExpireFailureDetectionHistory()
					go ExpireTopologyRecoveryHistory()
					go ExpireTopologyRecoveryStepsHistory()

					if runCheckAndRecoverOperationsTimeRipe() && IsLeader() {
						go SubmitPrimariesToKvStores("", false)
					}
				} else {
					// Take this opportunity to refresh yourself
					go inst.LoadHostnameResolveCache()
				}
			}()
		case <-recoveryTick:
			go func() {
				if IsLeaderOrActive() {
					go ClearActiveFailureDetections()
					go ClearActiveRecoveries()
					go ExpireBlockedRecoveries()
					go AcknowledgeCrashedRecoveries()
					go inst.ExpireInstanceAnalysisChangelog()

					go func() {
						// This function is non re-entrant (it can only be running once at any point in time)
						if atomic.CompareAndSwapInt64(&recoveryEntrance, 0, 1) {
							defer atomic.StoreInt64(&recoveryEntrance, 0)
						} else {
							return
						}
						if runCheckAndRecoverOperationsTimeRipe() {
							CheckAndRecover(nil, nil, false)
						} else {
							log.Debugf("Waiting for %+v seconds to pass before running failure detection/recovery", checkAndRecoverWaitPeriod.Seconds())
						}
					}()
				}
			}()
		case <-snapshotTopologiesTick:
			go func() {
				if IsLeaderOrActive() {
					go inst.SnapshotTopologies()
				}
			}()
		case <-tabletTopoTick:
			go RefreshTablets(false)
		}
	}
}
