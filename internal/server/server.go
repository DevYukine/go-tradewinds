package server

import (
	"context"
	"fmt"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/cors"
	fiberlogger "github.com/gofiber/fiber/v3/middleware/logger"
	"go.uber.org/fx"
	"go.uber.org/zap"
	"gorm.io/gorm"

	"github.com/DevYukine/go-tradewinds/internal/bot"
	"github.com/DevYukine/go-tradewinds/internal/config"
)

// Module provides the Fiber HTTP server to the fx DI container.
var Module = fx.Module("server",
	fx.Provide(NewServer),
	fx.Invoke(RegisterServer),
)

// Server holds the Fiber app and its dependencies.
type Server struct {
	app       *fiber.App
	cfg       *config.Config
	logger    *zap.Logger
	db        *gorm.DB
	manager   *bot.Manager
	startedAt time.Time
}

// NewServer creates a new Fiber HTTP server with middleware and routes configured.
func NewServer(
	cfg *config.Config,
	logger *zap.Logger,
	gormDB *gorm.DB,
	manager *bot.Manager,
) *Server {
	app := fiber.New(fiber.Config{
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  120 * time.Second,
	})

	// Middleware.
	app.Use(cors.New(cors.Config{
		AllowOrigins: []string{"*"},
		AllowMethods: []string{"GET"},
		AllowHeaders: []string{"Content-Type"},
	}))
	app.Use(fiberlogger.New(fiberlogger.Config{
		Format:     "${time} ${status} ${method} ${path} ${latency}\n",
		TimeFormat: "15:04:05",
	}))

	s := &Server{
		app:       app,
		cfg:       cfg,
		logger:    logger.Named("server"),
		db:        gormDB,
		manager:   manager,
		startedAt: time.Now(),
	}

	// Register routes.
	s.registerHandlers()
	s.registerSSE()

	return s
}

// RegisterServer hooks the server into the fx lifecycle.
func RegisterServer(lc fx.Lifecycle, s *Server) {
	lc.Append(fx.Hook{
		OnStart: func(_ context.Context) error {
			addr := fmt.Sprintf(":%d", s.cfg.APIPort)
			s.logger.Info("starting API server", zap.String("addr", addr))
			go func() {
				if err := s.app.Listen(addr, fiber.ListenConfig{
					DisableStartupMessage: true,
				}); err != nil {
					s.logger.Error("API server error", zap.Error(err))
				}
			}()
			return nil
		},
		OnStop: func(_ context.Context) error {
			s.logger.Info("shutting down API server")
			return s.app.Shutdown()
		},
	})
}
