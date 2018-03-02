package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"code.cloudfoundry.org/lager"
	"github.com/alphagov/paas-billing/auth"
	"github.com/alphagov/paas-billing/cloudfoundry"
	"github.com/alphagov/paas-billing/collector"
	collector_cf "github.com/alphagov/paas-billing/collector/cloudfoundry"
	"github.com/alphagov/paas-billing/db"
	"github.com/alphagov/paas-billing/server"
	"github.com/pkg/errors"
)

var (
	logger = createLogger()
)

func createLogger() lager.Logger {
	logger := lager.NewLogger("paas-billing")
	logLevel := lager.INFO
	if strings.ToLower(os.Getenv("LOG_LEVEL")) == "debug" {
		logLevel = lager.DEBUG
	}
	logger.RegisterSink(lager.NewWriterSink(os.Stdout, logLevel))

	return logger
}

func createCFClient() (cloudfoundry.Client, error) {
	config := cloudfoundry.CreateConfigFromEnv()
	return cloudfoundry.NewClient(config)
}

func Main() error {

	sqlClient, err := db.NewPostgresClient(os.Getenv("DATABASE_URL"))
	if err != nil {
		return err
	}

	if err := sqlClient.InitSchema(); err != nil {
		return errors.Wrap(err, "failed to initialise database schema")
	}

	cfClient, clientErr := createCFClient()
	if clientErr != nil {
		return errors.Wrap(clientErr, "failed to connect to Cloud Foundry")
	}

	collectorConfig, err := collector.CreateConfigFromEnv()
	if err != nil {
		return errors.Wrap(err, "configuration error")
	}

	uaaConfig, err := auth.CreateConfigFromEnv()
	if err != nil {
		return err
	}
	apiAuthenticator := &auth.UAA{uaaConfig}

	ctx, shutdown := context.WithCancel(context.Background())
	go func() {
		signalChan := make(chan os.Signal, 1)
		signal.Notify(signalChan, syscall.SIGINT, syscall.SIGTERM)
		defer signal.Reset(syscall.SIGINT, syscall.SIGTERM)
		<-signalChan
		shutdown()
	}()

	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		defer shutdown()

		appUsageEventsCollector := collector.New(
			collectorConfig,
			logger,
			collector_cf.NewEventFetcher(
				sqlClient,
				cloudfoundry.NewAppUsageEventsAPI(cfClient, logger),
			),
		)
		appUsageEventsCollector.Run(ctx)
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		defer shutdown()

		serviceUsageEventsCollector := collector.New(
			collectorConfig,
			logger,
			collector_cf.NewEventFetcher(
				sqlClient,
				cloudfoundry.NewServiceUsageEventsAPI(cfClient, logger),
			),
		)
		serviceUsageEventsCollector.Run(ctx)
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		defer logger.Info("stopped view updater")
		logger.Info("starting view updater")
		for {
			logger.Info("updating views")
			if err := sqlClient.UpdateViews(); err != nil {
				logger.Error("update-views", err)
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(1 * time.Hour):
			}
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		defer logger.Info("stopped api server")
		logger.Info("starting api server")
		s := server.New(sqlClient, apiAuthenticator, cfClient)
		port := os.Getenv("PORT")
		if port == "" {
			port = "8881"
		}
		server.ListenAndServe(ctx, s, fmt.Sprintf(":%s", port))
	}()

	wg.Wait()
	return nil
}

func main() {
	if err := Main(); err != nil {
		logger.Error("main", err)
		os.Exit(1)
	}
	logger.Info("shutdown")
}
