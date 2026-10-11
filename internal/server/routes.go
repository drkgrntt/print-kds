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
	// Auth routes (login/signup/logout) are public - registered before
	// RequireAuth below so they're reachable without a session.
	controllers.NewAuthController(s.authStore).RegisterRoutes(s.App)

	s.App.Use(middleware.RequireAuth(s.authStore))

	controllers.NewBoardController(s.boardStore).RegisterRoutes(s.App)
	// NewOrderController and UsersController each gate their own routes
	// with middleware.RequireAdmin - order intake and user management are
	// admin-only, the board itself is open to any logged-in user.
	controllers.NewNewOrderController(s.boardStore).RegisterRoutes(s.App)
	controllers.NewUsersController(s.authStore, s.boardStore).RegisterRoutes(s.App)
	controllers.NewCustomersController(s.boardStore).RegisterRoutes(s.App)
	controllers.NewInvoicesController(s.boardStore).RegisterRoutes(s.App)
	// The client portal gates its own routes with middleware.RequireClient;
	// /files/:id (staff download) with RequireStaff.
	controllers.NewPortalController(s.boardStore, s.uploadDir).RegisterRoutes(s.App)

	logger.Info("routes registered")
}
