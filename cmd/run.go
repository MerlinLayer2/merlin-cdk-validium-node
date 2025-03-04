package main

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/pprof"
	"os"
	"os/signal"
	"runtime"
	"time"

	agglayerClient "github.com/0xPolygon/agglayer/client"
	dataCommitteeClient "github.com/0xPolygon/cdk-data-availability/client"
	datastreamerlog "github.com/0xPolygonHermez/zkevm-data-streamer/log"
	"github.com/0xPolygonHermez/zkevm-node"
	"github.com/0xPolygonHermez/zkevm-node/aggregator"
	"github.com/0xPolygonHermez/zkevm-node/config"
	"github.com/0xPolygonHermez/zkevm-node/dataavailability"
	"github.com/0xPolygonHermez/zkevm-node/dataavailability/datacommittee"
	"github.com/0xPolygonHermez/zkevm-node/db"
	"github.com/0xPolygonHermez/zkevm-node/etherman"
	"github.com/0xPolygonHermez/zkevm-node/ethtxmanager"
	"github.com/0xPolygonHermez/zkevm-node/event"
	"github.com/0xPolygonHermez/zkevm-node/event/nileventstorage"
	"github.com/0xPolygonHermez/zkevm-node/event/pgeventstorage"
	"github.com/0xPolygonHermez/zkevm-node/gasprice"
	"github.com/0xPolygonHermez/zkevm-node/jsonrpc"
	"github.com/0xPolygonHermez/zkevm-node/jsonrpc/client"
	"github.com/0xPolygonHermez/zkevm-node/log"
	"github.com/0xPolygonHermez/zkevm-node/merkletree"
	"github.com/0xPolygonHermez/zkevm-node/metrics"
	"github.com/0xPolygonHermez/zkevm-node/pool"
	"github.com/0xPolygonHermez/zkevm-node/pool/pgpoolstorage"
	"github.com/0xPolygonHermez/zkevm-node/sequencer"
	"github.com/0xPolygonHermez/zkevm-node/sequencesender"
	"github.com/0xPolygonHermez/zkevm-node/state"
	"github.com/0xPolygonHermez/zkevm-node/state/pgstatestorage"
	"github.com/0xPolygonHermez/zkevm-node/state/runtime/executor"
	"github.com/0xPolygonHermez/zkevm-node/synchronizer"
	"github.com/0xPolygonHermez/zkevm-node/synchronizer/common/syncinterfaces"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/jackc/pgx/v4/pgxpool"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/urfave/cli/v2"
)

func start(cliCtx *cli.Context) error {
	c, err := config.Load(cliCtx, true)
	if err != nil {
		return err
	}
	setupLog(c.Log)

	if c.Log.Environment == log.EnvironmentDevelopment {
		zkevm.PrintVersion(os.Stdout)
		log.Info("Starting application")
	} else if c.Log.Environment == log.EnvironmentProduction {
		logVersion()
	}

	if c.Metrics.Enabled {
		metrics.Init()
	}
	components := cliCtx.StringSlice(config.FlagComponents)

	// Only runs migration if the component is the synchronizer and if the flag is deactivated
	if !cliCtx.Bool(config.FlagMigrations) {
		for _, comp := range components {
			if comp == SYNCHRONIZER {
				log.Infof("Running DB migrations host: %s:%s db:%s user:%s", c.State.DB.Host, c.State.DB.Port, c.State.DB.Name, c.State.DB.User)
				runStateMigrations(c.State.DB)
			}
		}
	}
	checkStateMigrations(c.State.DB)

	var (
		eventLog                      *event.EventLog
		eventStorage                  event.Storage
		cancelFuncs                   []context.CancelFunc
		needsExecutor, needsStateTree bool
	)

	// Decide if this node instance needs an executor and/or a state tree
	for _, component := range components {
		switch component {
		case SEQUENCER, RPC, SYNCHRONIZER:
			needsExecutor = true
			needsStateTree = true
		}
	}

	if c.EventLog.DB.Name != "" {
		eventStorage, err = pgeventstorage.NewPostgresEventStorage(c.EventLog.DB)
		if err != nil {
			log.Fatal(err)
		}
	} else {
		eventStorage, err = nileventstorage.NewNilEventStorage()
		if err != nil {
			log.Fatal(err)
		}
	}
	eventLog = event.NewEventLog(c.EventLog, eventStorage)

	// Core State DB
	stateSqlDB, err := db.NewSQLDB(c.State.DB)
	if err != nil {
		log.Fatal(err)
	}

	// READ CHAIN ID FROM POE SC
	tmpEthMan, err := etherman.NewClient(c.Etherman, c.NetworkConfig.L1Config, nil, nil)
	if err != nil {
		log.Fatal(err)
	}
	l2ChainID, err := tmpEthMan.GetL2ChainID()
	if err != nil {
		log.Fatal(err)
	}

	etherman, err := etherman.NewClient(c.Etherman, c.NetworkConfig.L1Config, nil, nil)
	if err != nil {
		log.Fatal(err)
	}
	st, currentForkID := newState(cliCtx.Context, c, etherman, l2ChainID, stateSqlDB, eventLog, needsExecutor, needsStateTree, false)

	etherman, err = newEtherman(*c, st)
	if err != nil {
		log.Fatal(err)
	}

	c.Aggregator.ChainID = l2ChainID
	c.Sequencer.StreamServer.ChainID = l2ChainID
	log.Infof("Chain ID read from POE SC = %v", l2ChainID)
	// If the aggregator is restarted before the end of the sync process, this currentForkID could be wrong
	c.Aggregator.ForkId = currentForkID
	c.Pool.ForkID = currentForkID

	ethTxManagerStorage, err := ethtxmanager.NewPostgresStorage(c.State.DB)
	if err != nil {
		log.Fatal(err)
	}

	etm := ethtxmanager.New(c.EthTxManager, etherman, ethTxManagerStorage, st)

	ev := &event.Event{
		ReceivedAt: time.Now(),
		Source:     event.Source_Node,
		Level:      event.Level_Info,
		EventID:    event.EventID_NodeComponentStarted,
	}

	var poolInstance *pool.Pool

	if c.Metrics.ProfilingEnabled {
		go startProfilingHttpServer(c.Metrics)
	}
	for _, component := range components {
		switch component {
		case AGGREGATOR:
			ev.Component = event.Component_Aggregator
			ev.Description = "Running aggregator"
			err := eventLog.LogEvent(cliCtx.Context, ev)
			if err != nil {
				log.Fatal(err)
			}
			go runAggregator(cliCtx.Context, c.Aggregator, etherman, etm, st)
		case SEQUENCER:
			c.Sequencer.StreamServer.Log = datastreamerlog.Config{
				Environment: datastreamerlog.LogEnvironment(c.Log.Environment),
				Level:       c.Log.Level,
				Outputs:     c.Log.Outputs,
			}
			ev.Component = event.Component_Sequencer
			ev.Description = "Running sequencer"
			err := eventLog.LogEvent(cliCtx.Context, ev)
			if err != nil {
				log.Fatal(err)
			}
			if poolInstance == nil {
				poolInstance = createPool(c.Pool, c.State.Batch.Constraints, l2ChainID, st, eventLog)
			}
			seq := createSequencer(*c, poolInstance, st, etherman, eventLog)
			go seq.Start(cliCtx.Context)
		case SEQUENCE_SENDER:
			ev.Component = event.Component_Sequence_Sender
			ev.Description = "Running sequence sender"
			err := eventLog.LogEvent(cliCtx.Context, ev)
			if err != nil {
				log.Fatal(err)
			}
			if poolInstance == nil {
				poolInstance = createPool(c.Pool, c.State.Batch.Constraints, l2ChainID, st, eventLog)
			}
			seqSender := createSequenceSender(*c, poolInstance, ethTxManagerStorage, st, eventLog)
			go seqSender.Start(cliCtx.Context)
		case RPC:
			ev.Component = event.Component_RPC
			ev.Description = "Running JSON-RPC server"
			err := eventLog.LogEvent(cliCtx.Context, ev)
			if err != nil {
				log.Fatal(err)
			}
			if poolInstance == nil {
				poolInstance = createPool(c.Pool, c.State.Batch.Constraints, l2ChainID, st, eventLog)
			}
			if c.RPC.EnableL2SuggestedGasPricePolling {
				// Needed for rejecting transactions with too low gas price
				poolInstance.StartPollingMinSuggestedGasPrice(cliCtx.Context)
			}
			poolInstance.StartRefreshingBlockedAddressesPeriodically()
			apis := map[string]bool{}
			for _, a := range cliCtx.StringSlice(config.FlagHTTPAPI) {
				apis[a] = true
			}
			st, _ := newState(cliCtx.Context, c, etherman, l2ChainID, stateSqlDB, eventLog, needsExecutor, needsStateTree, true)
			go runJSONRPCServer(*c, etherman, l2ChainID, poolInstance, st, apis)
		case SYNCHRONIZER:
			ev.Component = event.Component_Synchronizer
			ev.Description = "Running synchronizer"
			err := eventLog.LogEvent(cliCtx.Context, ev)
			if err != nil {
				log.Fatal(err)
			}
			if poolInstance == nil {
				poolInstance = createPool(c.Pool, c.State.Batch.Constraints, l2ChainID, st, eventLog)
			}
			go runSynchronizer(*c, etherman, ethTxManagerStorage, st, poolInstance, eventLog)
		case ETHTXMANAGER:
			ev.Component = event.Component_EthTxManager
			ev.Description = "Running eth tx manager service"
			err := eventLog.LogEvent(cliCtx.Context, ev)
			if err != nil {
				log.Fatal(err)
			}
			etm := createEthTxManager(*c, ethTxManagerStorage, st)
			go etm.Start()
		case L2GASPRICER:
			ev.Component = event.Component_GasPricer
			ev.Description = "Running L2 gasPricer"
			err := eventLog.LogEvent(cliCtx.Context, ev)
			if err != nil {
				log.Fatal(err)
			}
			if poolInstance == nil {
				poolInstance = createPool(c.Pool, c.State.Batch.Constraints, l2ChainID, st, eventLog)
			}
			go runL2GasPriceSuggester(c.L2GasPriceSuggester, st, poolInstance, etherman)
		}
	}

	if c.Metrics.Enabled {
		go startMetricsHttpServer(c.Metrics)
	}

	waitSignal(cancelFuncs)

	return nil
}

func setupLog(c log.Config) {
	log.Init(c)
}

func runStateMigrations(c db.Config) {
	runMigrations(c, db.StateMigrationName)
}

func checkStateMigrations(c db.Config) {
	err := db.CheckMigrations(c, db.StateMigrationName)
	if err != nil {
		log.Fatal(err)
	}
}

func runPoolMigrations(c db.Config) {
	runMigrations(c, db.PoolMigrationName)
}

func runMigrations(c db.Config, name string) {
	log.Infof("running migrations for %v", name)
	err := db.RunMigrationsUp(c, name)
	if err != nil {
		log.Fatal(err)
	}
}

func newEtherman(c config.Config, st *state.State) (*etherman.Client, error) {
	ethman, err := etherman.NewClient(c.Etherman, c.NetworkConfig.L1Config, nil, nil)
	if err != nil {
		return nil, err
	}
	da, err := newDataAvailability(c, st, ethman, false)
	if err != nil {
		return nil, err
	}
	return etherman.NewClient(c.Etherman, c.NetworkConfig.L1Config, da, st)
}

func newDataAvailability(c config.Config, st *state.State, etherman *etherman.Client, isSequenceSender bool) (*dataavailability.DataAvailability, error) {
	var (
		trustedSequencerURL string
		dataSourcePriority  []dataavailability.DataSourcePriority
		err                 error
	)
	if !c.IsTrustedSequencer {
		if c.Synchronizer.TrustedSequencerURL != "" {
			trustedSequencerURL = c.Synchronizer.TrustedSequencerURL
		} else {
			log.Debug("getting trusted sequencer URL from smc")
			trustedSequencerURL, err = etherman.GetTrustedSequencerURL()
			if err != nil {
				return nil, fmt.Errorf("error getting trusted sequencer URI. Error: %v", err)
			}
		}
		log.Debug("trustedSequencerURL ", trustedSequencerURL)
	}
	zkEVMClient := client.NewClient(trustedSequencerURL)

	dataSourcePriority = c.Synchronizer.L2Synchronization.DataSourcePriority
	if len(dataSourcePriority) == 0 {
		dataSourcePriority = dataavailability.DefaultPriority
	}

	// Backend specific config
	daProtocolName, err := etherman.GetDAProtocolName()
	if err != nil {
		return nil, fmt.Errorf("error getting data availability protocol name: %v", err)
	}
	var daBackend dataavailability.DABackender
	switch daProtocolName {
	case string(dataavailability.DataAvailabilityCommittee):
		var (
			pk  *ecdsa.PrivateKey
			err error
		)
		if isSequenceSender {
			_, pk, err = etherman.LoadAuthFromKeyStore(c.SequenceSender.PrivateKey.Path, c.SequenceSender.PrivateKey.Password)
			if err != nil {
				return nil, err
			}
		}
		dacAddr, err := etherman.GetDAProtocolAddr()
		if err != nil {
			return nil, fmt.Errorf("error getting trusted sequencer URI. Error: %v", err)
		}

		daBackend, err = datacommittee.New(
			c.Etherman.URL,
			dacAddr,
			pk,
			dataCommitteeClient.NewFactory(),
		)
		if err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("unexpected / unsupported DA protocol: %s", daProtocolName)
	}

	return dataavailability.New(
		c.IsTrustedSequencer,
		daBackend,
		st,
		zkEVMClient,
		dataSourcePriority,
	)
}

func newL2EthClient(url string) (*ethclient.Client, error) {
	ethClient, err := ethclient.Dial(url)
	if err != nil {
		log.Errorf("error connecting L1 to %s: %+v", url, err)
		return nil, err
	}
	return ethClient, nil
}

func runSynchronizer(cfg config.Config, etherman *etherman.Client, ethTxManagerStorage *ethtxmanager.PostgresStorage, st *state.State, pool *pool.Pool, eventLog *event.EventLog) {
	var trustedSequencerURL string
	var err error
	if !cfg.IsTrustedSequencer {
		if cfg.Synchronizer.TrustedSequencerURL != "" {
			trustedSequencerURL = cfg.Synchronizer.TrustedSequencerURL
		} else {
			log.Debug("getting trusted sequencer URL from smc")
			trustedSequencerURL, err = etherman.GetTrustedSequencerURL()
			if err != nil {
				log.Fatal("error getting trusted sequencer URI. Error: %v", err)
			}
		}
		log.Info("trustedSequencerURL ", trustedSequencerURL)
	}
	var ethClientForL2 *ethclient.Client
	if trustedSequencerURL != "" {
		log.Infof("Creating L2 ethereum client %s", trustedSequencerURL)
		ethClientForL2, err = newL2EthClient(trustedSequencerURL)
		if err != nil {
			log.Fatalf("Can't create L2 ethereum client. Err:%w", err)
		}
	} else {
		ethClientForL2 = nil
		log.Infof("skipping creating L2 ethereum client because URL is empty")
	}
	zkEVMClient := client.NewClient(trustedSequencerURL)
	etherManForL1 := []syncinterfaces.EthermanFullInterface{}
	// If synchronizer are using sequential mode, we only need one etherman client
	if cfg.Synchronizer.L1SynchronizationMode == synchronizer.ParallelMode {
		for i := 0; i < int(cfg.Synchronizer.L1ParallelSynchronization.MaxClients+1); i++ {
			eth, err := newEtherman(cfg, st)
			if err != nil {
				log.Fatal(err)
			}
			etherManForL1 = append(etherManForL1, eth)
		}
	}
	etm := ethtxmanager.New(cfg.EthTxManager, etherman, ethTxManagerStorage, st)
	sy, err := synchronizer.NewSynchronizer(
		cfg.IsTrustedSequencer, etherman, etherManForL1, st, pool, etm,
		zkEVMClient, ethClientForL2, eventLog, cfg.NetworkConfig.Genesis, cfg.Synchronizer, cfg.Log.Environment == "development",
	)
	if err != nil {
		log.Fatal(err)
	}
	if err := sy.Sync(); err != nil {
		log.Fatal(err)
	}
}

func runJSONRPCServer(c config.Config, etherman *etherman.Client, chainID uint64, pool *pool.Pool, st *state.State, apis map[string]bool) {
	var err error
	storage := jsonrpc.NewStorage()
	c.RPC.MaxCumulativeGasUsed = c.State.Batch.Constraints.MaxCumulativeGasUsed
	c.RPC.L2Coinbase = c.SequenceSender.L2Coinbase
	c.RPC.ZKCountersLimits = jsonrpc.ZKCountersLimits{
		MaxKeccakHashes:     c.State.Batch.Constraints.MaxKeccakHashes,
		MaxPoseidonHashes:   c.State.Batch.Constraints.MaxPoseidonHashes,
		MaxPoseidonPaddings: c.State.Batch.Constraints.MaxPoseidonPaddings,
		MaxMemAligns:        c.State.Batch.Constraints.MaxMemAligns,
		MaxArithmetics:      c.State.Batch.Constraints.MaxArithmetics,
		MaxBinaries:         c.State.Batch.Constraints.MaxBinaries,
		MaxSteps:            c.State.Batch.Constraints.MaxSteps,
		MaxSHA256Hashes:     c.State.Batch.Constraints.MaxSHA256Hashes,
	}
	if !c.IsTrustedSequencer {
		if c.RPC.SequencerNodeURI == "" {
			log.Debug("getting trusted sequencer URL from smc")
			c.RPC.SequencerNodeURI, err = etherman.GetTrustedSequencerURL()
			if err != nil {
				log.Fatal("error getting trusted sequencer URI. Error: %v", err)
			}
		}
		log.Debug("SequencerNodeURI ", c.RPC.SequencerNodeURI)
	}

	services := []jsonrpc.Service{}
	if _, ok := apis[jsonrpc.APIEth]; ok {
		services = append(services, jsonrpc.Service{
			Name:    jsonrpc.APIEth,
			Service: jsonrpc.NewEthEndpoints(c.RPC, chainID, pool, st, etherman, storage),
		})
	}

	if _, ok := apis[jsonrpc.APINet]; ok {
		services = append(services, jsonrpc.Service{
			Name:    jsonrpc.APINet,
			Service: jsonrpc.NewNetEndpoints(c.RPC, chainID),
		})
	}

	if _, ok := apis[jsonrpc.APIZKEVM]; ok {
		services = append(services, jsonrpc.Service{
			Name:    jsonrpc.APIZKEVM,
			Service: jsonrpc.NewZKEVMEndpoints(c.RPC, pool, st, etherman),
		})
	}

	if _, ok := apis[jsonrpc.APITxPool]; ok {
		services = append(services, jsonrpc.Service{
			Name:    jsonrpc.APITxPool,
			Service: &jsonrpc.TxPoolEndpoints{},
		})
	}

	if _, ok := apis[jsonrpc.APIDebug]; ok {
		services = append(services, jsonrpc.Service{
			Name:    jsonrpc.APIDebug,
			Service: jsonrpc.NewDebugEndpoints(c.RPC, st, etherman),
		})
	}

	if _, ok := apis[jsonrpc.APIWeb3]; ok {
		services = append(services, jsonrpc.Service{
			Name:    jsonrpc.APIWeb3,
			Service: &jsonrpc.Web3Endpoints{},
		})
	}

	if _, ok := apis[jsonrpc.APIMerlin]; ok {
		services = append(services, jsonrpc.Service{
			Name:    jsonrpc.APIMerlin,
			Service: jsonrpc.NewMerlinEndpoints(c.RPC, st, etherman),
		})
	}

	if err := jsonrpc.NewServer(c.RPC, chainID, pool, st, storage, services).Start(); err != nil {
		log.Fatal(err)
	}
}

func createSequencer(cfg config.Config, pool *pool.Pool, st *state.State, etherman *etherman.Client, eventLog *event.EventLog) *sequencer.Sequencer {
	cfg.Sequencer.L2Coinbase = cfg.SequenceSender.L2Coinbase

	seq, err := sequencer.New(cfg.Sequencer, cfg.State.Batch, cfg.Pool, pool, st, etherman, eventLog)
	if err != nil {
		log.Fatal(err)
	}
	return seq
}

func createSequenceSender(cfg config.Config, pool *pool.Pool, etmStorage *ethtxmanager.PostgresStorage, st *state.State, eventLog *event.EventLog) *sequencesender.SequenceSender {
	etherman, err := newEtherman(cfg, st)
	if err != nil {
		log.Fatal(err)
	}

	auth, _, err := etherman.LoadAuthFromKeyStore(cfg.SequenceSender.PrivateKey.Path, cfg.SequenceSender.PrivateKey.Password)
	if err != nil {
		log.Fatal(err)
	}
	cfg.SequenceSender.SenderAddress = auth.From

	cfg.SequenceSender.ForkUpgradeBatchNumber = cfg.ForkUpgradeBatchNumber

	ethTxManager := ethtxmanager.New(cfg.EthTxManager, etherman, etmStorage, st)

	da, err := newDataAvailability(cfg, st, etherman, true)
	if err != nil {
		log.Fatal(err)
	}

	seqSender, err := sequencesender.New(cfg.SequenceSender, st, etherman, ethTxManager, eventLog, da)
	if err != nil {
		log.Fatal(err)
	}

	return seqSender
}

func runAggregator(ctx context.Context, c aggregator.Config, etherman *etherman.Client, ethTxManager *ethtxmanager.Client, st *state.State) {
	var (
		aggCli *agglayerClient.Client
		pk     *ecdsa.PrivateKey
		err    error
	)

	if c.SettlementBackend == aggregator.AggLayer {
		aggCli = agglayerClient.New(c.AggLayerURL)

		// Load private key
		pk, err = config.NewKeyFromKeystore(c.SequencerPrivateKey)
		if err != nil {
			log.Fatal(err)
		}
	}

	agg, err := aggregator.New(c, st, ethTxManager, etherman, aggCli, pk)
	if err != nil {
		log.Fatal(err)
	}
	err = agg.Start(ctx)
	if err != nil {
		log.Fatal(err)
	}
}

// runL2GasPriceSuggester init gas price gasPriceEstimator based on type in config.
func runL2GasPriceSuggester(cfg gasprice.Config, state *state.State, pool *pool.Pool, etherman *etherman.Client) {
	ctx := context.Background()
	gasprice.NewL2GasPriceSuggester(ctx, cfg, pool, etherman, state)
}

func waitSignal(cancelFuncs []context.CancelFunc) {
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt)

	for sig := range signals {
		switch sig {
		case os.Interrupt, os.Kill:
			log.Info("terminating application gracefully...")

			exitStatus := 0
			for _, cancel := range cancelFuncs {
				cancel()
			}
			os.Exit(exitStatus)
		}
	}
}

func newState(ctx context.Context, c *config.Config, etherman *etherman.Client, l2ChainID uint64, sqlDB *pgxpool.Pool, eventLog *event.EventLog, needsExecutor, needsStateTree, avoidForkIDInMemory bool) (*state.State, uint64) {
	// Executor
	var executorClient executor.ExecutorServiceClient
	if needsExecutor {
		executorClient, _, _ = executor.NewExecutorClient(ctx, c.Executor)
	}

	// State Tree
	var stateTree *merkletree.StateTree
	if needsStateTree {
		stateDBClient, _, _ := merkletree.NewMTDBServiceClient(ctx, c.MTClient)
		stateTree = merkletree.NewStateTree(stateDBClient)
	}

	stateCfg := state.Config{
		MaxCumulativeGasUsed:         c.State.Batch.Constraints.MaxCumulativeGasUsed,
		ChainID:                      l2ChainID,
		ForkIDIntervals:              []state.ForkIDInterval{},
		MaxResourceExhaustedAttempts: c.Executor.MaxResourceExhaustedAttempts,
		WaitOnResourceExhaustion:     c.Executor.WaitOnResourceExhaustion,
		ForkUpgradeBatchNumber:       c.ForkUpgradeBatchNumber,
		ForkUpgradeNewForkId:         c.ForkUpgradeNewForkId,
		MaxLogsCount:                 c.RPC.MaxLogsCount,
		MaxLogsBlockRange:            c.RPC.MaxLogsBlockRange,
		MaxNativeBlockHashBlockRange: c.RPC.MaxNativeBlockHashBlockRange,
		AvoidForkIDInMemory:          avoidForkIDInMemory,
	}
	stateDb := pgstatestorage.NewPostgresStorage(stateCfg, sqlDB)
	st := state.NewState(stateCfg, stateDb, executorClient, stateTree, eventLog, nil)
	// This is to force to build cache, and check that DB is ok before starting the application
	l1inforoot, err := st.GetCurrentL1InfoRoot(ctx, nil)
	if err != nil {
		log.Fatal("error getting current L1InfoRoot. Error: ", err)
	}
	log.Infof("Starting L1InfoRoot: %v", l1inforoot.String())

	forkIDIntervals, err := forkIDIntervals(ctx, st, etherman, c.NetworkConfig.Genesis.RollupBlockNumber)
	if err != nil {
		log.Fatal("error getting forkIDs. Error: ", err)
	}
	st.UpdateForkIDIntervalsInMemory(forkIDIntervals)

	currentForkID := forkIDIntervals[len(forkIDIntervals)-1].ForkId
	log.Infof("Fork ID read from POE SC = %v", currentForkID)

	return st, currentForkID
}

func createPool(cfgPool pool.Config, constraintsCfg state.BatchConstraintsCfg, l2ChainID uint64, st *state.State, eventLog *event.EventLog) *pool.Pool {
	runPoolMigrations(cfgPool.DB)
	poolStorage, err := pgpoolstorage.NewPostgresPoolStorage(cfgPool.DB)
	if err != nil {
		log.Fatal(err)
	}
	poolInstance := pool.NewPool(cfgPool, constraintsCfg, poolStorage, st, l2ChainID, eventLog)
	if cfgPool.EnableReadDB {
		log.Info("Read DB Enabled!")
		poolReadStorage, err := pgpoolstorage.NewPostgresPoolStorage(cfgPool.ReadDB)
		if err != nil {
			log.Fatal(err)
		}
		poolInstance.AddReadStorageCli(poolReadStorage)
	}
	return poolInstance
}

func createEthTxManager(cfg config.Config, etmStorage *ethtxmanager.PostgresStorage, st *state.State) *ethtxmanager.Client {
	etherman, err := newEtherman(cfg, st)
	if err != nil {
		log.Fatal(err)
	}

	for _, privateKey := range cfg.EthTxManager.PrivateKeys {
		_, _, err := etherman.LoadAuthFromKeyStore(privateKey.Path, privateKey.Password)
		if err != nil {
			log.Fatal(err)
		}
	}
	etm := ethtxmanager.New(cfg.EthTxManager, etherman, etmStorage, st)
	return etm
}

func startProfilingHttpServer(c metrics.Config) {
	const two = 2
	mux := http.NewServeMux()
	address := fmt.Sprintf("%s:%d", c.ProfilingHost, c.ProfilingPort)
	lis, err := net.Listen("tcp", address)
	if err != nil {
		log.Errorf("failed to create tcp listener for profiling: %v", err)
		return
	}
	mux.HandleFunc(metrics.ProfilingIndexEndpoint, pprof.Index)
	mux.HandleFunc(metrics.ProfileEndpoint, pprof.Profile)
	mux.HandleFunc(metrics.ProfilingCmdEndpoint, pprof.Cmdline)
	mux.HandleFunc(metrics.ProfilingSymbolEndpoint, pprof.Symbol)
	mux.HandleFunc(metrics.ProfilingTraceEndpoint, pprof.Trace)
	profilingServer := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: two * time.Minute,
		ReadTimeout:       two * time.Minute,
	}
	log.Infof("profiling server listening on port %d", c.ProfilingPort)
	if err := profilingServer.Serve(lis); err != nil {
		if err == http.ErrServerClosed {
			log.Warnf("http server for profiling stopped")
			return
		}
		log.Errorf("closed http connection for profiling server: %v", err)
		return
	}
}

func startMetricsHttpServer(c metrics.Config) {
	const ten = 10
	mux := http.NewServeMux()
	address := fmt.Sprintf("%s:%d", c.Host, c.Port)
	lis, err := net.Listen("tcp", address)
	if err != nil {
		log.Errorf("failed to create tcp listener for metrics: %v", err)
		return
	}
	mux.Handle(metrics.Endpoint, promhttp.Handler())

	metricsServer := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: ten * time.Second,
		ReadTimeout:       ten * time.Second,
	}
	log.Infof("metrics server listening on port %d", c.Port)
	if err := metricsServer.Serve(lis); err != nil {
		if err == http.ErrServerClosed {
			log.Warnf("http server for metrics stopped")
			return
		}
		log.Errorf("closed http connection for metrics server: %v", err)
		return
	}
}

func logVersion() {
	log.Infow("Starting application",
		// node version is already logged by default
		"gitRevision", zkevm.GitRev,
		"gitBranch", zkevm.GitBranch,
		"goVersion", runtime.Version(),
		"built", zkevm.BuildDate,
		"os/arch", fmt.Sprintf("%s/%s", runtime.GOOS, runtime.GOARCH),
	)
}

func forkIDIntervals(ctx context.Context, st *state.State, etherman *etherman.Client, genesisBlockNumber uint64) ([]state.ForkIDInterval, error) {
	log.Debug("getting forkIDs from db")
	forkIDIntervals, err := st.GetForkIDs(ctx, nil)
	if err != nil && !errors.Is(err, state.ErrStateNotSynchronized) {
		return []state.ForkIDInterval{}, fmt.Errorf("error getting forkIDs from db. Error: %v", err)
	}
	numberForkIDs := len(forkIDIntervals)
	log.Debug("numberForkIDs: ", numberForkIDs)
	// var forkIDIntervals []state.ForkIDInterval
	if numberForkIDs == 0 {
		// Get last L1block Synced
		lastBlock, err := st.GetLastBlock(ctx, nil)
		if err != nil && !errors.Is(err, state.ErrStateNotSynchronized) {
			return []state.ForkIDInterval{}, fmt.Errorf("error checking lastL1BlockSynced. Error: %v", err)
		}
		if lastBlock != nil {
			log.Info("Getting forkIDs intervals. Please wait...")
			// Read Fork ID FROM POE SC
			forkIntervals, err := etherman.GetForks(ctx, genesisBlockNumber, lastBlock.BlockNumber)
			if err != nil {
				return []state.ForkIDInterval{}, fmt.Errorf("error getting forks. Please check the configuration. Error: %v", err)
			} else if len(forkIntervals) == 0 {
				return []state.ForkIDInterval{}, fmt.Errorf("error: no forkID received. It should receive at least one, please check the configuration...")
			}

			dbTx, err := st.BeginStateTransaction(ctx)
			if err != nil {
				return []state.ForkIDInterval{}, fmt.Errorf("error creating dbTx. Error: %v", err)
			}
			log.Info("Storing forkID intervals into db")
			// Store forkIDs
			for _, f := range forkIntervals {
				err := st.AddForkID(ctx, f, dbTx)
				if err != nil {
					log.Errorf("error adding forkID to db. Error: %v", err)
					rollbackErr := dbTx.Rollback(ctx)
					if rollbackErr != nil {
						log.Errorf("error rolling back dbTx. RollbackErr: %s. Error : %v", rollbackErr.Error(), err)
						return []state.ForkIDInterval{}, rollbackErr
					}
					return []state.ForkIDInterval{}, fmt.Errorf("error adding forkID to db. Error: %v", err)
				}
			}
			err = dbTx.Commit(ctx)
			if err != nil {
				log.Errorf("error committing dbTx. Error: %v", err)
				rollbackErr := dbTx.Rollback(ctx)
				if rollbackErr != nil {
					log.Errorf("error rolling back dbTx. RollbackErr: %s. Error : %v", rollbackErr.Error(), err)
					return []state.ForkIDInterval{}, rollbackErr
				}
				return []state.ForkIDInterval{}, fmt.Errorf("error committing dbTx. Error: %v", err)
			}
			forkIDIntervals = forkIntervals
		} else {
			log.Debug("Getting all forkIDs")

			// Get last L1 block number
			bn, err := etherman.GetLatestBlockNumber(ctx)
			if err != nil {
				return []state.ForkIDInterval{}, fmt.Errorf("error getting latest block number. Error: %v", err)
			}

			// Get all forkIDs since genesis
			forkIntervals, err := etherman.GetForks(ctx, genesisBlockNumber, bn)
			if err != nil {
				return []state.ForkIDInterval{}, fmt.Errorf("error getting forks. Please check the configuration. Error: %v", err)
			} else if len(forkIntervals) == 0 {
				return []state.ForkIDInterval{}, fmt.Errorf("error: no forkID received. It should receive at least one, please check the configuration...")
			}
			forkIDIntervals = forkIntervals

			log.Debugf("Retrieved %d forkIDs", len(forkIDIntervals))

			log.Debug("Adding forkIDs to db and memory")
			for _, forkID := range forkIDIntervals {
				err = st.AddForkIDInterval(ctx, forkID, nil)
				if err != nil {
					log.Fatal("error adding forkID to db. Error: ", err)
				}
			}
		}
	}
	return forkIDIntervals, nil
}
