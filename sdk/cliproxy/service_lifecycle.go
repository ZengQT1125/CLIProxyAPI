package cliproxy

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/api"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/home"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/managementasset"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/redisqueue"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	internalusage "github.com/router-for-me/CLIProxyAPI/v7/internal/usage"
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v7/sdk/access"
	sdkAuth "github.com/router-for-me/CLIProxyAPI/v7/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	log "github.com/sirupsen/logrus"
)

// Run starts the service and blocks until the context is cancelled or the server stops.
// It initializes all components including authentication, file watching, HTTP server,
// and starts processing requests. The method blocks until the context is cancelled.
//
// Parameters:
//   - ctx: The context for controlling the service lifecycle
//
// Returns:
//   - error: An error if the service fails to start or run
func (s *Service) Run(ctx context.Context) error {
	if s == nil {
		return fmt.Errorf("cliproxy: service is nil")
	}
	s.serviceStartedAt = time.Now()
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, runCancel := context.WithCancel(ctx)
	s.homeMu.Lock()
	s.runCancel = runCancel
	s.homeMu.Unlock()
	defer func() {
		runCancel()
		s.homeMu.Lock()
		if s.runCancel != nil {
			s.runCancel = nil
		}
		s.homeMu.Unlock()
	}()

	usage.StartDefault(ctx)
	homeEnabled := s.cfg != nil && s.cfg.Home.Enabled
	if homeEnabled {
		forceHomeRuntimeConfig(s.cfg)
		redisqueue.SetUsageStatisticsEnabled(true)
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer shutdownCancel()
	defer func() {
		if err := s.Shutdown(shutdownCtx); err != nil {
			log.Errorf("service shutdown returned error: %v", err)
		}
	}()
	runCtx, runCancel := context.WithCancel(ctx)
	defer runCancel()

	if !homeEnabled {
		if errEnsureAuthDir := s.ensureAuthDir(); errEnsureAuthDir != nil {
			return errEnsureAuthDir
		}
	}

	if s.cfg != nil {
		if err := internalusage.UpdatePersistence(runCtx, s.cfg.UsagePersistenceEnabled, s.cfg.AuthDir); err != nil {
			if s.cfg.UsagePersistenceEnabled {
				return fmt.Errorf("cliproxy: initialize usage persistence: %w", err)
			}
			log.Warnf("usage database init failed while persistence disabled: %v", err)
		}
	}

	s.applyRetryConfig(s.cfg)
	s.configureCooldownStateStore(s.cfg)

	if s.coreManager != nil && !homeEnabled {
		if !s.progressiveFileAuth {
			startupCtx := coreauth.WithSkipPersist(runCtx)
			if errLoad := s.coreManager.Load(runCtx); errLoad != nil {
				log.Warnf("failed to load auth store: %v", errLoad)
			}
			// Bind provider executors for every auth loaded from the remote token
			// store (PG/object/git). Home mode uses includeBaseline; progressive
			// file load uses prepareCoreAuth per auth update. Non-progressive
			// remote stores only hit Manager.Load — without this, no executor is
			// registered for oauth credentials and every request fails with
			// auth_not_found even though models resolve to the right providers.
			s.registerAvailableExecutors(startupCtx, executorRegistrationOptions{
				auths: s.coreManager.List(),
			})
			s.registerConfigAPIKeyAuths(startupCtx, s.cfg)
			if s.cfg.SaveCooldownStatus {
				if errRestoreCooldown := s.coreManager.RestoreCooldownStates(runCtx); errRestoreCooldown != nil {
					log.Warnf("failed to restore cooldown state: %v", errRestoreCooldown)
				}
			}
		}
	}

	if !homeEnabled {
		tokenResult, err := s.tokenProvider.Load(runCtx, s.cfg)
		if err != nil && !errors.Is(err, context.Canceled) {
			return err
		}
		if tokenResult == nil {
			tokenResult = &TokenClientResult{}
		}

		apiKeyResult, err := s.apiKeyProvider.Load(runCtx, s.cfg)
		if err != nil && !errors.Is(err, context.Canceled) {
			return err
		}
		if apiKeyResult == nil {
			apiKeyResult = &APIKeyClientResult{}
		}
	}

	// legacy clients removed; no caches to refresh

	s.ensureWebsocketGateway()
	if homeEnabled {
		s.registerAvailableExecutors(runCtx, executorRegistrationOptions{
			includeBaseline: true,
		})
		// Home mode does not expose in-process Redis RESP usage output; usage is forwarded to home instead.
		redisqueue.SetEnabled(true)
	}
	if s.hooks.OnBeforeStart != nil {
		s.hooks.OnBeforeStart(s.cfg)
	}

	var watcherWrapper *WatcherWrapper
	if !homeEnabled {
		var errWatcher error
		watcherWrapper, errWatcher = s.setupFileWatcher(runCtx)
		if errWatcher != nil {
			return fmt.Errorf("cliproxy: %w", errWatcher)
		}
	}

	serverOptions := append([]api.ServerOption(nil), s.serverOptions...)
	if watcherWrapper != nil {
		serverOptions = append(serverOptions,
			api.WithAuthLoadStatusProvider(watcherWrapper.AuthLoadStatus),
			api.WithAuthFileMutationHook(watcherWrapper.MarkAuthPathChanged),
		)
	}
	// handlers no longer depend on legacy clients; pass nil slice initially
	s.server = api.NewServer(s.cfg, s.coreManager, s.accessManager, s.configPath, serverOptions...)
	s.syncPluginRuntimeConfig(runCtx)
	if homeEnabled {
		s.syncPluginModelRuntime(runCtx)
	}

	if s.authManager == nil {
		s.authManager = newDefaultAuthManager()
	}

	if homeEnabled {
		s.startHomeSubscriber(runCtx)
	}

	if s.server != nil && s.wsGateway != nil {
		s.server.AttachWebsocketRoute(s.wsGateway.Path(), s.wsGateway.Handler())
		s.server.SetWebsocketAuthChangeHandler(func(oldEnabled, newEnabled bool) {
			if oldEnabled == newEnabled {
				return
			}
			if !oldEnabled && newEnabled {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if errStop := s.wsGateway.Stop(ctx); errStop != nil {
					log.Warnf("failed to reset websocket connections after ws-auth change %t -> %t: %v", oldEnabled, newEnabled, errStop)
					return
				}
				log.Debugf("ws-auth enabled; existing websocket sessions terminated to enforce authentication")
				return
			}
			log.Debugf("ws-auth disabled; existing websocket sessions remain connected")
		})
	}

	s.serverErr = make(chan error, 1)
	go func() {
		if errStart := s.server.Start(); errStart != nil {
			s.serverErr <- errStart
		} else {
			s.serverErr <- nil
		}
	}()

	select {
	case addr, ok := <-s.server.Listening():
		if !ok || addr == nil {
			return fmt.Errorf("cliproxy: server stopped before listener became ready")
		}
		s.listenerReadyAt = time.Now()
		log.WithField("address", addr.String()).Info("API server listener ready")
	case errServer := <-s.serverErr:
		return errServer
	case <-runCtx.Done():
		return runCtx.Err()
	}
	if !homeEnabled {
		managementasset.StartAutoUpdater(runCtx, s.configPath)
	}

	if s.progressiveFileAuth && watcherWrapper != nil {
		watcherWrapper.StartInitialAuthLoad(runCtx, s.cfg.AuthLoadWorkers)
	} else if s.coreManager != nil && !homeEnabled {
		s.registerModelsForAuthBatch(runCtx, s.coreManager.List())
		s.startCoreAuthAutoRefresh(runCtx)
	}

	fmt.Printf("API server started successfully on: %s:%d\n", s.cfg.Host, s.cfg.Port)

	s.applyPprofConfig(s.cfg)

	if s.hooks.OnAfterStart != nil {
		s.hooks.OnAfterStart(s)
	}

	s.registerModelRefreshCallback()

	select {
	case <-runCtx.Done():
		log.Debug("service context cancelled, shutting down...")
		return runCtx.Err()
	case errServer := <-s.serverErr:
		return errServer
	}
}

// Shutdown gracefully stops background workers and the HTTP server.
// It ensures all resources are properly cleaned up and connections are closed.
// The shutdown is idempotent and can be called multiple times safely.
//
// Parameters:
//   - ctx: The context for controlling the shutdown timeout
//
// Returns:
//   - error: An error if shutdown fails
func (s *Service) Shutdown(ctx context.Context) error {
	if s == nil {
		return nil
	}
	var shutdownErr error
	s.shutdownOnce.Do(func() {
		if ctx == nil {
			ctx = context.Background()
		}

		s.homeLifecycleMu.Lock()
		if supervisor := s.homeSupervisor; supervisor != nil {
			s.homeConfigCommitMu.Lock()
			supervisor.cancel()
			s.homeConfigCommitMu.Unlock()
			<-supervisor.done
		}
		s.homeMu.Lock()
		homeCancel := s.homeCancel
		homeClient := s.homeClient
		homeRegistry := s.homeRegistry
		homeDispatchBundle := s.homeDispatchBundle
		homeForwarder := s.homeLogForwarder
		homeForwarderClient := s.homeLogForwarderClient
		s.homeGeneration++
		s.homeCancel = nil
		s.homeClient = nil
		s.homeRegistry = nil
		s.homeDispatchBundle = nil
		s.homeDrainBound = 0
		s.homeLogForwarder = nil
		s.homeLogForwarderClient = nil
		s.homeMu.Unlock()
		if s.coreManager != nil {
			s.coreManager.ClearHomeDispatchBundle(homeDispatchBundle)
		}
		home.ClearCurrentIf(homeClient)
		if homeCancel != nil {
			homeCancel()
		}
		if homeRegistry != nil {
			if errClose := homeRegistry.Close(); errClose != nil {
				log.WithError(errClose).Warn("failed to close Home execution registry during shutdown")
			}
		}
		if homeClient != nil {
			homeClient.Close()
		}
		if homeForwarder != nil {
			if homeForwarderClient == homeClient {
				homeForwarder.Deactivate(homeClient)
			}
			homeForwarder.Stop()
		}
		s.homeLifecycleMu.Unlock()

		// legacy refresh loop removed; only stopping core auth manager below

		if s.watcherCancel != nil {
			s.watcherCancel()
		}
		if s.coreManager != nil {
			s.coreManager.StopAutoRefresh()
		}
		if s.watcher != nil {
			if err := s.watcher.Stop(); err != nil {
				log.Errorf("failed to stop file watcher: %v", err)
				shutdownErr = err
			}
		}
		if s.wsGateway != nil {
			if err := s.wsGateway.Stop(ctx); err != nil {
				log.Errorf("failed to stop websocket gateway: %v", err)
				if shutdownErr == nil {
					shutdownErr = err
				}
			}
		}
		if s.authQueueStop != nil {
			s.authQueueStop()
			s.authQueueStop = nil
		}

		if errShutdownPprof := s.shutdownPprof(ctx); errShutdownPprof != nil {
			log.Errorf("failed to stop pprof server: %v", errShutdownPprof)
			if shutdownErr == nil {
				shutdownErr = errShutdownPprof
			}
		}

		// no legacy clients to persist

		if s.server != nil {
			shutdownCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			if err := s.server.Stop(shutdownCtx); err != nil {
				log.Errorf("error stopping API server: %v", err)
				if shutdownErr == nil {
					shutdownErr = err
				}
			}
		}
		if s.coreManager != nil {
			if errFlush := s.coreManager.FlushCooldownStates(ctx); errFlush != nil {
				log.Errorf("failed to flush cooldown state: %v", errFlush)
				if shutdownErr == nil {
					shutdownErr = errFlush
				}
			}
		}

		if s.pluginHost != nil {
			sdktranslator.SetPluginHooks(nil)
			sdkAuth.RegisterPluginAuthParser(nil)
			if s.watcher != nil {
				s.watcher.SetPluginAuthParser(nil)
			}
			s.pluginHost.ApplyConfig(ctx, &config.Config{})
			s.pluginHost.RegisterModels(ctx, registry.GetGlobalRegistry())
			s.registerAvailableExecutors(ctx, executorRegistrationOptions{
				includePlugins: true,
			})
			s.pluginHost.RegisterFrontendAuthProviders()
			s.pluginHost.ShutdownAllContext(ctx)
			if s.accessManager != nil {
				s.accessManager.SetProviders(sdkaccess.RegisteredProviders())
			}
		}

		internalusage.CloseDatabasePlugin()
		usage.StopDefault()
	})
	return shutdownErr
}

func (s *Service) ensureAuthDir() error {
	info, err := os.Stat(s.cfg.AuthDir)
	if err != nil {
		if os.IsNotExist(err) {
			if mkErr := os.MkdirAll(s.cfg.AuthDir, 0o755); mkErr != nil {
				return fmt.Errorf("cliproxy: failed to create auth directory %s: %w", s.cfg.AuthDir, mkErr)
			}
			log.Infof("created missing auth directory: %s", s.cfg.AuthDir)
			return nil
		}
		return fmt.Errorf("cliproxy: error checking auth directory %s: %w", s.cfg.AuthDir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("cliproxy: auth path exists but is not a directory: %s", s.cfg.AuthDir)
	}
	return nil
}
