// Package mockpsp is an in-memory reference implementation of the API Pix,
// for local development, demos and conformance testing of the pixapi
// client. It is not a PSP: nothing is settled, nothing is persisted, and
// behavior that the specification leaves to the PSP is implemented in the
// simplest reasonable way and documented as such.
//
// It implements the OAuth2 client_credentials token endpoint, optional
// mutual TLS with certificate-bound tokens, the cob, cobv, lotecobv, pix
// (with devoluções), webhook and loc endpoints, and a control endpoint,
// POST /simulate/pay/{txid}, which settles a charge as if a payer had paid
// it and notifies the merchant's webhook over mTLS, with retries.
package mockpsp

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/victorcondackdev/brpay/pix"
	"github.com/victorcondackdev/pix-cobranca-go/pixapi"
)

// Scopes supported by the token endpoint. The names follow the API Pix
// specification.
var Scopes = []string{
	"cob.write", "cob.read",
	"cobv.write", "cobv.read",
	"lotecobv.write", "lotecobv.read",
	"pix.write", "pix.read",
	"webhook.write", "webhook.read",
	"payloadlocation.write", "payloadlocation.read",
}

// Config configures a Server. The zero value is usable for tests over
// plain HTTP; New fills in defaults.
type Config struct {
	// PublicURL is the externally visible URL of the server; its host is
	// used in payload locations. Default "https://localhost:8443".
	PublicURL string
	// BasePath is the prefix of the API routes. Default "/api/v2".
	BasePath string
	// Clients maps OAuth2 client IDs to secrets. Default: one client,
	// "dev-client" / "dev-secret".
	Clients map[string]string
	// TokenTTL is the lifetime of access tokens. Default one hour.
	TokenTTL time.Duration
	// RequireMTLS rejects API calls made without a verified client
	// certificate, and binds tokens to the certificate that obtained
	// them (RFC 8705). Enable it when serving over TLS with
	// tls.VerifyClientCertIfGiven or RequireAndVerifyClientCert.
	RequireMTLS bool
	// Keys restricts the Pix keys charges may use. Empty accepts any
	// valid key.
	Keys []string
	// MerchantName and MerchantCity go into the BR Codes. Defaults
	// "MOCK PSP MERCHANT" and "RIO DE JANEIRO".
	MerchantName, MerchantCity string
	// Recebedor is echoed in charges with due date.
	Recebedor pixapi.Recebedor
	// PayerISPB is the ISPB used in simulated EndToEndIds. Default
	// "00000000".
	PayerISPB string
	// ISPB is the receiving PSP's ISPB, used in refund ids. Default
	// "99999999".
	ISPB string
	// WebhookClient performs webhook calls; give it the PSP's client
	// certificate for mTLS. Default http.DefaultClient.
	WebhookClient *http.Client
	// WebhookAttempts and WebhookBackoff control redelivery. Defaults 5
	// and 500ms (doubling).
	WebhookAttempts int
	WebhookBackoff  time.Duration
	// AllowHTTPWebhooks accepts http:// webhook URLs (the specification
	// requires https). For tests only.
	AllowHTTPWebhooks bool
	// RefundSettleDelay is how long a devolução stays EM_PROCESSAMENTO.
	// Zero settles it before PUT returns.
	RefundSettleDelay time.Duration
	// Now is the clock. Default time.Now.
	Now func() time.Time
	// Logger receives operational logs. Default: discard.
	Logger *slog.Logger
}

type tokenInfo struct {
	clientID string
	scopes   map[string]bool
	expiry   time.Time
	certHash string
}

// Delivery is the record of one webhook delivery attempt.
type Delivery struct {
	URL        string
	EndToEndID string
	Attempt    int
	Status     int
	Err        string
	At         time.Time
}

type outgoing struct {
	url  string
	e2e  string
	body []byte
}

// Server is a mock PSP. Create it with New and serve Handler.
type Server struct {
	cfg  Config
	host string
	keys map[string]bool

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu         sync.Mutex
	tokens     map[string]tokenInfo
	tokenCalls int
	cobs       map[string][]pixapi.Cob // txid -> revisions
	cobvs      map[string][]pixapi.CobV
	locs       map[int64]*pixapi.Loc
	nextLoc    int64
	pix        map[string]*pixapi.Pix
	webhooks   map[string]*pixapi.Webhook
	lotes      map[int64]*pixapi.LoteCobV
	deliveries []Delivery
	outbox     []outgoing
}

// New returns a Server.
func New(cfg Config) (*Server, error) {
	if cfg.PublicURL == "" {
		cfg.PublicURL = "https://localhost:8443"
	}
	u, err := url.Parse(cfg.PublicURL)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("mockpsp: invalid PublicURL %q", cfg.PublicURL)
	}
	if cfg.BasePath == "" {
		cfg.BasePath = "/api/v2"
	}
	cfg.BasePath = "/" + strings.Trim(cfg.BasePath, "/")
	if cfg.BasePath == "/" {
		cfg.BasePath = ""
	}
	if cfg.Clients == nil {
		cfg.Clients = map[string]string{"dev-client": "dev-secret"}
	}
	if cfg.TokenTTL <= 0 {
		cfg.TokenTTL = time.Hour
	}
	if cfg.MerchantName == "" {
		cfg.MerchantName = "MOCK PSP MERCHANT"
	}
	if cfg.MerchantCity == "" {
		cfg.MerchantCity = "RIO DE JANEIRO"
	}
	if cfg.PayerISPB == "" {
		cfg.PayerISPB = "00000000"
	}
	if cfg.ISPB == "" {
		cfg.ISPB = "99999999"
	}
	if cfg.WebhookClient == nil {
		cfg.WebhookClient = http.DefaultClient
	}
	if cfg.WebhookAttempts <= 0 {
		cfg.WebhookAttempts = 5
	}
	if cfg.WebhookBackoff <= 0 {
		cfg.WebhookBackoff = 500 * time.Millisecond
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.NewTextHandler(discard{}, nil))
	}
	s := &Server{
		cfg:      cfg,
		host:     u.Host,
		keys:     map[string]bool{},
		tokens:   map[string]tokenInfo{},
		cobs:     map[string][]pixapi.Cob{},
		cobvs:    map[string][]pixapi.CobV{},
		locs:     map[int64]*pixapi.Loc{},
		pix:      map[string]*pixapi.Pix{},
		webhooks: map[string]*pixapi.Webhook{},
		lotes:    map[int64]*pixapi.LoteCobV{},
	}
	for _, k := range cfg.Keys {
		n, err := normalizeKey(k)
		if err != nil {
			return nil, fmt.Errorf("mockpsp: key %q: %w", k, err)
		}
		s.keys[n] = true
	}
	probe := &pixapi.Loc{Location: s.host + "/qr/v2/probe"}
	if _, err := pix.NewDynamic(pix.DynamicOptions{URL: probe.Location, MerchantName: cfg.MerchantName, MerchantCity: cfg.MerchantCity}); err != nil {
		return nil, fmt.Errorf("mockpsp: merchant name or city: %w", err)
	}
	s.ctx, s.cancel = context.WithCancel(context.Background())
	return s, nil
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }

// Close stops pending webhook deliveries and waits for them to return.
func (s *Server) Close() {
	s.cancel()
	s.wg.Wait()
}

// WaitDeliveries blocks until every webhook delivery started so far has
// finished (succeeded or exhausted its attempts).
func (s *Server) WaitDeliveries() { s.wg.Wait() }

// Deliveries returns a copy of the webhook delivery log.
func (s *Server) Deliveries() []Delivery {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Delivery(nil), s.deliveries...)
}

// TokenRequests returns how many tokens were issued.
func (s *Server) TokenRequests() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tokenCalls
}

// BasePath returns the prefix of the API routes.
func (s *Server) BasePath() string { return s.cfg.BasePath }

func (s *Server) now() time.Time { return s.cfg.Now().UTC().Truncate(time.Millisecond) }

// Handler returns the HTTP handler serving every route.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	b := s.cfg.BasePath
	api := func(pattern, scope string, h func(*http.Request) (int, any)) {
		method, path, _ := strings.Cut(pattern, " ")
		mux.Handle(method+" "+b+path, s.authorize(scope, h))
	}
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	mux.HandleFunc("POST /oauth/token", s.handleToken)

	api("PUT /cob/{txid}", "cob.write", s.putCob)
	api("POST /cob", "cob.write", s.postCob)
	api("PATCH /cob/{txid}", "cob.write", s.patchCob)
	api("GET /cob/{txid}", "cob.read", s.getCob)
	api("GET /cob", "cob.read", s.listCob)

	api("PUT /cobv/{txid}", "cobv.write", s.putCobV)
	api("PATCH /cobv/{txid}", "cobv.write", s.patchCobV)
	api("GET /cobv/{txid}", "cobv.read", s.getCobV)
	api("GET /cobv", "cobv.read", s.listCobV)

	api("PUT /lotecobv/{id}", "lotecobv.write", s.putLote)
	api("PATCH /lotecobv/{id}", "lotecobv.write", s.patchLote)
	api("GET /lotecobv/{id}", "lotecobv.read", s.getLote)
	api("GET /lotecobv", "lotecobv.read", s.listLote)

	api("GET /pix/{e2eid}", "pix.read", s.getPix)
	api("GET /pix", "pix.read", s.listPix)
	api("PUT /pix/{e2eid}/devolucao/{id}", "pix.write", s.putDevolucao)
	api("GET /pix/{e2eid}/devolucao/{id}", "pix.read", s.getDevolucao)

	api("PUT /webhook/{chave}", "webhook.write", s.putWebhook)
	api("GET /webhook/{chave}", "webhook.read", s.getWebhook)
	api("DELETE /webhook/{chave}", "webhook.write", s.deleteWebhook)
	api("GET /webhook", "webhook.read", s.listWebhooks)

	api("POST /loc", "payloadlocation.write", s.postLoc)
	api("GET /loc/{id}", "payloadlocation.read", s.getLoc)
	api("DELETE /loc/{id}/txid", "payloadlocation.write", s.unlinkLoc)
	api("GET /loc", "payloadlocation.read", s.listLoc)

	mux.Handle("POST /simulate/pay/{txid}", s.control(s.simulatePay))
	mux.Handle("POST /simulate/remove/{txid}", s.control(s.simulateRemove))
	return mux
}

// ---- plumbing ------------------------------------------------------------

func problem(status int, typ, title, detail string, v ...pixapi.Violacao) *pixapi.Problem {
	return &pixapi.Problem{Type: typ, Title: title, Status: status, Detail: detail, Violacoes: v}
}

func invalidRequest(detail string, v ...pixapi.Violacao) *pixapi.Problem {
	return problem(http.StatusBadRequest, pixapi.TypeRequisicaoInvalida, "Requisição inválida.", detail, v...)
}

func validationProblem(typ, title string, err error) *pixapi.Problem {
	var ve *pixapi.ValidationError
	if errors.As(err, &ve) {
		return problem(http.StatusBadRequest, typ, title, "A requisição que busca alterar ou criar uma cobrança não respeita o schema ou está semanticamente errada.", ve.Violacoes...)
	}
	return problem(http.StatusBadRequest, typ, title, err.Error())
}

// write serializes v; *pixapi.Problem values become problem documents.
func (s *Server) write(w http.ResponseWriter, r *http.Request, status int, b []byte, isProblem bool) {
	if isProblem {
		w.Header().Set("Content-Type", "application/problem+json")
	} else if b != nil {
		w.Header().Set("Content-Type", "application/json")
	}
	if id := r.Header.Get("X-Request-Id"); id != "" {
		w.Header().Set("X-Request-Id", id)
	}
	w.WriteHeader(status)
	if b != nil {
		_, _ = w.Write(b)
	}
}

// run executes h with the state lock held, serializes its result under
// the lock (so no other request mutates it meanwhile), then dispatches the
// webhook deliveries queued by h.
func (s *Server) run(w http.ResponseWriter, r *http.Request, h func(*http.Request) (int, any)) {
	s.mu.Lock()
	status, v := h(r)
	var b []byte
	var err error
	p, isProblem := v.(*pixapi.Problem)
	if isProblem && p.CorrelationID == "" {
		p.CorrelationID = randomHex(8)
	}
	if v != nil {
		b, err = json.Marshal(v)
	}
	out := s.outbox
	s.outbox = nil
	s.mu.Unlock()
	if err != nil {
		s.cfg.Logger.Error("encode response", "error", err)
		status, isProblem = http.StatusInternalServerError, true
		b, _ = json.Marshal(problem(status, pixapi.TypeErroInternoDoServidor, "Erro interno do servidor.", ""))
	}
	// Register deliveries before answering, so that a client calling
	// WaitDeliveries after the response observes them.
	for _, o := range out {
		s.deliver(o)
	}
	s.write(w, r, status, b, isProblem)
}

func (s *Server) control(h func(*http.Request) (int, any)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { s.run(w, r, h) })
}

func certHash(r *http.Request) string {
	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		return ""
	}
	sum := sha256.Sum256(r.TLS.PeerCertificates[0].Raw)
	return hex.EncodeToString(sum[:])
}

func (s *Server) authorize(scope string, h func(*http.Request) (int, any)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		deny := func(status int, typ, title, detail string) {
			if status == http.StatusUnauthorized {
				w.Header().Set("WWW-Authenticate", `Bearer realm="mockpsp"`)
			}
			b, _ := json.Marshal(problem(status, typ, title, detail))
			s.write(w, r, status, b, true)
		}
		ch := certHash(r)
		if s.cfg.RequireMTLS && ch == "" {
			deny(http.StatusUnauthorized, pixapi.TypeAcessoNegado, "Acesso negado.", "Certificado de cliente ausente.")
			return
		}
		tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || tok == "" {
			deny(http.StatusUnauthorized, pixapi.TypeAcessoNegado, "Acesso negado.", "Token de acesso ausente.")
			return
		}
		s.mu.Lock()
		info, found := s.tokens[tok]
		if found && !s.cfg.Now().Before(info.expiry) {
			delete(s.tokens, tok)
			found = false
		}
		s.mu.Unlock()
		switch {
		case !found:
			deny(http.StatusUnauthorized, pixapi.TypeAcessoNegado, "Acesso negado.", "Token de acesso inválido ou expirado.")
			return
		case info.certHash != "" && info.certHash != ch:
			deny(http.StatusUnauthorized, pixapi.TypeAcessoNegado, "Acesso negado.", "Token emitido para outro certificado.")
			return
		case !info.scopes[scope]:
			deny(http.StatusForbidden, pixapi.TypeAcessoNegado, "Acesso negado.", "Escopo "+scope+" ausente.")
			return
		}
		s.run(w, r, h)
	})
}

func (s *Server) handleToken(w http.ResponseWriter, r *http.Request) {
	oauthErr := func(status int, code, desc string) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		if status == http.StatusUnauthorized {
			w.Header().Set("WWW-Authenticate", `Basic realm="mockpsp"`)
		}
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": code, "error_description": desc})
	}
	if s.cfg.RequireMTLS && certHash(r) == "" {
		oauthErr(http.StatusUnauthorized, "invalid_client", "client certificate required")
		return
	}
	if err := r.ParseForm(); err != nil {
		oauthErr(http.StatusBadRequest, "invalid_request", "malformed form")
		return
	}
	id, secret, ok := r.BasicAuth()
	if ok {
		id, _ = url.QueryUnescape(id)
		secret, _ = url.QueryUnescape(secret)
	} else {
		id, secret = r.PostForm.Get("client_id"), r.PostForm.Get("client_secret")
	}
	if r.PostForm.Get("grant_type") != "client_credentials" {
		oauthErr(http.StatusBadRequest, "unsupported_grant_type", "only client_credentials is supported")
		return
	}
	want, known := s.cfg.Clients[id]
	if !known || !constantTimeEqual(want, secret) {
		oauthErr(http.StatusUnauthorized, "invalid_client", "invalid client credentials")
		return
	}
	granted := map[string]bool{}
	var names []string
	if req := strings.Fields(r.PostForm.Get("scope")); len(req) > 0 {
		for _, sc := range req {
			if !contains(Scopes, sc) {
				oauthErr(http.StatusBadRequest, "invalid_scope", "unknown scope "+sc)
				return
			}
			granted[sc] = true
			names = append(names, sc)
		}
	} else {
		for _, sc := range Scopes {
			granted[sc] = true
		}
		names = Scopes
	}
	tok := randomHex(32)
	s.mu.Lock()
	s.tokens[tok] = tokenInfo{clientID: id, scopes: granted, expiry: s.cfg.Now().Add(s.cfg.TokenTTL), certHash: certHash(r)}
	s.tokenCalls++
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"access_token": tok,
		"token_type":   "Bearer",
		"expires_in":   int64(s.cfg.TokenTTL / time.Second),
		"scope":        strings.Join(names, " "),
	})
}

func constantTimeEqual(a, b string) bool {
	ha, hb := sha256.Sum256([]byte(a)), sha256.Sum256([]byte(b))
	var d byte
	for i := range ha {
		d |= ha[i] ^ hb[i]
	}
	return d == 0
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

const alnum = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

func randomTxID(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	for i := range b {
		b[i] = alnum[int(b[i])%len(alnum)]
	}
	return string(b)
}

func decode(r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	if err := dec.Decode(v); err != nil {
		return err
	}
	return nil
}

// timeRange parses inicio and fim.
func timeRange(r *http.Request, required bool) (time.Time, time.Time, *pixapi.Problem) {
	q := r.URL.Query()
	var out [2]time.Time
	var viol []pixapi.Violacao
	for i, name := range []string{"inicio", "fim"} {
		v := q.Get(name)
		if v == "" {
			if required {
				viol = append(viol, pixapi.Violacao{Propriedade: name, Razao: "obrigatório"})
			}
			continue
		}
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			viol = append(viol, pixapi.Violacao{Propriedade: name, Razao: "deve estar no formato RFC 3339", Valor: v})
			continue
		}
		out[i] = t
	}
	if len(viol) > 0 {
		return out[0], out[1], invalidRequest("Parâmetros de consulta inválidos.", viol...)
	}
	if !out[0].IsZero() && !out[1].IsZero() && out[1].Before(out[0]) {
		return out[0], out[1], invalidRequest("fim anterior a inicio.", pixapi.Violacao{Propriedade: "fim", Razao: "anterior a inicio"})
	}
	return out[0], out[1], nil
}

func inRange(t, inicio, fim time.Time) bool {
	return (inicio.IsZero() || !t.Before(inicio)) && (fim.IsZero() || !t.After(fim))
}

// paginate slices items according to the paginacao.* query parameters.
func paginate[T any](r *http.Request, items []T, inicio, fim time.Time) ([]T, pixapi.Parametros, *pixapi.Problem) {
	q := r.URL.Query()
	page, size := 0, 100
	var err error
	if v := q.Get("paginacao.paginaAtual"); v != "" {
		if page, err = strconv.Atoi(v); err != nil || page < 0 {
			return nil, pixapi.Parametros{}, invalidRequest("paginacao.paginaAtual inválido.")
		}
	}
	if v := q.Get("paginacao.itensPorPagina"); v != "" {
		if size, err = strconv.Atoi(v); err != nil || size < 1 || size > 1000 {
			return nil, pixapi.Parametros{}, invalidRequest("paginacao.itensPorPagina deve estar entre 1 e 1000.")
		}
	}
	total := len(items)
	pages := (total + size - 1) / size
	lo := page * size
	if lo > total {
		lo = total
	}
	hi := lo + size
	if hi > total {
		hi = total
	}
	return items[lo:hi], pixapi.Parametros{
		Inicio: inicio, Fim: fim,
		Paginacao: pixapi.Paginacao{PaginaAtual: page, ItensPorPagina: size, QuantidadeDePaginas: pages, QuantidadeTotalDeItens: total},
	}, nil
}

func sortByTime[T any](items []T, at func(T) time.Time, id func(T) string) {
	sort.Slice(items, func(i, j int) bool {
		ti, tj := at(items[i]), at(items[j])
		if !ti.Equal(tj) {
			return ti.Before(tj)
		}
		return id(items[i]) < id(items[j])
	})
}
