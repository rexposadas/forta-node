package inspector

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/cenkalti/backoff"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/forta-network/forta-core-go/clients/health"
	"github.com/forta-network/forta-core-go/inspect"
	"github.com/forta-network/forta-core-go/protocol/transform"
	"github.com/forta-network/forta-node/clients"
	"github.com/forta-network/forta-node/clients/messaging"
	"github.com/forta-network/forta-node/config"
	log "github.com/sirupsen/logrus"
)

// Inspector runs continuous inspections.
type Inspector struct {
	ctx context.Context
	cfg InspectorConfig

	msgClient clients.MessageClient

	lastErr health.ErrorTracker

	inspectEvery int
	inspectTrace bool
	inspectCh    chan uint64

	latestBlockEventTime time.Time
}

const blockEventWaitTimeout = time.Minute * 10

type InspectorConfig struct {
	Config    config.Config
	ProxyHost string
	ProxyPort string
}

func (ins *Inspector) Start() error {
	if ins.cfg.Config.LocalModeConfig.Enable && !ins.cfg.Config.LocalModeConfig.EnableInspection {
		log.Warn("inspection is disabled - please enable it from the local mode config if you need it")
		return nil
	}

	ins.registerMessageHandlers()

	go func() {
		err := ins.inspectionTimeoutWorker(ins.ctx)
		if err != nil {
			log.WithFields(
				log.Fields{
					"error": err,
				},
			).Error("inspection timeout ticker failed")
		}
	}()

	go func() {
		for {
			select {
			case <-ins.ctx.Done():
				return
			case blockNum := <-ins.inspectCh:
				inspectBackoff := backoff.NewExponentialBackOff()
				inspectBackoff.InitialInterval = time.Second * 3
				inspectBackoff.MaxInterval = time.Second * 30
				inspectBackoff.MaxElapsedTime = time.Minute * 5
				if err := backoff.Retry(
					func() error {
						ctx, cancel := context.WithTimeout(ins.ctx, time.Minute*2)
						defer cancel()
						return ins.runInspection(ctx, blockNum)
					}, inspectBackoff,
				); err != nil {
					log.WithFields(
						log.Fields{
							"error":             err,
							"inspectingAtBlock": blockNum,
						},
					).Error("finally failed to complete inspection")
				}
			}
		}
	}()
	return nil
}

func (ins *Inspector) inspectionTimeoutWorker(ctx context.Context) error {
	t := time.NewTicker(time.Second)

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			if time.Now().After(ins.latestBlockEventTime.Add(blockEventWaitTimeout)) {
				rpcClient, err := rpc.DialContext(ctx, ins.cfg.Config.Scan.JsonRpc.Url)
				// if scan api is still failing, run a placeholder-like inspection with genesis block
				if err != nil {
					ins.inspectCh <- 0
					continue
				}

				blockNum, err := ethclient.NewClient(rpcClient).BlockNumber(ctx)
				// if scan api is still failing, run a placeholder-like inspection with genesis block
				if err != nil {
					ins.inspectCh <- 0
					continue
				}

				// run inspection with current block, even though scanner didn't publish any blocks for some time
				if blockNum%uint64(ins.inspectEvery) == 0 {
					ins.inspectCh <- blockNum
					continue
				}
			}
		}
	}
}

func (ins *Inspector) runInspection(ctx context.Context, blockNum uint64) error {
	results, err := inspect.Inspect(
		ctx, inspect.InspectionConfig{
			ScanAPIURL:  ins.cfg.Config.Scan.JsonRpc.Url,
			ProxyAPIURL: fmt.Sprintf("http://%s:%s", ins.cfg.ProxyHost, ins.cfg.ProxyPort),
			TraceAPIURL: ins.cfg.Config.Trace.JsonRpc.Url,
			BlockNumber: blockNum,
			CheckTrace:  ins.inspectTrace,
		},
	)
	// publish inspection results even if there are errors, because inspection results are independent from errors
	ins.msgClient.PublishProto(messaging.SubjectInspectionDone, transform.ToProtoInspectionResults(results))

	b, _ := json.Marshal(results)
	log.WithField("results", string(b)).Info("inspection done")
	if err != nil {
		log.WithError(err).Warn("error(s) during inspection")
		return err
	}

	return nil
}

func (ins *Inspector) registerMessageHandlers() {
	ins.msgClient.Subscribe(messaging.SubjectScannerBlock, messaging.ScannerHandler(ins.handleScannerBlock))
}

func (ins *Inspector) handleScannerBlock(payload messaging.ScannerPayload) error {
	ins.latestBlockEventTime = time.Now()

	if payload.LatestBlockInput > 0 && payload.LatestBlockInput%uint64(ins.inspectEvery) == 0 {
		// inspect from N blocks back to avoid synchronizations issues
		inspectionBlockNum := payload.LatestBlockInput - uint64(ins.inspectEvery)
		logger := log.WithFields(
			log.Fields{
				"triggeredAtBlock":  payload.LatestBlockInput,
				"inspectingAtBlock": inspectionBlockNum,
			},
		)
		logger.Info("triggering inspection")
		// non-blocking insert
		select {
		case ins.inspectCh <- inspectionBlockNum:
			logger.Info("successfully triggered new inspection")
		default:
			logger.Info("failed to trigger new inspection: already busy")
		}
	}
	return nil
}

func (ins *Inspector) Stop() error {
	return nil
}

func (ins *Inspector) Name() string {
	return "inspector"
}

// Health implements health.Reporter interface.
func (ins *Inspector) Health() health.Reports {
	return health.Reports{
		ins.lastErr.GetReport("last-error"),
	}
}

func NewInspector(ctx context.Context, cfg InspectorConfig) (*Inspector, error) {
	msgClient := messaging.NewClient("inspector", fmt.Sprintf("%s:%s", config.DockerNatsContainerName, config.DefaultNatsPort))

	chainSettings := config.GetChainSettings(cfg.Config.ChainID)
	inspectionInterval := chainSettings.InspectionInterval
	if cfg.Config.InspectionConfig.BlockInterval != nil {
		inspectionInterval = *cfg.Config.InspectionConfig.BlockInterval
	}

	inspect.DownloadTestSavingMode = cfg.Config.InspectionConfig.NetworkSavingMode

	return &Inspector{
		ctx:          ctx,
		msgClient:    msgClient,
		cfg:          cfg,
		inspectEvery: inspectionInterval,
		inspectTrace: chainSettings.EnableTrace,
		inspectCh:    make(chan uint64, 1), // let it tolerate being late on one block inspection
	}, nil
}
