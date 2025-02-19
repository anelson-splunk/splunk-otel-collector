// Copyright Splunk, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package discoveryreceiver

import (
	"context"
	"fmt"
	"sync"

	"github.com/open-telemetry/opentelemetry-collector-contrib/extension/observer"
	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/config"
	"go.opentelemetry.io/collector/config/configtelemetry"
	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/obsreport"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
)

const (
	eventTypeAttr      = "discovery.event.type"
	observerNameAttr   = "discovery.observer.name"
	observerTypeAttr   = "discovery.observer.type"
	receiverConfigAttr = "discovery.receiver.config"
	receiverRuleAttr   = "discovery.receiver.rule"
)

var (
	_ component.LogsReceiver = (*discoveryReceiver)(nil)
)

type discoveryReceiver struct {
	logsConsumer      consumer.Logs
	receiverCreator   component.MetricsReceiver
	alreadyLogged     *sync.Map
	endpointTracker   *endpointTracker
	sentinel          chan struct{}
	metricEvaluator   *metricEvaluator
	logger            *zap.Logger
	config            *Config
	obsreportReceiver *obsreport.Receiver
	pLogs             chan plog.Logs
	observables       map[config.ComponentID]observer.Observable
	loopFinished      *sync.WaitGroup
	settings          component.ReceiverCreateSettings
}

func newDiscoveryReceiver(
	settings component.ReceiverCreateSettings,
	config *Config,
	consumer consumer.Logs,
) *discoveryReceiver {
	d := &discoveryReceiver{
		config: config,
		obsreportReceiver: obsreport.NewReceiver(obsreport.ReceiverSettings{
			ReceiverID:             config.ID(),
			Transport:              "none",
			ReceiverCreateSettings: settings,
		}),
		logger:        settings.TelemetrySettings.Logger,
		settings:      settings,
		logsConsumer:  consumer,
		pLogs:         make(chan plog.Logs),
		sentinel:      make(chan struct{}, 1),
		loopFinished:  &sync.WaitGroup{},
		alreadyLogged: &sync.Map{},
	}

	return d
}

func (d *discoveryReceiver) Start(ctx context.Context, host component.Host) (err error) {
	if d.observables, err = d.observablesFromHost(host); err != nil {
		return fmt.Errorf("failed obtaining observables from host: %w", err)
	}

	d.endpointTracker = newEndpointTracker(d.observables, d.config, d.logger, d.pLogs)
	d.endpointTracker.start()

	d.metricEvaluator = newMetricEvaluator()

	if err = d.createAndSetReceiverCreator(); err != nil {
		return fmt.Errorf("failed creating internal receiver_creator: %w", err)
	}

	loopStarted := &sync.WaitGroup{}
	loopStarted.Add(1)
	d.loopFinished.Add(1)
	go d.consumerLoop(loopStarted)
	// wait until we know consumer loop is running before starting receiver creator
	// so as not to miss any resulting telemetry
	d.logger.Debug("log consumer initializing")
	loopStarted.Wait()
	d.logger.Debug("successfully initialized")

	if err = d.receiverCreator.Start(ctx, host); err != nil {
		return fmt.Errorf("failed starting internal receiver_creator: %w", err)
	}
	d.logger.Debug("started receiver_creator receiver")
	return
}

func (d *discoveryReceiver) Shutdown(ctx context.Context) error {
	d.endpointTracker.stop()
	defer func() {
		d.logger.Debug("discovery receiver shutting down")
		d.sentinel <- struct{}{}
		d.loopFinished.Wait()
		close(d.sentinel)
		close(d.pLogs)
		d.logger.Debug("finished shutdown")
	}()

	if err := d.receiverCreator.Shutdown(ctx); err != nil {
		return fmt.Errorf("failed shutting down internal receiver_creator: %w", err)
	}

	return nil
}

func (d *discoveryReceiver) consumerLoop(loopStarted *sync.WaitGroup) {
	loopStarted.Done()
	defer d.loopFinished.Done()
	for {
		select {
		case <-d.sentinel:
			d.logger.Debug("halting consumer loop.")
			return
		case pLog, ok := <-d.pLogs:
			if !ok {
				return
			}
			ctx := d.obsreportReceiver.StartLogsOp(context.Background())
			err := d.logsConsumer.ConsumeLogs(context.Background(), pLog)
			if err != nil {
				d.logger.Info("logsConsumer failed consumption", zap.Error(err))
			}
			d.obsreportReceiver.EndLogsOp(ctx, typeStr, pLog.LogRecordCount(), err)
		}
	}
}

func (d *discoveryReceiver) createAndSetReceiverCreator() error {
	receiverCreatorFactory, receiverCreatorConfig, err := d.config.receiverCreatorFactoryAndConfig()
	if err != nil {
		return nil
	}
	receiverCreatorSettings := component.ReceiverCreateSettings{
		TelemetrySettings: component.TelemetrySettings{
			Logger: d.logger.With(
				zap.String("kind", "receiver"),
				zap.String("name", receiverCreatorConfig.ID().String()),
			),
			TracerProvider: trace.NewNoopTracerProvider(),
			MeterProvider:  metric.NewNoopMeterProvider(),
			MetricsLevel:   configtelemetry.LevelDetailed,
		},
		BuildInfo: component.BuildInfo{
			Command: "discovery",
			Version: "latest",
		},
	}
	if d.receiverCreator, err = receiverCreatorFactory.CreateMetricsReceiver(
		context.Background(), receiverCreatorSettings, receiverCreatorConfig, d.metricEvaluator,
	); err != nil {
		return err
	}
	return nil
}

// observablesFromHost finds configured `watch_observers` extension instances from the host
// by their ComponentID. It is based on the equivalent logic in the Receiver Creator:
// https://github.com/open-telemetry/opentelemetry-collector-contrib/blob/d6042eda45ec9d8a5df1ae553388eaca67d9d16c/receiver/receivercreator/receiver.go#L79
func (d *discoveryReceiver) observablesFromHost(host component.Host) (map[config.ComponentID]observer.Observable, error) {
	watchObservables := map[config.ComponentID]observer.Observable{}
	for _, obs := range d.config.WatchObservers {
		for cid, ext := range host.GetExtensions() {
			if cid != obs {
				continue
			}

			observable, ok := ext.(observer.Observable)
			if !ok {
				return nil, fmt.Errorf("extension %q in watch_observers is not an observer", obs.String())
			}
			watchObservables[obs] = observable
		}
	}

	// Make sure all specified watch_observers are present
	for _, obs := range d.config.WatchObservers {
		if watchObservables[obs] == nil {
			return nil, fmt.Errorf("failed to find observer %q as a configured extension", obs)
		}
	}
	if len(watchObservables) == 0 {
		d.logger.Warn("no observers were configured so discoveryreceiver will be inactive")
	}

	return watchObservables, nil
}
