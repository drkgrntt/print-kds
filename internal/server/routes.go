package server

import (
	"print-kds/internal/logger"
	"print-kds/internal/server/controllers"
	"print-kds/internal/server/middleware"
)

func (s *FiberServer) RegisterFiberRoutes() {
	// RequestID first (nothing after it needs to run without an id to tag
	// its own logs with), then Recover right behind it, so it wraps every
	// handler registered below.
	s.App.Use(middleware.RequestID())
	s.App.Use(middleware.Recover())

	s.App.Static("/static", "./web/static")

	controllers.NewHealthController(s.db).RegisterRoutes(s.App)
	controllers.NewBoardController(s.boardStore).RegisterRoutes(s.App)
	controllers.NewNewOrderController(s.boardStore).RegisterRoutes(s.App)

	logger.Info("routes registered")
}
