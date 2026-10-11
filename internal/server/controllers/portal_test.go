package controllers

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"print-kds/internal/auth"
	"print-kds/internal/board"
	"print-kds/internal/database"
	"print-kds/internal/server/middleware"

	"github.com/gofiber/fiber/v2"
	"github.com/joho/godotenv"
)

func init() {
	_ = godotenv.Load("../../../.env")
}

type portalEnv struct {
	app         *fiber.App
	board       *board.Store
	clientToken string
	staffToken  string
	otherToken  string // a client of a different customer
	customerID  int64
	tenantID    int64
	uploadDir   string
}

func newPortalEnv(t *testing.T) *portalEnv {
	t.Helper()
	ctx := context.Background()
	db := database.New().DB()
	as := auth.NewStore(db)
	bs := board.NewStore(db)

	suffix := time.Now().UnixNano()
	admin, err := as.SignUp(ctx, fmt.Sprintf("Portal Shop %d", suffix), fmt.Sprintf("admin-%d@example.com", suffix), "password123")
	if err != nil {
		t.Fatalf("SignUp: %v", err)
	}
	t.Cleanup(func() { db.ExecContext(ctx, `DELETE FROM tenants WHERE id = $1`, admin.TenantID) })

	acme, _ := bs.CreateCustomer(ctx, admin.TenantID, board.Customer{Name: "Acme"})
	globex, _ := bs.CreateCustomer(ctx, admin.TenantID, board.Customer{Name: "Globex"})
	client, err := as.CreateClientUser(ctx, admin.TenantID, acme.ID, fmt.Sprintf("acme-%d@example.com", suffix), "password123")
	if err != nil {
		t.Fatalf("CreateClientUser: %v", err)
	}
	other, err := as.CreateClientUser(ctx, admin.TenantID, globex.ID, fmt.Sprintf("globex-%d@example.com", suffix), "password123")
	if err != nil {
		t.Fatalf("CreateClientUser: %v", err)
	}
	token := func(u auth.User) string {
		tok, _, err := as.CreateSession(ctx, u)
		if err != nil {
			t.Fatalf("CreateSession: %v", err)
		}
		return tok
	}

	env := &portalEnv{
		board:       bs,
		clientToken: token(client),
		staffToken:  token(admin),
		otherToken:  token(other),
		customerID:  acme.ID,
		tenantID:    admin.TenantID,
		uploadDir:   t.TempDir(),
	}
	app := fiber.New()
	app.Use(middleware.RequireAuth(as))
	NewBoardController(bs).RegisterRoutes(app)
	NewNewOrderController(bs).RegisterRoutes(app)
	NewPortalController(bs, env.uploadDir).RegisterRoutes(app)
	NewInvoicesController(bs).RegisterRoutes(app)
	env.app = app
	return env
}

func (e *portalEnv) do(t *testing.T, req *http.Request, token string) *http.Response {
	t.Helper()
	req.AddCookie(&http.Cookie{Name: middleware.SessionCookieName, Value: token})
	resp, err := e.app.Test(req, -1)
	if err != nil {
		t.Fatalf("request %s %s: %v", req.Method, req.URL.Path, err)
	}
	return resp
}

func TestRoleGating(t *testing.T) {
	e := newPortalEnv(t)

	cases := []struct {
		name, method, path, token string
		want                      int
	}{
		{"client board redirects to portal", "GET", "/", e.clientToken, http.StatusSeeOther},
		{"client cannot transition", "POST", "/print-items/1/pop", e.clientToken, http.StatusForbidden},
		{"client cannot cancel", "POST", "/print-items/1/cancel", e.clientToken, http.StatusForbidden},
		{"client cannot admin intake", "GET", "/admin/new-order", e.clientToken, http.StatusForbidden},
		{"staff cannot use portal", "GET", "/portal", e.staffToken, http.StatusForbidden},
		{"client portal ok", "GET", "/portal", e.clientToken, http.StatusOK},
		{"staff board ok", "GET", "/", e.staffToken, http.StatusOK},
		{"client cannot see invoices", "GET", "/admin/invoices", e.clientToken, http.StatusForbidden},
		{"client cannot create invoices", "POST", "/admin/invoices", e.clientToken, http.StatusForbidden},
		{"client cannot price items", "POST", "/admin/invoices/items/1/price", e.clientToken, http.StatusForbidden},
		{"admin invoices ok", "GET", "/admin/invoices", e.staffToken, http.StatusOK},
		{"admin preview ok", "GET", "/admin/invoices?from=2020-01-01&to=2020-01-14", e.staffToken, http.StatusOK},
		{"admin bad range still renders", "GET", "/admin/invoices?from=2020-02-01&to=2020-01-01", e.staffToken, http.StatusOK},
		{"unknown invoice 404s", "GET", "/admin/invoices/999999", e.staffToken, http.StatusNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := e.do(t, httptest.NewRequest(tc.method, tc.path, nil), tc.token)
			if resp.StatusCode != tc.want {
				t.Errorf("%s %s = %d, want %d", tc.method, tc.path, resp.StatusCode, tc.want)
			}
		})
	}
}

func multipartOrder(t *testing.T, filename string, content string) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	w.WriteField("due_date", time.Now().Add(72*time.Hour).Format(dueDateLayout))
	w.WriteField("customer_id", "999999") // must be ignored: customer comes from the session
	w.WriteField("ship_name", "Alice Nguyen")
	w.WriteField("ship_line1", "1 Main St")
	w.WriteField("ship_city", "Austin")
	w.WriteField("ship_state", "TX")
	w.WriteField("ship_postal_code", "78701")
	w.WriteField("file_name", "part.stl")
	w.WriteField("color_material", "Blue PLA")
	w.WriteField("quantity_required", "3")
	fw, _ := w.CreateFormFile("files", filename)
	fw.Write([]byte(content))
	w.Close()
	return &buf, w.FormDataContentType()
}

func TestPortalOrderAndFileAccess(t *testing.T) {
	e := newPortalEnv(t)
	ctx := context.Background()

	body, ct := multipartOrder(t, "bracket.stl", "solid x")
	req := httptest.NewRequest("POST", "/portal/new-order", body)
	req.Header.Set("Content-Type", ct)
	resp := e.do(t, req, e.clientToken)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("place order = %d, want 303", resp.StatusCode)
	}

	orders, err := e.board.ListCustomerOrders(ctx, e.tenantID, e.customerID)
	if err != nil || len(orders) != 1 || orders[0].FileCount != 1 || orders[0].ItemCount != 1 {
		t.Fatalf("orders = %+v, %v; want 1 order with 1 file/1 item for the session's customer", orders, err)
	}
	files, _ := e.board.ListOrderFiles(ctx, e.tenantID, e.customerID, orders[0].ID)
	if len(files) != 1 {
		t.Fatalf("files = %v", files)
	}
	fileURL := fmt.Sprintf("/portal/files/%d", files[0].ID)

	if r := e.do(t, httptest.NewRequest("GET", fileURL, nil), e.clientToken); r.StatusCode != http.StatusOK {
		t.Errorf("owner download = %d, want 200", r.StatusCode)
	}
	if r := e.do(t, httptest.NewRequest("GET", fileURL, nil), e.otherToken); r.StatusCode != http.StatusNotFound {
		t.Errorf("other customer download = %d, want 404", r.StatusCode)
	}
	if r := e.do(t, httptest.NewRequest("GET", fmt.Sprintf("/files/%d", files[0].ID), nil), e.staffToken); r.StatusCode != http.StatusOK {
		t.Errorf("staff download = %d, want 200", r.StatusCode)
	}
	if r := e.do(t, httptest.NewRequest("GET", fmt.Sprintf("/files/%d", files[0].ID), nil), e.clientToken); r.StatusCode == http.StatusOK {
		t.Errorf("client hit staff download route: %d", r.StatusCode)
	}
}

func TestPortalRejectsBadUpload(t *testing.T) {
	e := newPortalEnv(t)

	body, ct := multipartOrder(t, "evil.exe", "MZ")
	req := httptest.NewRequest("POST", "/portal/new-order", body)
	req.Header.Set("Content-Type", ct)
	resp := e.do(t, req, e.clientToken)
	if resp.StatusCode != http.StatusOK { // re-rendered form with an error banner
		t.Fatalf("bad upload = %d, want 200 (form re-render)", resp.StatusCode)
	}
	orders, _ := e.board.ListCustomerOrders(context.Background(), e.tenantID, e.customerID)
	if len(orders) != 0 {
		t.Errorf("order created despite bad upload: %+v", orders)
	}
}

func TestPortalRequiresShipTo(t *testing.T) {
	e := newPortalEnv(t)

	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	w.WriteField("due_date", time.Now().Add(72*time.Hour).Format(dueDateLayout))
	w.WriteField("file_name", "part.stl")
	w.WriteField("quantity_required", "1")
	w.Close()
	req := httptest.NewRequest("POST", "/portal/new-order", &buf)
	req.Header.Set("Content-Type", w.FormDataContentType())
	if resp := e.do(t, req, e.clientToken); resp.StatusCode != http.StatusOK {
		t.Fatalf("missing ship-to = %d, want 200 (form re-render)", resp.StatusCode)
	}
	if orders, _ := e.board.ListCustomerOrders(context.Background(), e.tenantID, e.customerID); len(orders) != 0 {
		t.Errorf("order created without a ship-to address: %+v", orders)
	}
}

func TestShipToPrefill(t *testing.T) {
	e := newPortalEnv(t)

	// No prior order: the portal form has no address to prefill.
	resp := e.do(t, httptest.NewRequest("GET", "/portal/new-order", nil), e.clientToken)
	if b := readBody(t, resp); strings.Contains(b, "1 Main St") {
		t.Fatal("prefilled an address before any order existed")
	}

	body, ct := multipartOrder(t, "a.stl", "solid x")
	req := httptest.NewRequest("POST", "/portal/new-order", body)
	req.Header.Set("Content-Type", ct)
	if r := e.do(t, req, e.clientToken); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("place order = %d", r.StatusCode)
	}

	// Client's form now prefills from that order.
	resp = e.do(t, httptest.NewRequest("GET", "/portal/new-order", nil), e.clientToken)
	if b := readBody(t, resp); !strings.Contains(b, "1 Main St") {
		t.Error("portal form not prefilled from the last order")
	}
	// Another customer in the same tenant does not get it.
	resp = e.do(t, httptest.NewRequest("GET", "/portal/new-order", nil), e.otherToken)
	if b := readBody(t, resp); strings.Contains(b, "1 Main St") {
		t.Error("other customer's form was prefilled with someone else's address")
	}

	// Admin fragment: by customer id; blank for 0.
	resp = e.do(t, httptest.NewRequest("GET", fmt.Sprintf("/admin/new-order/ship-to?customer_id=%d", e.customerID), nil), e.staffToken)
	if b := readBody(t, resp); !strings.Contains(b, "1 Main St") {
		t.Error("admin ship-to fragment not prefilled")
	}
	resp = e.do(t, httptest.NewRequest("GET", "/admin/new-order/ship-to?customer_id=0", nil), e.staffToken)
	if b := readBody(t, resp); strings.Contains(b, "1 Main St") {
		t.Error("admin fragment prefilled for a one-off customer")
	}
	if r := e.do(t, httptest.NewRequest("GET", "/admin/new-order/ship-to?customer_id=1", nil), e.clientToken); r.StatusCode != http.StatusForbidden {
		t.Errorf("client hit admin ship-to fragment: %d", r.StatusCode)
	}
}

func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(b)
}
