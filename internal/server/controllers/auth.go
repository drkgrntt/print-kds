package controllers

import (
	"errors"
	"strings"

	"print-kds/internal/auth"
	"print-kds/internal/logger"
	"print-kds/internal/server/middleware"
	"print-kds/internal/views"

	"github.com/gofiber/fiber/v2"
)

// minPasswordLength matches the <input minlength="8"> on the login/signup
// forms - enforced again server-side since a client can bypass the HTML
// attribute.
const minPasswordLength = 8

type AuthController struct {
	store *auth.Store
}

func NewAuthController(store *auth.Store) *AuthController {
	return &AuthController{store: store}
}

// RegisterRoutes registers the only routes reachable without a session -
// everything else goes through middleware.RequireAuth in routes.go.
func (a *AuthController) RegisterRoutes(app *fiber.App) {
	app.Get("/login", a.handleLoginForm)
	app.Post("/login", a.handleLogin)
	app.Get("/signup", a.handleSignupForm)
	app.Post("/signup", a.handleSignup)
	app.Post("/logout", a.handleLogout)
}

func (a *AuthController) handleLoginForm(c *fiber.Ctx) error {
	return render(c, views.Login(views.LoginFormData{}))
}

func (a *AuthController) handleLogin(c *fiber.Ctx) error {
	email := strings.TrimSpace(c.FormValue("email"))
	password := c.FormValue("password")

	user, err := a.store.Authenticate(c.Context(), email, password)
	if err != nil {
		if errors.Is(err, auth.ErrInvalidCredentials) {
			return render(c, views.Login(views.LoginFormData{Email: email, Error: "Invalid email or password."}))
		}
		logger.Error("authenticate", "error", err)
		return render(c, views.Login(views.LoginFormData{Email: email, Error: "Something went wrong logging in - try again."}))
	}

	if err := a.startSession(c, user); err != nil {
		logger.Error("create session", "error", err)
		return render(c, views.Login(views.LoginFormData{Email: email, Error: "Something went wrong logging in - try again."}))
	}

	return c.Redirect("/", fiber.StatusSeeOther)
}

func (a *AuthController) handleSignupForm(c *fiber.Ctx) error {
	return render(c, views.Signup(views.SignupFormData{}))
}

func (a *AuthController) handleSignup(c *fiber.Ctx) error {
	shopName := strings.TrimSpace(c.FormValue("shop_name"))
	email := strings.TrimSpace(c.FormValue("email"))
	password := c.FormValue("password")

	if msg := validateSignup(shopName, email, password); msg != "" {
		return render(c, views.Signup(views.SignupFormData{ShopName: shopName, Email: email, Error: msg}))
	}

	user, err := a.store.SignUp(c.Context(), shopName, email, password)
	if err != nil {
		if errors.Is(err, auth.ErrDuplicateEmail) {
			return render(c, views.Signup(views.SignupFormData{ShopName: shopName, Email: email, Error: "That email is already registered - log in instead."}))
		}
		logger.Error("sign up", "error", err)
		return render(c, views.Signup(views.SignupFormData{ShopName: shopName, Email: email, Error: "Something went wrong creating your shop - try again."}))
	}

	if err := a.startSession(c, user); err != nil {
		logger.Error("create session", "error", err)
		return render(c, views.Signup(views.SignupFormData{ShopName: shopName, Email: email, Error: "Something went wrong creating your shop - try again."}))
	}

	return c.Redirect("/", fiber.StatusSeeOther)
}

func validateSignup(shopName, email, password string) string {
	switch {
	case shopName == "":
		return "Shop name is required."
	case email == "":
		return "Email is required."
	case len(password) < minPasswordLength:
		return "Password must be at least 8 characters."
	}
	return ""
}

// handleLogout is POST-only (see RegisterRoutes) so a plain <a>/prefetch
// can't trigger it. Deleting an already-gone session is a no-op, so a
// missing/stale cookie doesn't turn this into an error path.
func (a *AuthController) handleLogout(c *fiber.Ctx) error {
	if token := c.Cookies(middleware.SessionCookieName); token != "" {
		if err := a.store.DeleteSession(c.Context(), token); err != nil {
			logger.Error("delete session", "error", err)
		}
	}
	c.ClearCookie(middleware.SessionCookieName)
	return c.Redirect("/login", fiber.StatusSeeOther)
}

// startSession creates a session row and sets it as an HttpOnly cookie.
// Secure is hardcoded false - this app is meant to run self-hosted, often
// without TLS in front of it; flip it to true once/if this runs behind a
// TLS-terminating reverse proxy.
func (a *AuthController) startSession(c *fiber.Ctx, user auth.User) error {
	token, expiresAt, err := a.store.CreateSession(c.Context(), user)
	if err != nil {
		return err
	}
	c.Cookie(&fiber.Cookie{
		Name:     middleware.SessionCookieName,
		Value:    token,
		Path:     "/",
		Expires:  expiresAt,
		HTTPOnly: true,
		SameSite: fiber.CookieSameSiteLaxMode,
		Secure:   false,
	})
	return nil
}
